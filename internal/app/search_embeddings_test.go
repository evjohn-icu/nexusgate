package app

import (
	"context"
	"hash/fnv"
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusgate/internal/search"
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
	if err := repo.ReplaceAssetShots(ctx, "asset-1", "", shots, "", ""); err != nil {
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
	if err := repo.ReplaceAssetShots(ctx, "asset-1", "", []domain.AssetShot{changed, committed[1].AssetShot}, "", ""); err != nil {
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

	// Backend switch: the planned space becomes test-embed-v2, the rebuild
	// re-embeds every shot there (its planned-space rows are all missing) and
	// the canonical analysis is untouched. Vectors are keyed per space now,
	// so the old space's rows are NOT destroyed: a fallback backend's vectors
	// stay readable for as long as their source hash matches the current
	// projection, and a later switch back finds them fresh.
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
	if len(oldRows) != 2 {
		t.Fatalf("other spaces' rows must survive a rebuild, got %d", len(oldRows))
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

// TestEmbeddingFallbackBatchStaysUnderFallbackAndRecoveryWritesPrimary drives
// the real storage path through a routed provider: the primary backend is
// down, so the incremental hook's batch lands under the FALLBACK space; a
// primary-space query has nothing to read; after recovery the rebuild writes
// primary rows without destroying the fallback rows; and content that changed
// since the fallback was written is invisible to reads in both spaces until
// it is re-embedded.
func TestEmbeddingFallbackBatchStaysUnderFallbackAndRecoveryWritesPrimary(t *testing.T) {
	repo, _ := openEmbeddingTestRepo(t)
	fixture := &embeddingFixtureServer{primaryFailuresLeft: 6}
	fixture.Server = httptest.NewServer(fixture.handle(t))
	defer fixture.Close()

	routes := newEmbeddingChannelRoutes(fixture.URL)
	runtime := newProviderChannelRuntime(&runtimeChannelRepo{channels: routes}, config.Config{}, &runtimeSecrets{values: map[string]string{
		"provider/embed-primary": "key-primary", "provider/embed-fallback": "key-fallback",
	}}, nil, nil, nil, nil, nil, nil)
	svc := &Service{repo: repo, embedder: runtime.embedder()}
	adapter := searchEmbedderAdapter{embedder: svc.embedder}
	primarySpace := adapter.EmbeddingSpaceID()
	if primarySpace == "" {
		t.Fatal("planned space must be the primary channel's identity")
	}
	// The fallback route's space, computed from its channel metadata alone (a
	// runtime holding only that route plans its space with no provider call).
	fallbackOnly := newProviderChannelRuntime(&runtimeChannelRepo{channels: routes[1:]}, config.Config{}, &runtimeSecrets{values: map[string]string{
		"provider/embed-fallback": "key-fallback",
	}}, nil, nil, nil, nil, nil, nil)
	fallbackSpace := fallbackOnly.embedder().(*channelEmbedder).EmbeddingSpaceID()
	if fallbackSpace == primarySpace || fallbackSpace == "" {
		t.Fatalf("fallback space %q must differ from primary %q", fallbackSpace, primarySpace)
	}
	ctx := context.Background()

	// Primary down: the batch is served by the fallback and stored there.
	svc.ensureShotTextEmbeddings(ctx, "asset-1")
	fallbackRows, err := repo.ListShotTextEmbeddings(ctx, fallbackSpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallbackRows) != 2 {
		t.Fatalf("fallback-served batch must be stored under the fallback space, got %d rows", len(fallbackRows))
	}
	primaryRows, err := repo.ListShotTextEmbeddings(ctx, primarySpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(primaryRows) != 0 {
		t.Fatalf("a primary-space query has nothing to read while the fallback wrote the batch, got %d rows", len(primaryRows))
	}
	fallbackVectors := make(map[string][]float32, len(fallbackRows))
	for _, row := range fallbackRows {
		fallbackVectors[row.Shot.ID] = row.Vector
	}

	// Recovery: the rebuild writes primary rows and leaves fallback rows
	// exactly as they are — no destructive rewrite across spaces.
	fixture.mu.Lock()
	fixture.primaryFailuresLeft = 0
	fixture.mu.Unlock()
	runtimeRecovered := newProviderChannelRuntime(&runtimeChannelRepo{channels: newEmbeddingChannelRoutes(fixture.URL)}, config.Config{}, &runtimeSecrets{values: map[string]string{
		"provider/embed-primary": "key-primary", "provider/embed-fallback": "key-fallback",
	}}, nil, nil, nil, nil, nil, nil)
	recovered := &Service{repo: repo, embedder: runtimeRecovered.embedder()}
	written, err := recovered.RebuildShotTextEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if written != 2 {
		t.Fatalf("recovered rebuild must embed both shots under primary, got %d", written)
	}
	primaryRows, err = repo.ListShotTextEmbeddings(ctx, primarySpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(primaryRows) != 2 {
		t.Fatalf("primary space must hold both shots after recovery, got %d rows", len(primaryRows))
	}
	fallbackRows, err = repo.ListShotTextEmbeddings(ctx, fallbackSpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallbackRows) != 2 {
		t.Fatalf("fallback rows must survive the primary rebuild, got %d rows", len(fallbackRows))
	}
	for _, row := range fallbackRows {
		want := fallbackVectors[row.Shot.ID]
		if len(want) != len(row.Vector) {
			t.Fatalf("fallback vector rewritten: %s", row.Shot.ID)
		}
		for i := range want {
			if want[i] != row.Vector[i] {
				t.Fatalf("fallback vector rewritten: %s", row.Shot.ID)
			}
		}
	}

	// Changed content invalidates the stale rows: both spaces' rows for the
	// changed shot no longer match the projection, so reads return nothing
	// until a re-embed stores the new text's vector. The recovered primary
	// serves the incremental hook; the stale fallback row stays in the table
	// but must never surface.
	changed := domain.AssetShot{ID: "shot-1", AssetID: "asset-1", Ordinal: 0, StartMS: 0, EndMS: 5000, Description: "blue van crossing"}
	unchanged := domain.AssetShot{ID: "shot-2", AssetID: "asset-1", Ordinal: 1, StartMS: 5000, EndMS: 10_000, Description: "empty parking lot"}
	if err := repo.ReplaceAssetShots(ctx, "asset-1", "", []domain.AssetShot{changed, unchanged}, "", ""); err != nil {
		t.Fatal(err)
	}
	stalePrimary, err := repo.ListShotTextEmbeddings(ctx, primarySpace)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range stalePrimary {
		if row.Shot.ID == "shot-1" {
			t.Fatal("a stale primary-space row must be filtered from reads")
		}
	}
	svc2 := &Service{repo: repo, embedder: runtimeRecovered.embedder()}
	svc2.ensureShotTextEmbeddings(ctx, "asset-1")
	refreshedPrimary, err := repo.ListShotTextEmbeddings(ctx, primarySpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshedPrimary) != 2 {
		t.Fatalf("hook must re-embed the changed shot under primary, got %d rows", len(refreshedPrimary))
	}
	refreshedFallback, err := repo.ListShotTextEmbeddings(ctx, fallbackSpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshedFallback) != 1 {
		t.Fatalf("fallback must keep only its still-valid row, got %d", len(refreshedFallback))
	}
	if refreshedFallback[0].Shot.ID != "shot-2" {
		t.Fatalf("the fallback row surviving a content change must be the untouched shot, got %s", refreshedFallback[0].Shot.ID)
	}
}

// scriptedEmbedder answers every batch with a fixed script: a provider that
// misbehaves the way a half-broken gateway can.
type scriptedEmbedder struct {
	vectors [][]float64
	err     error
}

func (s scriptedEmbedder) Name() string  { return "scripted" }
func (s scriptedEmbedder) Model() string { return "scripted-v1" }
func (s scriptedEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	return s.vectors, s.err
}

// TestEmbedShotDocumentsRejectsMalformedBatch: a ragged or malformed batch is
// an error BEFORE any write — no partial space poisoning, no silent drop.
func TestEmbedShotDocumentsRejectsMalformedBatch(t *testing.T) {
	repo, _ := openEmbeddingTestRepo(t)
	ctx := context.Background()
	docs := []search.ShotSearchDocument{
		{ShotID: "shot-1", Description: "red car"},
		{ShotID: "shot-2", Description: "dog park"},
	}
	cases := map[string]scriptedEmbedder{
		"fewer vectors than inputs": {vectors: [][]float64{{1, 0}}},
		"mixed dimensions":          {vectors: [][]float64{{1, 0}, {1, 0, 0}}},
		"mixed empty and real":      {vectors: [][]float64{{1, 0}, {}}},
		"non-finite component":      {vectors: [][]float64{{1, 0}, {math.NaN(), 1}}},
		"zero norm":                 {vectors: [][]float64{{1, 0}, {0, 0}}},
	}
	for name, provider := range cases {
		adapter := searchEmbedderAdapter{embedder: provider}
		if _, err := embedShotDocuments(ctx, repo, adapter, docs); err == nil {
			t.Fatalf("%s: malformed batch must fail", name)
		}
		rows, err := repo.ListShotTextEmbeddings(ctx, provider.Model())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("%s: failed batch must write nothing, got %d rows", name, len(rows))
		}
	}
	// All-empty vectors: the provider said nothing — prior no-vector
	// semantics, no error, no rows.
	allEmpty := searchEmbedderAdapter{embedder: zeroVectorEmbedder{}}
	persisted, err := embedShotDocuments(ctx, repo, allEmpty, docs)
	if err != nil || persisted != 0 {
		t.Fatalf("all-empty batch = %d, err=%v; want 0, nil", persisted, err)
	}
}
