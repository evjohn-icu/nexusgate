package search

// Synthetic engine benchmarks over an in-memory corpus. There is no SQLite
// here: the ShotStore contract (store.go) is implemented by a scripted fake,
// so the numbers measure the ENGINE — compile → intent routing → channels →
// fusion → evidence gate → selection → response assembly — and the
// O(N·D) cosine full scan, never persistence I/O. The corpus sizes (1k /
// 10k / 100k shots) are the library scale the Hub stores behind the same
// interface; results are recorded in docs/performance.md.
//
// These are `func Benchmark` entries only: they never run under `go test`
// without -bench, so the normal suite stays fast. Run:
//
//	go test ./internal/search/ -bench . -benchmem -benchtime 1s -run '^$'

import (
	"context"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// benchmarkStore is the scripted ShotStore. Every retrieval channel returns
// the same N pre-scripted candidates (the limit argument is ignored on
// purpose — the stress dimension is N candidates flowing through the whole
// pipeline, while the SQLite implementation would cap each channel at the
// recall pool). The engine only reads these slices; retrieval converts to
// fresh Candidates, so the corpus is reused across iterations untouched.
type benchmarkStore struct {
	candidates []domain.ShotSearchResult
	rows       []ShotEmbeddingRow
}

var _ ShotStore = (*benchmarkStore)(nil)

func (s *benchmarkStore) ScoreCandidates(context.Context, string, domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return s.candidates, nil
}

func (s *benchmarkStore) ScoreCandidatesV2(context.Context, string, domain.FacetFilter, domain.AssetContextFilter) ([]domain.ShotSearchResult, error) {
	return s.candidates, nil
}

func (s *benchmarkStore) LexicalRankedShots(context.Context, string, [5]float64, int) ([]domain.ShotSearchResult, error) {
	return s.candidates, nil
}

func (s *benchmarkStore) TranscriptRankedShots(context.Context, string, int) ([]domain.ShotSearchResult, error) {
	return s.candidates, nil
}

func (s *benchmarkStore) MetadataRankedShots(context.Context, string, int) ([]domain.ShotSearchResult, error) {
	return s.candidates, nil
}

func (s *benchmarkStore) ShotTranscriptSpans(context.Context, string, int64, int64) ([]domain.AlignmentWord, error) {
	return nil, nil
}

func (s *benchmarkStore) ShotTranscriptSpansBatch(_ context.Context, requests []TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error) {
	return make(map[string][]domain.AlignmentWord, len(requests)), nil
}

func (s *benchmarkStore) NeighborShots(context.Context, string, int) (*domain.AssetShot, *domain.AssetShot, error) {
	return nil, nil, nil
}

func (s *benchmarkStore) NeighborShotsBatch(_ context.Context, requests []NeighborRequest) (map[string]Neighbors, error) {
	return make(map[string]Neighbors, len(requests)), nil
}

func (s *benchmarkStore) ShotSession(context.Context, string) (string, error) {
	return "", nil
}

// benchmarkEmptySessions is the scripted no-shoot-session answer: one shared
// empty map, so selection's batched lookup costs one map read per candidate
// and never allocates under measurement.
var benchmarkEmptySessions = map[string]string{}

func (s *benchmarkStore) ShotSessions(context.Context, []string) (map[string]string, error) {
	return benchmarkEmptySessions, nil
}

func (s *benchmarkStore) UpsertShotTextEmbeddings(context.Context, []ShotEmbeddingRow) error {
	return nil
}

func (s *benchmarkStore) ListShotTextEmbeddings(context.Context, string) ([]ShotEmbeddingRow, error) {
	return s.rows, nil
}

func (s *benchmarkStore) AllShotTextDocuments(context.Context) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (s *benchmarkStore) ShotTextDocumentsByAsset(context.Context, string) ([]ShotSearchDocument, error) {
	return nil, nil
}

func (s *benchmarkStore) ShotTextEmbeddingHashes(context.Context, string, string) (map[string]string, error) {
	return nil, nil
}

// buildSearchCorpus returns N scripted shots. Every shot satisfies the
// benchmark query's must constraints (person + umbrella in structured fields)
// so the fact-intent gate keeps the pool, and every channel's score declines
// with the index. Because all channels rank the same order, the fused RRF
// list is already sorted: the benchmark measures the pipeline's O(N) passes
// and the per-candidate evidence work, not a pathological insertion sort over
// an adversarial order (which production never produces either — the recall
// pool caps every channel at 200 candidates).
func buildSearchCorpus(n int) []domain.ShotSearchResult {
	out := make([]domain.ShotSearchResult, 0, n)
	for i := 0; i < n; i++ {
		id := strconv.Itoa(i)
		decline := 1 - float64(i)/float64(n)
		out = append(out, domain.ShotSearchResult{
			AssetShot: domain.AssetShot{
				ID:          "shot-" + id,
				AssetID:     "asset-" + id,
				Ordinal:     i,
				StartMS:     int64(i) * 20_000,
				EndMS:       int64(i)*20_000 + 8_000,
				Description: "夜晚下雨的街头 行人撑着伞走过",
				Objects:     []string{"person", "umbrella"},
				Confidence:  0.9,
			},
			Filename:        "footage-" + id + ".mp4",
			LexicalScore:    decline,
			SemanticScore:   decline,
			TranscriptScore: decline,
			MetadataScore:   decline,
		})
	}
	return out
}

func benchSearch(b *testing.B, n int) {
	store := &benchmarkStore{candidates: buildSearchCorpus(n)}
	svc := NewService(store, DefaultOptions())
	req := SearchRequest{Query: "夜晚下雨 有人撑伞", Limit: 20, IncludeEvidence: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		response, err := svc.Search(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		if len(response.Results) != 20 {
			b.Fatalf("want 20 results, got %d", len(response.Results))
		}
	}
}

func BenchmarkSearch1k(b *testing.B)   { benchSearch(b, 1_000) }
func BenchmarkSearch10k(b *testing.B)  { benchSearch(b, 10_000) }
func BenchmarkSearch100k(b *testing.B) { benchSearch(b, 100_000) }

// embeddingDim is the corpus vector dimension — the storage schema's
// 256-dim model shape, the same dimension the real providers emit.
const embeddingDim = 256

// buildEmbeddingCorpus returns N stored 256-dim rows plus the query vector.
// Row i's cosine against the query declines strictly with i: the vector
// points along the (unit) query direction scaled by 1-i/N, plus a
// query-orthogonal noise of fixed norm. The scan therefore measures the
// real O(N·D) dot-product hot spot, while the post-scan sort sees an
// already-ranked list — exactly the deterministic (shot_id) order the
// SQLite store returns.
func buildEmbeddingCorpus(n int) ([]ShotEmbeddingRow, []float64) {
	rng := rand.New(rand.NewSource(42))
	query := make([]float64, embeddingDim)
	queryNorm := 0.0
	for j := range query {
		query[j] = rng.NormFloat64()
		queryNorm += query[j] * query[j]
	}
	for j := range query {
		query[j] /= math.Sqrt(queryNorm)
	}
	const noiseNorm = 0.3
	rows := make([]ShotEmbeddingRow, 0, n)
	for i := 0; i < n; i++ {
		id := strconv.Itoa(i)
		along := 1 - float64(i)/float64(n)
		w := make([]float64, embeddingDim)
		dot := 0.0
		wNorm := 0.0
		for j := range w {
			w[j] = rng.NormFloat64()
			dot += w[j] * query[j]
			wNorm += w[j] * w[j]
		}
		projectionNorm := math.Sqrt(wNorm - dot*dot)
		vec := make([]float32, embeddingDim)
		for j := range vec {
			noise := (w[j] - dot*query[j]) / projectionNorm * noiseNorm
			vec[j] = float32(along*query[j] + noise)
		}
		rows = append(rows, ShotEmbeddingRow{
			Shot:           domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "embed-" + id}},
			Model:          benchmarkEmbeddingModel,
			Vector:         vec,
			SourceTextHash: "hash-" + id,
		})
	}
	return rows, query
}

// benchmarkEmbeddingModel must match what the fake embedder reports, so the
// retriever's model gate passes the stored rows through.
const benchmarkEmbeddingModel = "benchmark-model"

// benchmarkEmbedder is the fake text-embedding provider: one deterministic
// query vector, never an I/O cost under measurement.
type benchmarkEmbedder struct {
	vector []float64
}

func (e *benchmarkEmbedder) Name() string  { return "benchmark-embedder" }
func (e *benchmarkEmbedder) Model() string { return benchmarkEmbeddingModel }
func (e *benchmarkEmbedder) Embed(context.Context, []string) ([][]float64, error) {
	return [][]float64{e.vector}, nil
}

func benchEmbeddingScan(b *testing.B, n int) {
	rows, query := buildEmbeddingCorpus(n)
	store := &benchmarkStore{rows: rows}
	retriever := NewTextEmbeddingRetriever(store, &benchmarkEmbedder{vector: query})
	q := Compile("夜晚下雨 有人撑伞")
	b.ReportAllocs()
	// Bytes per op = the vectors the scan touches (float32 x dim per row);
	// the harness turns this into the MB/s column.
	b.SetBytes(int64(n) * embeddingDim * 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		candidates, err := retriever.Retrieve(context.Background(), q, 20)
		if err != nil {
			b.Fatal(err)
		}
		if len(candidates) == 0 {
			b.Fatal("embedding retriever must return candidates")
		}
	}
}

func BenchmarkEmbeddingFullScan1k(b *testing.B)   { benchEmbeddingScan(b, 1_000) }
func BenchmarkEmbeddingFullScan10k(b *testing.B)  { benchEmbeddingScan(b, 10_000) }
func BenchmarkEmbeddingFullScan100k(b *testing.B) { benchEmbeddingScan(b, 100_000) }

// benchmarkQueries are typical Chinese footage searches the compiler must
// handle: a fact query, a negation, a speech claim, a creative request and a
// shot-id similarity. Panic-safety is pinned separately by
// compiler_panic_test.go; this benchmark measures the offline compile cost.
var benchmarkQueries = []string{
	"夜晚下雨 有人撑伞",
	"没有人的海边空镜",
	"他说“明天见”然后转身离开",
	"给我找点城市夜景车流的素材",
	"shot_ab12cd 类似的镜头",
}

// benchmarkSink keeps the compiler's result observable so the loop cannot be
// optimized away.
var benchmarkSink SearchQuery

func BenchmarkCompile(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkSink = Compile(benchmarkQueries[i%len(benchmarkQueries)])
	}
}
