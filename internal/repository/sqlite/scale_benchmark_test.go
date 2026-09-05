package sqlite

// This file is the real-SQLite complement to internal/search/benchmark_test.go.
// The synthetic engine benchmarks are useful for isolating Go-side work, but
// they cannot show the cost of FTS, canonical-row materialization, WAL reads,
// or the text-embedding blob scan. The corpus builder below writes a
// deterministic on-disk library once, before a benchmark timer starts.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/discovery"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

const (
	scaleShotsPerAsset  = 10
	scaleEmbeddingDim   = 256
	scaleEmbeddingModel = "nexusgate-scale-embedding-v1"
)

// scaleCorpus is kept open while the benchmark process runs. Keeping one
// repository per size means all scenarios measure the same seeded database,
// while the seed itself is paid only once per size and never inside b.N.
type scaleCorpus struct {
	repo  *Repository
	path  string
	shots int
}

var scaleCorpora = struct {
	sync.Mutex
	items map[int]*scaleCorpus
}{items: make(map[int]*scaleCorpus)}

// TestSQLiteScaleSanity is the ordinary-CI guard. It intentionally uses only
// the 1k corpus; release jobs opt into larger benchmark sizes explicitly.
func TestSQLiteScaleSanity(t *testing.T) {
	repo := openScaleTestRepo(t, 1_000)
	ctx := context.Background()

	var shots int
	if err := repo.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shots`).Scan(&shots); err != nil {
		t.Fatal(err)
	}
	if shots != 1_000 {
		t.Fatalf("scale corpus has %d shots, want 1000", shots)
	}

	lexical, err := repo.LexicalRankedShots(ctx, "rain city", [5]float64{1, 1, 1, 1, 1}, 20)
	if err != nil {
		t.Fatalf("lexical sanity: %v", err)
	}
	if len(lexical) == 0 {
		t.Fatal("lexical sanity returned no rows")
	}

	service := search.NewService(repo, search.DefaultOptions())
	response, err := service.Search(ctx, search.SearchRequest{
		Query: "car", Mode: "fact", Limit: 5, IncludeEvidence: true, IncludeContext: true,
	})
	if err != nil {
		t.Fatalf("fact/context sanity: %v", err)
	}
	if len(response.Results) == 0 {
		t.Fatal("fact/context sanity returned no rows")
	}
	if response.Results[0].Evidence == nil {
		t.Fatal("fact/context sanity did not return evidence")
	}
	if response.Results[0].Context == nil {
		t.Fatal("fact/context sanity did not return context")
	}

	retriever := search.NewTextEmbeddingRetriever(repo, scaleEmbedder{})
	embedding, err := retriever.Retrieve(ctx, search.Compile("rain city car"), 20)
	if err != nil {
		t.Fatalf("embedding sanity: %v", err)
	}
	if len(embedding) == 0 {
		t.Fatal("embedding sanity returned no rows")
	}
}

// Benchmark names are intentionally explicit so a release job can select one
// scenario without accidentally materializing every large corpus.
func BenchmarkSQLiteScaleLexical1k(b *testing.B) {
	benchSQLiteScale(b, 1_000, false, scaleLexical)
}
func BenchmarkSQLiteScaleLexical1kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 1_000, true, scaleLexical)
}
func BenchmarkSQLiteScaleLexical10k(b *testing.B) {
	benchSQLiteScale(b, 10_000, false, scaleLexical)
}
func BenchmarkSQLiteScaleLexical10kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 10_000, true, scaleLexical)
}
func BenchmarkSQLiteScaleLexical100k(b *testing.B) {
	benchSQLiteScale(b, 100_000, false, scaleLexical)
}
func BenchmarkSQLiteScaleLexical100kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 100_000, true, scaleLexical)
}
func BenchmarkSQLiteScaleLexical500k(b *testing.B) {
	benchSQLiteScale(b, 500_000, false, scaleLexical)
}
func BenchmarkSQLiteScaleLexical500kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 500_000, true, scaleLexical)
}

func BenchmarkSQLiteScaleHybrid1k(b *testing.B) {
	benchSQLiteScale(b, 1_000, false, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid1kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 1_000, true, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid10k(b *testing.B) {
	benchSQLiteScale(b, 10_000, false, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid10kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 10_000, true, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid100k(b *testing.B) {
	benchSQLiteScale(b, 100_000, false, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid100kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 100_000, true, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid500k(b *testing.B) {
	benchSQLiteScale(b, 500_000, false, scaleHybrid)
}
func BenchmarkSQLiteScaleHybrid500kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 500_000, true, scaleHybrid)
}

func BenchmarkSQLiteScaleFactEvidence1k(b *testing.B) {
	benchSQLiteScale(b, 1_000, false, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence1kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 1_000, true, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence10k(b *testing.B) {
	benchSQLiteScale(b, 10_000, false, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence10kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 10_000, true, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence100k(b *testing.B) {
	benchSQLiteScale(b, 100_000, false, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence100kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 100_000, true, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence500k(b *testing.B) {
	benchSQLiteScale(b, 500_000, false, scaleFactEvidence)
}
func BenchmarkSQLiteScaleFactEvidence500kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 500_000, true, scaleFactEvidence)
}

func BenchmarkSQLiteScaleContext1k(b *testing.B) {
	benchSQLiteScale(b, 1_000, false, scaleContext)
}
func BenchmarkSQLiteScaleContext1kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 1_000, true, scaleContext)
}
func BenchmarkSQLiteScaleContext10k(b *testing.B) {
	benchSQLiteScale(b, 10_000, false, scaleContext)
}
func BenchmarkSQLiteScaleContext10kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 10_000, true, scaleContext)
}
func BenchmarkSQLiteScaleContext100k(b *testing.B) {
	benchSQLiteScale(b, 100_000, false, scaleContext)
}
func BenchmarkSQLiteScaleContext100kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 100_000, true, scaleContext)
}
func BenchmarkSQLiteScaleContext500k(b *testing.B) {
	benchSQLiteScale(b, 500_000, false, scaleContext)
}
func BenchmarkSQLiteScaleContext500kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 500_000, true, scaleContext)
}

func BenchmarkSQLiteScaleEmbedding1k(b *testing.B) {
	benchSQLiteScale(b, 1_000, false, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding1kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 1_000, true, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding10k(b *testing.B) {
	benchSQLiteScale(b, 10_000, false, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding10kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 10_000, true, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding100k(b *testing.B) {
	benchSQLiteScale(b, 100_000, false, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding100kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 100_000, true, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding500k(b *testing.B) {
	benchSQLiteScale(b, 500_000, false, scaleEmbedding)
}
func BenchmarkSQLiteScaleEmbedding500kFreshOpen(b *testing.B) {
	benchSQLiteScale(b, 500_000, true, scaleEmbedding)
}

type scaleScenario func(context.Context, *Repository) error

func benchSQLiteScale(b *testing.B, shots int, freshOpen bool, scenario scaleScenario) {
	b.Helper()
	if !scaleSizeEnabled(shots) {
		b.Skipf("size %d is disabled; set NEXUSGATE_SQLITE_SCALE_SIZES to enable it", shots)
	}
	corpus := scaleBenchmarkCorpus(b, shots)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		repo := corpus.repo
		var closeRepo func() error
		if freshOpen {
			var err error
			repo, err = Open(corpus.path)
			if err != nil {
				b.Fatal(err)
			}
			closeRepo = repo.Close
		}
		err := scenario(ctx, repo)
		if closeRepo != nil {
			_ = closeRepo()
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}

func scaleLexical(ctx context.Context, repo *Repository) error {
	hits, err := repo.LexicalRankedShots(ctx, "rain city", [5]float64{1, 1, 1, 1, 1}, 20)
	if err == nil && len(hits) == 0 {
		return fmt.Errorf("lexical search returned no hits")
	}
	return err
}

func scaleHybrid(ctx context.Context, repo *Repository) error {
	hits, err := repo.HybridSearchShots(ctx, "rain city car", 20)
	if err == nil && len(hits) == 0 {
		return fmt.Errorf("hybrid search returned no hits")
	}
	return err
}

func scaleFactEvidence(ctx context.Context, repo *Repository) error {
	response, err := search.NewService(repo, search.DefaultOptions()).Search(ctx, search.SearchRequest{
		Query: "car", Mode: "fact", Limit: 20, IncludeEvidence: true,
	})
	if err == nil && len(response.Results) == 0 {
		return fmt.Errorf("fact/evidence search returned no hits")
	}
	return err
}

func scaleContext(ctx context.Context, repo *Repository) error {
	response, err := search.NewService(repo, search.DefaultOptions()).Search(ctx, search.SearchRequest{
		Query: "car", Mode: "fact", Limit: 20, IncludeEvidence: true, IncludeContext: true,
	})
	if err != nil {
		return err
	}
	if len(response.Results) == 0 {
		return fmt.Errorf("context search returned no hits")
	}
	if response.Results[0].Context == nil {
		return fmt.Errorf("context search returned no context")
	}
	return nil
}

func scaleEmbedding(ctx context.Context, repo *Repository) error {
	retriever := search.NewTextEmbeddingRetriever(repo, scaleEmbedder{})
	hits, err := retriever.Retrieve(ctx, search.Compile("rain city car"), 20)
	if err == nil && len(hits) == 0 {
		return fmt.Errorf("embedding search returned no hits")
	}
	return err
}

type scaleEmbedder struct{}

func (scaleEmbedder) Name() string  { return "nexusgate-scale-embedder" }
func (scaleEmbedder) Model() string { return scaleEmbeddingModel }
func (scaleEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	vector := make([]float64, scaleEmbeddingDim)
	vector[0] = 1
	vector[1] = 0.01
	vector[2] = 0.02
	return [][]float64{vector}, nil
}

func scaleSizeEnabled(shots int) bool {
	value := strings.TrimSpace(os.Getenv("NEXUSGATE_SQLITE_SCALE_SIZES"))
	if value == "" {
		return shots == 1_000
	}
	if value == "all" {
		return shots == 1_000 || shots == 10_000 || shots == 100_000 || shots == 500_000
	}
	for _, item := range strings.Split(value, ",") {
		if strings.TrimSpace(item) == strconv.Itoa(shots) {
			return true
		}
	}
	return false
}

func scaleBenchmarkCorpus(b *testing.B, shots int) *scaleCorpus {
	b.Helper()
	scaleCorpora.Lock()
	defer scaleCorpora.Unlock()
	if corpus := scaleCorpora.items[shots]; corpus != nil {
		return corpus
	}
	path := filepath.Join(scaleBenchmarkDir(), fmt.Sprintf("scale-%d.db", shots))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatal(err)
	}
	// These exact files are benchmark-owned. WAL sidecars must be removed too,
	// otherwise an interrupted prior run could make the seed non-deterministic.
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			b.Fatalf("remove stale benchmark database %s: %v", candidate, err)
		}
	}
	repo, err := Open(path)
	if err != nil {
		b.Fatalf("open scale database: %v", err)
	}
	if err := repo.Migrate(context.Background()); err != nil {
		_ = repo.Close()
		b.Fatalf("migrate scale database: %v", err)
	}
	if err := seedScaleCorpus(context.Background(), repo, shots); err != nil {
		_ = repo.Close()
		b.Fatalf("seed %d-shot scale database: %v", shots, err)
	}
	corpus := &scaleCorpus{repo: repo, path: path, shots: shots}
	scaleCorpora.items[shots] = corpus
	return corpus
}

func openScaleTestRepo(t *testing.T, shots int) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), fmt.Sprintf("scale-%d.db", shots)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := seedScaleCorpus(context.Background(), repo, shots); err != nil {
		t.Fatal(err)
	}
	return repo
}

func scaleBenchmarkDir() string {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join(".bench", "sqlite")
	}
	// source is internal/repository/sqlite/scale_benchmark_test.go.
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	return filepath.Join(root, ".bench", "sqlite")
}

// seedScaleCorpus builds a canonical, indexed corpus directly inside one
// transaction. This is synthetic benchmark input, not model output: no
// production request can call this helper, and benchmark data never enters a
// user's library. The rows match the projections used by real search methods.
func seedScaleCorpus(ctx context.Context, repo *Repository, shots int) error {
	if shots <= 0 || shots%scaleShotsPerAsset != 0 {
		return fmt.Errorf("shots must be a positive multiple of %d", scaleShotsPerAsset)
	}
	db := repo.DB()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := "2026-01-01T00:00:00.000000000Z"
	rootID := "scale-root"
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES(?,?,?,?)`, rootID, "/nexusgate-scale-corpus", now, now); err != nil {
		return err
	}

	assetStmt, err := tx.PrepareContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`)
	if err != nil {
		return err
	}
	defer assetStmt.Close()
	locationStmt, err := tx.PrepareContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,0,1,1,?)`)
	if err != nil {
		return err
	}
	defer locationStmt.Close()
	shotStmt, err := tx.PrepareContext(ctx, `INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES(?,?,NULL,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer shotStmt.Close()
	ftsStmt, err := tx.PrepareContext(ctx, `INSERT INTO asset_shot_search(shot_id,asset_id,description,tags,objects,actions,mood) VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ftsStmt.Close()
	rowIDStmt, err := tx.PrepareContext(ctx, `INSERT INTO asset_shot_search_rowids(shot_id,asset_id,search_rowid) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer rowIDStmt.Close()
	semanticStmt, err := tx.PrepareContext(ctx, `INSERT INTO shot_semantic_vectors(shot_id,model,vector_json,source_text,created_at) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer semanticStmt.Close()
	embeddingStmt, err := tx.PrepareContext(ctx, `INSERT INTO shot_text_embeddings(shot_id,model,vector_blob,source_text_hash,created_at) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer embeddingStmt.Close()

	tagsJSON := `["rain","city"]`
	objectsJSON := `["car"]`
	actionsJSON := `["driving"]`
	moodJSON := `["cinematic"]`
	for i := 0; i < shots/scaleShotsPerAsset; i++ {
		assetID := fmt.Sprintf("scale-asset-%07d", i)
		if _, err := assetStmt.ExecContext(ctx, assetID, "scale-fingerprint-"+strconv.Itoa(i), now, now); err != nil {
			return err
		}
		if _, err := locationStmt.ExecContext(ctx, "scale-location-"+strconv.Itoa(i), assetID, rootID, fmt.Sprintf("clips/scale-%07d.mp4", i), fmt.Sprintf("/nexusgate-scale-corpus/clips/scale-%07d.mp4", i), now); err != nil {
			return err
		}
		for ordinal := 0; ordinal < scaleShotsPerAsset; ordinal++ {
			shotID := fmt.Sprintf("scale-shot-%09d", i*scaleShotsPerAsset+ordinal)
			description := fmt.Sprintf("rain city car footage clip %07d shot %02d", i, ordinal)
			startMS := int64(ordinal) * 10_000
			endMS := startMS + 8_000
			if _, err := shotStmt.ExecContext(ctx, shotID, assetID, ordinal, startMS, endMS, description, tagsJSON, objectsJSON, actionsJSON, moodJSON, 0.9, now); err != nil {
				return err
			}
			ftsResult, err := ftsStmt.ExecContext(ctx, shotID, assetID, indexText(description), indexText("rain city"), indexText("car"), indexText("driving"), indexText("cinematic"))
			if err != nil {
				return err
			}
			rowID, err := ftsResult.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := rowIDStmt.ExecContext(ctx, shotID, assetID, rowID); err != nil {
				return err
			}
			shot := domain.AssetShot{ID: shotID, AssetID: assetID, Description: description, Tags: []string{"rain", "city"}, Objects: []string{"car"}, Actions: []string{"driving"}, Mood: []string{"cinematic"}}
			semantic, err := json.Marshal(discovery.VectorForShot(shot))
			if err != nil {
				return err
			}
			if _, err := semanticStmt.ExecContext(ctx, shotID, discovery.HeuristicVectorModel, string(semantic), description, now); err != nil {
				return err
			}
			if _, err := embeddingStmt.ExecContext(ctx, shotID, scaleEmbeddingModel, encodeVectorBlob(scaleEmbeddingVector(i*scaleShotsPerAsset+ordinal)), "scale-source-"+strconv.Itoa(i*scaleShotsPerAsset+ordinal), now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func scaleEmbeddingVector(index int) []float32 {
	vector := make([]float32, scaleEmbeddingDim)
	vector[0] = 1
	vector[1] = float32(index%97+1) / 1000
	vector[2] = float32(index%89+1) / 1000
	vector[3] = float32(index%83+1) / 1000
	// Keep the remaining dimensions non-zero so this is a representative
	// float32 vector blob rather than a sparse special case.
	for i := 4; i < len(vector); i++ {
		vector[i] = float32((index+i)%17+1) / 10_000
	}
	return vector
}

// Compile-time check that the fixture's embedding provider remains compatible
// with the Search v2 boundary.
var _ search.TextEmbedder = scaleEmbedder{}
