package sqlite

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/search"
)

// The Search v2 benchmark compares five retrieval pipelines over the golden +
// v2 corpus and reports the same metrics the legacy golden set uses, plus the
// two new ones:
//
//	RetrievalFP — a notRelevant shot in the top-10 (recall-level false positive)
//	AssertionFP — a top-10 result whose must constraint has NO evidence at all
//	              (unknown state): the search "claims" the shot satisfies the
//	              query without evidence behind it. Per-result binary count,
//	              fact/negative queries only. The legacy pipeline cannot
//	              compute it (no evidence in its response) and reports N/A.
//
// Per-intent buckets (fact/speech/semantic/negative/creative) let a regression
// in one intent family be seen immediately. Router agreement reports how often
// the offline router matches the hand-tagged intent.

type benchmarkMetrics struct {
	precision5, precision10, recall10 float64
	retrievalFP, assertionFP          int
	relevantHits                      int
}

type pipelineResult struct {
	metrics      benchmarkMetrics
	perIntent    map[string]*benchmarkMetrics
	routerAgreed int
	// duplicateSuppression counts how many near-duplicate relevant shots were
	// selected away on the dedicated fixture (query "red car").
	duplicateSuppression int
}

// taggedIntent is the hand-tagged true intent for every golden query.
func taggedIntent(q string) search.SearchIntent {
	switch q {
	case "hello", "你好世界", "天气预报", "明天见", "narration":
		return search.IntentSpeech
	case "carefree":
		return search.IntentSemantic
	case "没有人的海边空镜":
		return search.IntentFact // negative
	case "孤独压抑的夜晚":
		return search.IntentSemantic
	case "谁说过我们明天出发":
		return search.IntentSpeech
	case "train", "rain", "red car", "夜晚下雨，有人撑伞走过街道":
		return search.IntentFact
	}
	// Golden queries with concrete objects are fact questions; the rest are
	// scene/mood descriptions.
	factQueries := map[string]bool{
		"car": true, "汽车": true, "person walking": true, "行人": true,
		"red sedan": true, "轿车": true, "vehicle": true, "traffic": true,
		"night car": true, "rain headlights": true, "night": true,
		"dog": true, "children swings": true, "cat": true, "laptop desk": true,
		"schoolyard children": true, "factory machinery": true,
		"flower market": true, "tulips": true, "ceremony ten minute": true,
		"boat": true, "chef plating": true, "empty dining room": true,
		"student desk": true, "commuters platform": true,
		"coffee cup": true, "咖啡": true, "empty counter": true,
		"bread loaves": true, "bicycle repair": true, "ripe tomatoes": true,
		"fishing boats": true,
	}
	if factQueries[q] {
		return search.IntentFact
	}
	return search.IntentSemantic
}

// benchmarkQueries merges the golden and v2 queries into one list with their
// true intents.
type benchmarkQuery struct {
	q           string
	intent      search.SearchIntent
	bucket      string // per-intent metric label (negative is its own bucket)
	relevant    []string
	notRelevant []string
	comment     string
}

func benchmarkQueries() []benchmarkQuery {
	var out []benchmarkQuery
	for _, gq := range goldenQueries() {
		intent := taggedIntent(gq.q)
		out = append(out, benchmarkQuery{
			q: gq.q, intent: intent, bucket: string(intent),
			relevant: gq.relevant, notRelevant: gq.notRelevant, comment: gq.comment,
		})
	}
	for _, vq := range searchV2Queries() {
		intent := search.SearchIntent(vq.intent)
		if intent == "negative" {
			// Negative queries run with the fact intent (MustNot semantics
			// are enforced by the gate) but bucket under their own label.
			intent = search.IntentFact
		}
		out = append(out, benchmarkQuery{
			q: vq.q, intent: intent, bucket: vq.intent,
			relevant: vq.relevant, notRelevant: vq.notRelevant, comment: vq.comment,
		})
	}
	return out
}

// fakeBenchEmbedder is the deterministic, offline embedding provider the
// benchmark seeds shot vectors with: token-hash vectors in their own 256-dim
// space, distinct from the discovery heuristic, so the text_embedding channel
// provably contributes an orthogonal signal.
type fakeBenchEmbedder struct{}

func (fakeBenchEmbedder) Name() string  { return "bench-embed" }
func (fakeBenchEmbedder) Model() string { return "bench-embed-v1" }

func (fakeBenchEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = benchEmbedVector(text)
	}
	return out, nil
}

func benchEmbedVector(text string) []float64 {
	const dim = 256
	vector := make([]float64, dim)
	seen := map[string]bool{}
	for _, token := range benchEmbedTokens(text) {
		if seen[token] {
			continue
		}
		seen[token] = true
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		sum := h.Sum64()
		if sum&1 == 0 {
			vector[int(sum%dim)] += 1
		} else {
			vector[int(sum%dim)] -= 1
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

func benchEmbedTokens(text string) []string {
	var out []string
	run := []rune{}
	flush := func() {
		if len(run) == 0 {
			return
		}
		word := string(run)
		run = nil
		if len([]rune(word)) > 3 {
			// CJK bigrams for long runs, ASCII words keep their identity.
			for i := 0; i+1 < len([]rune(word)); i++ {
				out = append(out, string([]rune(word)[i:i+2]))
			}
			return
		}
		out = append(out, word)
	}
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '\u3040' && r <= '\u9fff' {
			run = append(run, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// seedBenchEmbeddings writes vectors for every corpus shot under the fake
// model, through the real store methods — the same path a real rebuild uses.
func seedBenchEmbeddings(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	docs, err := repo.AllShotTextDocuments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, len(docs))
	for i, doc := range docs {
		texts[i] = search.ShotTextSource(doc)
	}
	vectors, err := fakeBenchEmbedder{}.Embed(ctx, texts)
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
			Model:          "bench-embed-v1",
			Vector:         vector,
			SourceTextHash: search.ShotTextSourceHash(doc),
		})
	}
	if err := repo.UpsertShotTextEmbeddings(ctx, rows); err != nil {
		t.Fatal(err)
	}
}

func TestSearchV2Benchmark(t *testing.T) {
	repo, ids := seedSearchV2Corpus(t)
	ctx := context.Background()
	seedBenchEmbeddings(t, repo)
	queries := benchmarkQueries()

	// The five pipelines. Mode is pinned to the tagged intent so per-intent
	// metrics measure the profile, not the router; router agreement is
	// measured separately.
	weighted := search.NewService(repo, search.Options{
		Fusion:         &search.WeightedBlend{Weights: search.Profiles()[search.IntentFact].ChannelWeights},
		GateFact:       false,
		ProfileVersion: "benchmark",
	})
	rrf := search.NewService(repo, search.Options{
		Fusion:         &search.RRF{K: 60},
		GateFact:       false,
		ProfileVersion: "benchmark",
	})
	gate := search.NewService(repo, search.Options{
		Fusion:         &search.RRF{K: 60},
		GateFact:       true,
		ProfileVersion: "benchmark",
	})
	diverse := search.NewService(repo, search.Options{
		Fusion:         &search.RRF{K: 60},
		GateFact:       true,
		ProfileVersion: "benchmark",
		Selection:      search.SelectionOptions{Diversity: 0.2, SameAssetPenalty: 0.25, SameSessionPenalty: 0.2, NearTimePenalty: 0.35, NearTimeWindowMS: 5000},
	})
	embed := search.NewService(repo, search.Options{
		Fusion:         &search.RRF{K: 60},
		GateFact:       true,
		Embedder:       fakeBenchEmbedder{},
		ProfileVersion: "benchmark",
	})

	type pipeline struct {
		name   string
		run    func(ctx context.Context, q string, intent search.SearchIntent) ([]search.ResultItem, error)
		legacy bool
	}
	pipelines := []pipeline{
		{"legacy-hybrid", func(ctx context.Context, q string, _ search.SearchIntent) ([]search.ResultItem, error) {
			hits, err := repo.HybridSearchShots(ctx, q, 10)
			if err != nil {
				return nil, err
			}
			items := make([]search.ResultItem, 0, len(hits))
			for _, h := range hits {
				items = append(items, search.ResultItem{ShotID: h.ID, Score: h.Score})
			}
			return items, nil
		}, true},
		{"v2-weighted", func(ctx context.Context, q string, intent search.SearchIntent) ([]search.ResultItem, error) {
			response, err := weighted.Search(ctx, search.SearchRequest{Query: q, Mode: string(intent), Limit: 10, IncludeEvidence: true})
			if err != nil {
				return nil, err
			}
			return response.Results, nil
		}, false},
		{"v2-rrf", func(ctx context.Context, q string, intent search.SearchIntent) ([]search.ResultItem, error) {
			response, err := rrf.Search(ctx, search.SearchRequest{Query: q, Mode: string(intent), Limit: 10, IncludeEvidence: true})
			if err != nil {
				return nil, err
			}
			return response.Results, nil
		}, false},
		{"v2-rrf-gate", func(ctx context.Context, q string, intent search.SearchIntent) ([]search.ResultItem, error) {
			response, err := gate.Search(ctx, search.SearchRequest{Query: q, Mode: string(intent), Limit: 10, IncludeEvidence: true})
			if err != nil {
				return nil, err
			}
			return response.Results, nil
		}, false},
		{"v2-rrf-gate-diversity", func(ctx context.Context, q string, intent search.SearchIntent) ([]search.ResultItem, error) {
			response, err := diverse.Search(ctx, search.SearchRequest{Query: q, Mode: string(intent), Limit: 10, IncludeEvidence: true})
			if err != nil {
				return nil, err
			}
			return response.Results, nil
		}, false},
		{"v2-rrf-gate-embed", func(ctx context.Context, q string, intent search.SearchIntent) ([]search.ResultItem, error) {
			response, err := embed.Search(ctx, search.SearchRequest{Query: q, Mode: string(intent), Limit: 10, IncludeEvidence: true})
			if err != nil {
				return nil, err
			}
			return response.Results, nil
		}, false},
	}

	routerAgreed := 0
	for _, bq := range queries {
		if search.Compile(bq.q).Intent == bq.intent {
			routerAgreed++
		}
	}

	results := map[string]*pipelineResult{}
	for _, p := range pipelines {
		pr := &pipelineResult{perIntent: map[string]*benchmarkMetrics{}}
		for _, bq := range queries {
			items, err := p.run(ctx, bq.q, bq.intent)
			if err != nil {
				t.Fatalf("%s: %q: %v", p.name, bq.q, err)
			}
			relevant := toGoldenIDs(ids, bq.relevant)
			notRelevant := toGoldenIDs(ids, bq.notRelevant)
			metrics := scoreBenchmarkQuery(items, relevant, notRelevant, bq.q, bq.intent, p.legacy)
			pr.metrics.add(metrics)
			bucket := pr.perIntent[bq.bucket]
			if bucket == nil {
				bucket = &benchmarkMetrics{}
				pr.perIntent[bq.bucket] = bucket
			}
			bucket.add(metrics)
			if bq.q == "red car" && metrics.duplicatesSelected >= 0 {
				pr.duplicateSuppression += metrics.duplicatesSelected
			}
		}
		pr.routerAgreed = routerAgreed
		results[p.name] = pr
	}

	t.Logf("queries=%d router-agreement=%d/%d", len(queries), routerAgreed, len(queries))
	t.Logf("pipeline                     P@5    P@10   R@10   RetrievalFP  AssertionFP")
	for _, p := range pipelines {
		pr := results[p.name]
		m := pr.metrics
		n := float64(len(queries))
		assertion := "N/A"
		if !p.legacy {
			assertion = fmt.Sprintf("%d", m.assertionFP)
		}
		t.Logf("%-26s %.3f  %.3f  %.3f  %-12d %s", p.name, m.precision5/n, m.precision10/n, m.recall10/n, m.retrievalFP, assertion)
	}
	for _, p := range pipelines {
		pr := results[p.name]
		t.Logf("== %s per-intent ==", p.name)
		for _, intent := range []string{"fact", "speech", "semantic", "negative", "creative"} {
			if bucket := pr.perIntent[intent]; bucket != nil {
				t.Logf("  %-10s P@5=%.3f P@10=%.3f R@10=%.3f FP=%d", intent, bucket.precision5, bucket.precision10, bucket.recall10, bucket.retrievalFP)
			}
		}
	}
	if p := results["v2-rrf-gate-diversity"]; p.duplicateSuppression == 0 {
		t.Logf("note: diversity pipeline selected away 0 near-duplicates (may already be resolved by fusion)")
	} else {
		t.Logf("diversity suppressed %d near-duplicate shot(s) on 'red car'", p.duplicateSuppression)
	}

	// Hard assertions: the gate must reduce unsupported assertions without
	// killing recall, and diversity must actually suppress duplicates.
	base := results["v2-rrf"].metrics
	gated := results["v2-rrf-gate"].metrics
	n := float64(len(queries))
	if gated.recall10 < 0.5*base.recall10 {
		t.Fatalf("gate killed recall: R@10 %.3f -> %.3f", base.recall10/n, gated.recall10/n)
	}
	if gated.assertionFP > base.assertionFP {
		t.Fatalf("gate increased assertion FPs: %d -> %d", base.assertionFP, gated.assertionFP)
	}
	if results["v2-rrf-gate-diversity"].duplicateSuppression != 1 {
		t.Fatalf("diversity must suppress the second near-duplicate car shot, got %d", results["v2-rrf-gate-diversity"].duplicateSuppression)
	}
	// The embedding channel is additive: it must not hurt retrieval (recall
	// within 10% of the gated baseline) and must not introduce false
	// positives where the gate had none.
	embedded := results["v2-rrf-gate-embed"].metrics
	if embedded.recall10 < 0.9*gated.recall10 {
		t.Fatalf("embedding channel hurt recall: R@10 %.3f -> %.3f", gated.recall10/n, embedded.recall10/n)
	}
	if embedded.retrievalFP > gated.retrievalFP {
		t.Fatalf("embedding channel added retrieval FPs: %d -> %d", gated.retrievalFP, embedded.retrievalFP)
	}
	if embedded.assertionFP > gated.assertionFP {
		t.Fatalf("embedding channel added assertion FPs: %d -> %d", gated.assertionFP, embedded.assertionFP)
	}
}

// benchmarkMetrics aggregates per-query scores. K denominators are fixed.
type queryMetrics struct {
	precision5, precision10, recall10 float64
	retrievalFP, assertionFP          int
	duplicatesSelected                int
}

func scoreBenchmarkQuery(items []search.ResultItem, relevant, notRelevant map[string]bool, q string, intent search.SearchIntent, legacy bool) queryMetrics {
	var m queryMetrics
	hit := func(k int) int {
		count := 0
		for i, item := range items {
			if i >= k {
				break
			}
			if relevant[item.ShotID] {
				count++
			}
		}
		return count
	}
	m.precision5 = float64(hit(5)) / 5
	m.precision10 = float64(hit(10)) / 10
	if len(relevant) > 0 {
		m.recall10 = float64(hit(10)) / float64(len(relevant))
	}
	for i, item := range items {
		if i >= 10 {
			break
		}
		if notRelevant[item.ShotID] {
			m.retrievalFP++
		}
		if legacy {
			continue
		}
		if (intent == search.IntentFact) && mustUnsupported(item, q) {
			m.assertionFP++
		}
	}
	// Duplicate suppression: among the near-duplicate relevant shots of the
	// dedicated fixture, how many were selected away.
	if q == "red car" {
		selected := 0
		for i, item := range items {
			if i >= 10 {
				break
			}
			if relevant[item.ShotID] {
				selected++
			}
		}
		m.duplicatesSelected = len(relevant) - selected
	}
	return m
}

// mustUnsupported reports whether the result carries a MUST constraint with
// no evidence at all (unknown state) — the assertion the gate exists to stop.
// Should constraints may legitimately stay unknown (a mood hint is not a
// claim), so only the query's musts count.
func mustUnsupported(item search.ResultItem, rawQuery string) bool {
	compiled := search.Compile(rawQuery)
	mustValues := make(map[string]bool, len(compiled.Must))
	for _, c := range compiled.Must {
		mustValues[c.Value] = true
	}
	for _, e := range item.Evidence {
		if e.Negated || !mustValues[e.Value] {
			continue
		}
		if e.State == search.EvidenceUnknown {
			return true
		}
	}
	return false
}

func (a *benchmarkMetrics) add(b queryMetrics) {
	a.precision5 += b.precision5
	a.precision10 += b.precision10
	a.recall10 += b.recall10
	a.retrievalFP += b.retrievalFP
	a.assertionFP += b.assertionFP
	a.relevantHits++
}
