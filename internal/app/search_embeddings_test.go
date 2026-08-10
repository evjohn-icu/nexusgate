package app

import (
	"context"
	"hash/fnv"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/search"
)

// testEmbedder is a deterministic embedding provider for app-level tests: it
// implements providers.Embedder (what the Service holds) and its vector space
// is token-hash based, so changed text produces changed vectors.
type testEmbedder struct{}

func (testEmbedder) Name() string  { return "test-embed" }
func (testEmbedder) Model() string { return "test-embed-v1" }

func (testEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = testEmbedVector(text)
	}
	return out, nil
}

func testEmbedVector(text string) []float64 {
	vector := make([]float64, 128)
	seen := map[string]bool{}
	for _, word := range strings.Fields(text) {
		if seen[word] {
			continue
		}
		seen[word] = true
		h := fnv.New64a()
		_, _ = h.Write([]byte(word))
		sum := h.Sum64()
		if sum&1 == 0 {
			vector[int(sum%128)] += 1
		} else {
			vector[int(sum%128)] -= 1
		}
	}
	var norm float64
	for _, v := range vector {
		norm += v * v
	}
	if norm == 0 {
		return vector
	}
	scale := 1 / math.Sqrt(norm)
	for i := range vector {
		vector[i] *= scale
	}
	return vector
}

// openEmbeddingTestRepo opens a migrated repo with one asset and two shots.
func openEmbeddingTestRepo(t *testing.T) (*sqlite.Repository, []domain.ShotSearchResult) {
	t.Helper()
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "embed-hook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-1','fp',100,'discovered','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	shots := []domain.AssetShot{
		{ID: "shot-1", AssetID: "asset-1", Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "red car crossing"},
		{ID: "shot-2", AssetID: "asset-1", Ordinal: 1, StartMS: 5000, EndMS: 10_000, Description: "empty parking lot"},
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-1", "", shots); err != nil {
		t.Fatal(err)
	}
	committed, err := repo.ListAssetShots(ctx, "asset-1")
	if err != nil {
		t.Fatal(err)
	}
	results := make([]domain.ShotSearchResult, 0, len(committed))
	for _, s := range committed {
		results = append(results, domain.ShotSearchResult{AssetShot: s})
	}
	return repo, results
}

func TestEnsureShotTextEmbeddingsWritesChangedShots(t *testing.T) {
	repo, committed := openEmbeddingTestRepo(t)
	svc := &Service{repo: repo, embedder: testEmbedder{}}
	ctx := context.Background()

	svc.ensureShotTextEmbeddings(ctx, "asset-1")

	rows, err := repo.ListShotTextEmbeddings(ctx, "test-embed-v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("both shots must be embedded on first pass, got %d", len(rows))
	}
	for _, row := range rows {
		if len(row.Vector) != 128 {
			t.Fatalf("vector dimension wrong for %s: %d", row.Shot.ID, len(row.Vector))
		}
		if row.SourceTextHash == "" {
			t.Fatalf("source hash must be persisted for %s", row.Shot.ID)
		}
	}

	// Second pass: nothing changed -> no new embeddings, no errors.
	svc.ensureShotTextEmbeddings(ctx, "asset-1")
	after, err := repo.ListShotTextEmbeddings(ctx, "test-embed-v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("unchanged shots must not be re-embedded, got %d", len(after))
	}

	// A changed shot (reanalysis wrote a new description) gets re-embedded.
	changed := committed[0].AssetShot
	changed.Description = "blue van crossing"
	if err := repo.ReplaceAssetShots(ctx, "asset-1", "", []domain.AssetShot{changed, committed[1].AssetShot}); err != nil {
		t.Fatal(err)
	}
	svc.ensureShotTextEmbeddings(ctx, "asset-1")
	final, err := repo.ListShotTextEmbeddings(ctx, "test-embed-v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 2 {
		t.Fatalf("rebuild after change must still cover both shots, got %d", len(final))
	}
	var changedRow *search.ShotEmbeddingRow
	for i := range final {
		if final[i].Shot.ID == "shot-1" {
			changedRow = &final[i]
		}
	}
	if changedRow == nil {
		t.Fatal("shot-1 row missing after re-embed")
	}
	wantHash := search.ShotTextSourceHash(search.ShotSearchDocument{ShotID: "shot-1", Description: "blue van crossing"})
	if changedRow.SourceTextHash != wantHash {
		t.Fatalf("hash must track the new text: got %q want %q", changedRow.SourceTextHash, wantHash)
	}
}

// TestEnsureShotTextEmbeddingsUnconfiguredIsNoOp: the adapter treats a
// missing embedding capability as "no vectors" — the hook must not error and
// must not write rows.
func TestEnsureShotTextEmbeddingsUnconfiguredIsNoOp(t *testing.T) {
	repo, _ := openEmbeddingTestRepo(t)
	svc := &Service{repo: repo, embedder: nil}
	svc.ensureShotTextEmbeddings(context.Background(), "asset-1")
	rows, err := repo.ListShotTextEmbeddings(context.Background(), "whatever")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("nil embedder must write nothing, got %d rows", len(rows))
	}
}

func TestRebuildShotTextEmbeddingsIncremental(t *testing.T) {
	repo, committed := openEmbeddingTestRepo(t)
	svc := &Service{repo: repo, embedder: testEmbedder{}}
	ctx := context.Background()

	rebuilt, err := svc.RebuildShotTextEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != 2 {
		t.Fatalf("full rebuild must embed both shots, got %d", rebuilt)
	}

	// Second rebuild: nothing changed.
	rebuilt, err = svc.RebuildShotTextEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != 0 {
		t.Fatalf("no changes must mean no rebuild, got %d", rebuilt)
	}

	// Model switch: the per-shot rows are overwritten under the new model
	// (one representation per shot, like shot_semantic_vectors), and the
	// rebuild re-embeds everything — canonical analysis untouched.
	svc2 := &Service{repo: repo, embedder: switchedModelEmbedder{}}
	rebuilt, err = svc2.RebuildShotTextEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt != 2 {
		t.Fatalf("new model must rebuild everything, got %d", rebuilt)
	}
	oldRows, err := repo.ListShotTextEmbeddings(ctx, "test-embed-v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRows) != 0 {
		t.Fatalf("old model rows must be replaced by the new model, got %d", len(oldRows))
	}
	newRows, err := repo.ListShotTextEmbeddings(ctx, "test-embed-v2")
	if err != nil {
		t.Fatal(err)
	}
	if len(newRows) != 2 {
		t.Fatalf("new model must have both shots, got %d", len(newRows))
	}
	_ = committed
}

type zeroVectorEmbedder struct{}

func (zeroVectorEmbedder) Name() string  { return "zero" }
func (zeroVectorEmbedder) Model() string { return "zero-v1" }
func (zeroVectorEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	return make([][]float64, len(texts)), nil
}

func TestRebuildShotTextEmbeddingsReportsPersistedCount(t *testing.T) {
	repo, _ := openEmbeddingTestRepo(t)
	svc := &Service{repo: repo, embedder: testEmbedder{}}
	count, err := svc.RebuildShotTextEmbeddings(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("rebuild count = %d, err=%v; want persisted count 2", count, err)
	}
}

func TestRebuildShotTextEmbeddingsZeroVectorIsNoOp(t *testing.T) {
	repo, _ := openEmbeddingTestRepo(t)
	svc := &Service{repo: repo, embedder: zeroVectorEmbedder{}}
	count, err := svc.RebuildShotTextEmbeddings(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("zero-vector rebuild = %d, err=%v; want 0, nil", count, err)
	}
	rows, err := repo.ListShotTextEmbeddings(context.Background(), "zero-v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("zero-vector provider must persist nothing, got %d", len(rows))
	}
}

type switchedModelEmbedder struct{ testEmbedder }

func (switchedModelEmbedder) Model() string { return "test-embed-v2" }
