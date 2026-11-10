package sqlite

// Real-SQLite visual KNN benchmarks. They complement scale_benchmark_test.go:
// the legacy corpus measures text retrieval, this one measures the exact
// cosine path over active visual rows with the canonical-shot universe applied
// inside the query, which is what Search v2's modality channels run.

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

const (
	scaleVisualDimension = 64
	scaleVisualSpaceID   = "space-scale-visual"
	scaleVisualConfig    = "cfg-scale-visual"
)

type visualCorpus struct {
	repo     *Repository
	shotIDs  []string
	universe map[string]bool
	query    []float32
}

func buildVisualCorpus(b testing.TB, shots int) *visualCorpus {
	b.Helper()
	if testing.Short() {
		b.Skip("scale corpus is too large for -short")
	}
	dir := b.TempDir()
	repo, err := Open(filepath.Join(dir, "visual-scale.db"))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	const perAsset = 1000
	shotIDs := make([]string, 0, shots)
	for start := 0; start < shots; start += perAsset {
		end := start + perAsset
		if end > shots {
			end = shots
		}
		assetID := fmt.Sprintf("scale-asset-%d", start/perAsset)
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, assetID+"-fp", now, now); err != nil {
			b.Fatal(err)
		}
		batch := make([]domain.AssetShot, 0, end-start)
		for i := start; i < end; i++ {
			id := fmt.Sprintf("scale-shot-%06d", i)
			slot := i - start
			batch = append(batch, domain.AssetShot{ID: id, StartMS: int64(slot) * 1000, EndMS: int64(slot+1) * 1000, Ordinal: slot, Description: "scale shot"})
			shotIDs = append(shotIDs, id)
		}
		if err := repo.ReplaceAssetShots(ctx, assetID, "", batch, "", ""); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO embedding_spaces(id,modality,provider,model_id,model_revision,dimension,normalization,preprocessing_json,runtime_digest,created_at)
		VALUES(?, 'visual','test','scale/model','rev-1',?,'l2','{}','test',?)`,
		scaleVisualSpaceID, scaleVisualDimension, now); err != nil {
		b.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at,model_id,model_revision,backend)
		VALUES('scale-generation','scale-asset-0','visual_embedding','in','`+scaleVisualConfig+`',?,'shots','committed','[]','[]',?,?, 'scale/model','rev-1','cpu')`,
		scaleVisualSpaceID, now, now); err != nil {
		b.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at)
		VALUES('scale-asset-0','visual_embedding',?, 'scale-generation', ?)`, scaleVisualConfig, now); err != nil {
		b.Fatal(err)
	}
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO visual_embeddings(generation_id,asset_id,shot_id,space_id,sample_index,timestamp_ms,strategy,source_hash,vector,created_at)
		VALUES('scale-generation',?,?,?, -1,0,'shot_frame_pooled','',?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i, shotID := range shotIDs {
		vector := scaleVisualVector(i)
		blob, err := encodeQueryVector(vector, scaleVisualDimension)
		if err != nil {
			b.Fatal(err)
		}
		assetID := fmt.Sprintf("scale-asset-%d", i/perAsset)
		if _, err := insert.ExecContext(ctx, assetID, shotID, scaleVisualSpaceID, blob, now); err != nil {
			b.Fatal(err)
		}
	}
	insert.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	corpus := &visualCorpus{repo: repo, shotIDs: shotIDs, universe: make(map[string]bool, len(shotIDs)), query: scaleVisualQuery()}
	for _, id := range shotIDs {
		corpus.universe[id] = true
	}
	b.Cleanup(func() { repo.Close() })
	return corpus
}

// scaleVisualVector is a deterministic unit vector: it leans toward the query
// direction for the first rows and rotates away as the index grows, so the
// benchmark exercises the real distance comparison instead of a constant.
func scaleVisualVector(index int) []float32 {
	out := make([]float32, scaleVisualDimension)
	for dim := range out {
		angle := float64(index%97+1) * 0.013 * float64(dim+1)
		out[dim] = float32(math.Sin(angle))
	}
	normalizeScaleVector(out)
	return out
}

func scaleVisualQuery() []float32 {
	out := make([]float32, scaleVisualDimension)
	for dim := range out {
		angle := 0.013 * float64(dim+1)
		out[dim] = float32(math.Sin(angle))
	}
	normalizeScaleVector(out)
	return out
}

func normalizeScaleVector(vector []float32) {
	var norm float64
	for _, value := range vector {
		norm += float64(value) * float64(value)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return
	}
	for i := range vector {
		vector[i] = float32(float64(vector[i]) / norm)
	}
}

func benchmarkVisualKNN(b *testing.B, shots int, filtered bool) {
	corpus := buildVisualCorpus(b, shots)
	ctx := context.Background()
	universe := corpus.universe
	if !filtered {
		universe = nil
	}
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		if _, err := corpus.repo.VisualWindowMatches(ctx, scaleVisualSpaceID, corpus.query, universe, 20); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(corpus.shotIDs)), "shots")
}

func BenchmarkSQLiteScaleVisual1k(b *testing.B)           { benchmarkVisualKNN(b, 1_000, false) }
func BenchmarkSQLiteScaleVisual10k(b *testing.B)          { benchmarkVisualKNN(b, 10_000, false) }
func BenchmarkSQLiteScaleVisual100k(b *testing.B)         { benchmarkVisualKNN(b, 100_000, false) }
func BenchmarkSQLiteScaleVisual10kFiltered(b *testing.B)  { benchmarkVisualKNN(b, 10_000, true) }
func BenchmarkSQLiteScaleVisual100kFiltered(b *testing.B) { benchmarkVisualKNN(b, 100_000, true) }

// TestVisualKNNMatchesBruteForce pins exact-cosine agreement: the SQL path and
// a Go full scan must return the same neighbours in the same order.
func TestVisualKNNMatchesBruteForce(t *testing.T) {
	corpus := buildVisualCorpus(t, 500)
	ctx := context.Background()
	got, err := corpus.repo.VisualWindowMatches(ctx, scaleVisualSpaceID, corpus.query, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("got %d rows, want 20", len(got))
	}
	type scored struct {
		id    string
		score float64
	}
	expected := make([]scored, 0, len(corpus.shotIDs))
	for i, id := range corpus.shotIDs {
		vector := scaleVisualVector(i)
		var dot, na, nb float64
		for dim := range vector {
			a, b := float64(corpus.query[dim]), float64(vector[dim])
			dot += a * b
			na += a * a
			nb += b * b
		}
		expected = append(expected, scored{id: id, score: dot / (math.Sqrt(na) * math.Sqrt(nb))})
	}
	for i := 0; i < len(expected); i++ {
		for j := i + 1; j < len(expected); j++ {
			if expected[j].score > expected[i].score || (expected[j].score == expected[i].score && expected[j].id < expected[i].id) {
				expected[i], expected[j] = expected[j], expected[i]
			}
		}
	}
	for i := 0; i < 20; i++ {
		if got[i].ShotID != expected[i].id {
			t.Fatalf("rank %d: sqlite=%s brute-force=%s (scores %v vs %v)", i, got[i].ShotID, expected[i].id, got[i].Score, expected[i].score)
		}
		if math.Abs(got[i].Score-expected[i].score) > 1e-5 {
			t.Fatalf("rank %d score mismatch: %v vs %v", i, got[i].Score, expected[i].score)
		}
	}
}
