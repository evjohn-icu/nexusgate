package sqlite

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	"github.com/evjohn-icu/timingdex/internal/providers/embedding"
	"github.com/evjohn-icu/timingdex/internal/search"
)

func TestUpsertShotTextEmbeddingsRollsBackInvalidBatch(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows := []search.ShotEmbeddingRow{
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "missing-shot"}}, Model: "m", Vector: []float32{1, 0}},
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: ""}}, Model: "m", Vector: []float32{1, 0}},
	}
	if err := repo.UpsertShotTextEmbeddings(ctx, rows); err == nil {
		t.Fatal("invalid batch must fail")
	}
	var count int
	if err := repo.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM shot_text_embeddings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed batch must roll back, got %d rows", count)
	}
}

func TestDecodeVectorBlobRejectsMalformedData(t *testing.T) {
	for _, blob := range [][]byte{{1}, func() []byte {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, math.Float32bits(float32(math.NaN())))
		return b
	}(), func() []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, math.Float32bits(0)); return b }()} {
		if _, err := decodeVectorBlob(blob); err == nil {
			t.Fatalf("blob %v must be rejected", blob)
		}
	}
}

func TestListShotTextEmbeddingsPropagatesMalformedBlob(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "malformed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('a','fp',1,'discovered','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "a", "", []domain.AssetShot{{ID: "s", AssetID: "a", EndMS: 1, Description: "test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO shot_text_embeddings(shot_id,model,vector_blob,source_text_hash,created_at) VALUES('s','m',X'01','h','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ListShotTextEmbeddings(ctx, "m"); err == nil {
		t.Fatal("malformed blob must propagate")
	}
}

func TestUpsertShotTextEmbeddingsRejectsInvalidVector(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, vector := range [][]float32{nil, {0, 0}, {float32(math.NaN()), 1}, {float32(math.Inf(1)), 1}} {
		err := repo.UpsertShotTextEmbeddings(context.Background(), []search.ShotEmbeddingRow{{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s"}}, Model: "m", Vector: vector}})
		if err == nil {
			t.Fatalf("vector %v must be rejected", vector)
		}
	}
}

// TestTextEmbeddingRoundtrip exercises the REAL embedding provider adapter
// (OpenAI-compatible protocol) against an httptest endpoint, then the full
// storage + retrieval path: embed -> upsert -> list -> cosine retriever.
// This is the CLAUDE.md-mandated pattern for provider code: httptest
// fixtures, never a real provider call.
func TestTextEmbeddingRoundtrip(t *testing.T) {
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received = append(received, body.Input...)
		if body.Model != "test-embed-model" {
			t.Errorf("model = %q, want test-embed-model", body.Model)
		}
		data := make([]map[string]any, 0, len(body.Input))
		for i, input := range body.Input {
			// Character-frequency histogram: text sharing characters scores
			// higher, so "car" ranks the car shot above the dog shot.
			vector := make([]float64, 26)
			for _, r := range input {
				if r >= 'a' && r <= 'z' {
					vector[int(r-'a')] += 1
				}
			}
			var norm float64
			for _, v := range vector {
				norm += v * v
			}
			if norm > 0 {
				scale := 1 / math.Sqrt(norm)
				for j := range vector {
					vector[j] *= scale
				}
			}
			data = append(data, map[string]any{"index": i, "embedding": vector})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	repo, err := Open(filepath.Join(t.TempDir(), "embed-roundtrip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-1','fp',100,'discovered','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-1", "", []domain.AssetShot{
		{ID: "shot-1", AssetID: "asset-1", Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "red car crossing"},
		{ID: "shot-2", AssetID: "asset-1", Ordinal: 1, StartMS: 5000, EndMS: 10_000, Description: "dog in a park"},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &embedding.Provider{
		Endpoint:  common.Endpoint{BaseURL: server.URL, HTTPClient: server.Client()},
		ModelName: "test-embed-model",
	}

	// Embed the two shots through the real adapter.
	docs, err := repo.AllShotTextDocuments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, len(docs))
	for i, doc := range docs {
		texts[i] = search.ShotTextSource(doc)
	}
	vectors, err := provider.Embed(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]search.ShotEmbeddingRow, 0, len(docs))
	for i, doc := range docs {
		vector := make([]float32, len(vectors[i]))
		for j, v := range vectors[i] {
			vector[j] = float32(v)
		}
		rows = append(rows, search.ShotEmbeddingRow{
			Shot:           domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: doc.ShotID}},
			Model:          provider.Model(),
			Vector:         vector,
			SourceTextHash: search.ShotTextSourceHash(doc),
		})
	}
	if err := repo.UpsertShotTextEmbeddings(ctx, rows); err != nil {
		t.Fatal(err)
	}

	// The query goes through the same adapter; the retriever must rank the
	// car shot first for a car query.
	retriever := search.NewTextEmbeddingRetriever(repo, provider)
	candidates, err := retriever.Retrieve(ctx, search.Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("roundtrip must produce candidates")
	}
	if candidates[0].ShotID != "shot-1" {
		t.Fatalf("car query must rank shot-1 first, got %+v", candidates)
	}
	if len(received) == 0 {
		t.Fatal("provider adapter must have hit the endpoint")
	}
}
