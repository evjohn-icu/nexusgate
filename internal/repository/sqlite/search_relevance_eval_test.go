package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/search"
)

// The relevance eval is deliberately an ordinary Go test rather than a
// provider/evaluation harness. It seeds the same real SQLite corpus as the
// search golden tests, runs the deterministic Search v2 service, and compares
// only stable metrics against a checked-in baseline. Search IDs, timestamps,
// and minted SQLite shot IDs never enter the fixture or baseline.

type searchEvalCorpusFile struct {
	Version  int                    `json:"version"`
	Source   string                 `json:"source"`
	IDFormat string                 `json:"id_format"`
	Shots    []searchEvalCorpusShot `json:"shots"`
}

type searchEvalCorpusShot struct {
	ShotID  string `json:"shot_id"`
	AssetID string `json:"asset_id"`
	Ordinal int    `json:"ordinal"`
}

type searchEvalJudgmentsFile struct {
	Version int               `json:"version"`
	Queries []searchEvalQuery `json:"queries"`
}

type searchEvalQuery struct {
	ID            string               `json:"id"`
	Query         string               `json:"query"`
	Family        string               `json:"family"`
	Language      string               `json:"language"`
	Intent        string               `json:"intent"`
	Relevant      []searchEvalRelevant `json:"relevant"`
	NotRelevant   []string             `json:"not_relevant"`
	Evidence      []searchEvalEvidence `json:"evidence"`
	ExpectedEmpty bool                 `json:"expected_empty"`
}

type searchEvalRelevant struct {
	ShotID string `json:"shot_id"`
	Grade  int    `json:"grade"`
}

type searchEvalEvidence struct {
	ShotID     string   `json:"shot_id"`
	Constraint string   `json:"constraint"`
	Value      string   `json:"value"`
	States     []string `json:"states"`
	Sources    []string `json:"sources"`
}

type searchEvalBaselineFile struct {
	Version         int               `json:"version"`
	Runner          searchEvalRunner  `json:"runner"`
	Metrics         searchEvalMetrics `json:"metrics"`
	Notes           string            `json:"notes,omitempty"`
	CorpusSHA256    string            `json:"corpus_sha256"`
	JudgmentsSHA256 string            `json:"judgments_sha256"`
}

type searchEvalRunner struct {
	Mode            string `json:"mode"`
	Limit           int    `json:"limit"`
	IncludeEvidence bool   `json:"include_evidence"`
	IncludeContext  bool   `json:"include_context"`
	ProfileVersion  string `json:"profile_version"`
	Provider        string `json:"provider"`
}

type searchEvalMetrics struct {
	RecallAt20            float64 `json:"recall_at_20"`
	PrecisionAt10         float64 `json:"precision_at_10"`
	NDCGAt10              float64 `json:"ndcg_at_10"`
	EmptyResultRate       float64 `json:"empty_result_rate"`
	ExpectedEmptyAccuracy float64 `json:"expected_empty_accuracy"`
	IntentAccuracy        float64 `json:"intent_accuracy"`
	NotRelevantHits       int     `json:"not_relevant_hits"`
	EvidenceCorrectness   float64 `json:"evidence_correctness"`
	EvidencePrecision     float64 `json:"evidence_precision"`
}

type searchEvalAccumulator struct {
	queries, rankQueries, empty, expectedEmptyCorrect, intentCorrect int
	notRelevantHits                                                  int
	recall, precision, ndcg                                          float64
	evidenceChecks, evidenceHits                                     int
	evidenceResults, evidenceSupported                               int
}

func (a *searchEvalAccumulator) metrics() searchEvalMetrics {
	if a.queries == 0 {
		return searchEvalMetrics{}
	}
	m := searchEvalMetrics{
		EmptyResultRate:       float64(a.empty) / float64(a.queries),
		ExpectedEmptyAccuracy: float64(a.expectedEmptyCorrect) / float64(a.queries),
		IntentAccuracy:        float64(a.intentCorrect) / float64(a.queries),
		NotRelevantHits:       a.notRelevantHits,
	}
	if a.rankQueries > 0 {
		m.RecallAt20 = a.recall / float64(a.rankQueries)
	}
	m.PrecisionAt10 = a.precision / float64(a.queries)
	m.NDCGAt10 = a.ndcg / float64(a.queries)
	if a.evidenceChecks > 0 {
		m.EvidenceCorrectness = float64(a.evidenceHits) / float64(a.evidenceChecks)
	}
	if a.evidenceResults > 0 {
		m.EvidencePrecision = float64(a.evidenceSupported) / float64(a.evidenceResults)
	}
	return m
}

func TestSearchRelevanceEval(t *testing.T) {
	dataDir := searchEvalDataDir()
	corpus, corpusBytes := readSearchEvalJSON[searchEvalCorpusFile](t, filepath.Join(dataDir, "corpus.json"))
	judgments, judgmentsBytes := readSearchEvalJSON[searchEvalJudgmentsFile](t, filepath.Join(dataDir, "judgments.json"))
	baseline := readSearchEvalJSONFile[searchEvalBaselineFile](t, filepath.Join(dataDir, "baseline.json"))
	validateSearchEvalFixtures(t, corpus, judgments, baseline)

	if len(judgments.Queries) != 32 {
		t.Fatalf("search relevance corpus has %d queries, want exactly 32", len(judgments.Queries))
	}

	repo, ids := seedSearchV2Corpus(t)
	for _, shot := range corpus.Shots {
		if ids[shot.ShotID] == "" {
			t.Fatalf("corpus shot %s was not produced by seedSearchV2Corpus", shot.ShotID)
		}
	}
	service := search.NewService(repo, search.DefaultOptions())
	ctx := context.Background()
	acc := &searchEvalAccumulator{}
	byFamily := map[string]*searchEvalAccumulator{}
	for _, judgment := range judgments.Queries {
		response, err := service.Search(ctx, search.SearchRequest{
			Query:           judgment.Query,
			Mode:            "auto",
			Limit:           20,
			IncludeEvidence: true,
			IncludeContext:  false,
		})
		if err != nil {
			t.Fatalf("query %s (%q): %v", judgment.ID, judgment.Query, err)
		}
		family := byFamily[judgment.Family]
		if family == nil {
			family = &searchEvalAccumulator{}
			byFamily[judgment.Family] = family
		}
		accumulateSearchEval(acc, family, judgment, response, ids, t)
	}

	actual := acc.metrics()
	for family, familyAcc := range byFamily {
		t.Logf("family=%s queries=%d metrics=%s", family, familyAcc.queries, formatSearchEvalMetrics(familyAcc.metrics()))
	}
	t.Logf("search relevance queries=%d intent-match=%d/%d metrics=%s", acc.queries, acc.intentCorrect, acc.queries, formatSearchEvalMetrics(actual))

	updateBaseline := os.Getenv("NEXUSSLATE_UPDATE_SEARCH_EVAL_BASELINE") == "1"
	if updateBaseline {
		baseline.Version = 1
		baseline.Runner = searchEvalRunner{
			Mode: "auto", Limit: 20, IncludeEvidence: true, IncludeContext: false,
			ProfileVersion: search.DefaultOptions().ProfileVersion, Provider: "offline",
		}
		baseline.Metrics = actual
		baseline.CorpusSHA256 = sha256Hex(corpusBytes)
		baseline.JudgmentsSHA256 = sha256Hex(judgmentsBytes)
		writeSearchEvalBaseline(t, filepath.Join(dataDir, "baseline.json"), baseline)
		t.Logf("updated search relevance baseline at %s", filepath.Join(dataDir, "baseline.json"))
		return
	}

	assertSearchEvalBaseline(t, baseline, actual)
	if hash := baseline.CorpusSHA256; hash != "" && hash != "pending" && hash != sha256Hex(corpusBytes) {
		t.Errorf("corpus hash changed: baseline=%s current=%s; regenerate baseline intentionally", hash, sha256Hex(corpusBytes))
	}
	if hash := baseline.JudgmentsSHA256; hash != "" && hash != "pending" && hash != sha256Hex(judgmentsBytes) {
		t.Errorf("judgments hash changed: baseline=%s current=%s; regenerate baseline intentionally", hash, sha256Hex(judgmentsBytes))
	}
}

func accumulateSearchEval(total, family *searchEvalAccumulator, judgment searchEvalQuery, response *search.SearchResponse, ids map[string]string, t *testing.T) {
	total.queries++
	family.queries++
	if response.Query.Intent == search.SearchIntent(judgment.Intent) {
		total.intentCorrect++
		family.intentCorrect++
	}
	if len(response.Results) == 0 {
		total.empty++
		family.empty++
	}
	if (judgment.ExpectedEmpty && len(response.Results) == 0) || (!judgment.ExpectedEmpty && len(response.Results) > 0) {
		total.expectedEmptyCorrect++
		family.expectedEmptyCorrect++
	}
	if judgment.ExpectedEmpty && len(response.Results) != 0 {
		t.Errorf("query %s (%q): expected an empty result, got %d", judgment.ID, judgment.Query, len(response.Results))
	}

	relevant := make(map[string]int, len(judgment.Relevant))
	for _, shot := range judgment.Relevant {
		relevant[ids[shot.ShotID]] = shot.Grade
	}
	if len(relevant) > 0 {
		total.rankQueries++
		family.rankQueries++
	}
	hits := 0
	for i, item := range response.Results {
		if i >= 10 {
			break
		}
		if relevant[item.ShotID] > 0 {
			hits++
		}
	}
	if len(relevant) > 0 {
		total.recall += recallAt(response.Results, relevant, 20)
		family.recall += recallAt(response.Results, relevant, 20)
	}
	// Precision uses a fixed denominator of ten. A short result list therefore
	// records missing tail results instead of making a tiny result look perfect.
	total.precision += float64(hits) / 10
	family.precision += float64(hits) / 10
	total.ndcg += ndcgAt(response.Results, relevant, 10, judgment.ExpectedEmpty)
	family.ndcg += ndcgAt(response.Results, relevant, 10, judgment.ExpectedEmpty)

	notRelevant := make(map[string]bool, len(judgment.NotRelevant))
	for _, key := range judgment.NotRelevant {
		notRelevant[ids[key]] = true
	}
	for _, item := range response.Results {
		if notRelevant[item.ShotID] {
			total.notRelevantHits++
			family.notRelevantHits++
			t.Logf("query=%s not_relevant_shot=%s", judgment.ID, stableShotKey(item, ids))
		}
	}

	actualByShot := make(map[string]search.ResultItem, len(response.Results))
	for _, item := range response.Results {
		actualByShot[item.ShotID] = item
	}
	for _, expected := range judgment.Evidence {
		total.evidenceChecks++
		family.evidenceChecks++
		item, ok := actualByShot[ids[expected.ShotID]]
		if ok && evidenceMatches(item.Evidence, expected) {
			total.evidenceHits++
			family.evidenceHits++
		} else if !ok {
			t.Logf("query=%s missing evidence shot=%s", judgment.ID, expected.ShotID)
		}
	}
	if len(judgment.Evidence) > 0 {
		for _, item := range response.Results {
			if resultSupportsMusts(judgment.Query, item) {
				total.evidenceSupported++
				family.evidenceSupported++
			}
			total.evidenceResults++
			family.evidenceResults++
		}
	}
}

func recallAt(results []search.ResultItem, relevant map[string]int, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	hits := 0
	seen := map[string]bool{}
	for i, item := range results {
		if i >= k {
			break
		}
		if relevant[item.ShotID] > 0 && !seen[item.ShotID] {
			hits++
			seen[item.ShotID] = true
		}
	}
	return float64(hits) / float64(len(relevant))
}

func ndcgAt(results []search.ResultItem, relevant map[string]int, k int, expectedEmpty bool) float64 {
	if len(relevant) == 0 {
		if expectedEmpty && len(results) == 0 {
			return 1
		}
		return 0
	}
	actual := 0.0
	for i, item := range results {
		if i >= k {
			break
		}
		if grade := relevant[item.ShotID]; grade > 0 {
			actual += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(i+2))
		}
	}
	grades := make([]int, 0, len(relevant))
	for _, grade := range relevant {
		grades = append(grades, grade)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(grades)))
	ideal := 0.0
	for i, grade := range grades {
		if i >= k {
			break
		}
		ideal += (math.Pow(2, float64(grade)) - 1) / math.Log2(float64(i+2))
	}
	if ideal == 0 {
		return 0
	}
	return actual / ideal
}

func evidenceMatches(actual []search.Evidence, expected searchEvalEvidence) bool {
	for _, evidence := range actual {
		if string(evidence.Constraint) != expected.Constraint || evidence.Value != expected.Value {
			continue
		}
		stateOK := len(expected.States) == 0
		for _, state := range expected.States {
			if string(evidence.State) == state {
				stateOK = true
				break
			}
		}
		if !stateOK {
			continue
		}
		sourceSet := make(map[string]bool, len(evidence.Sources))
		for _, source := range evidence.Sources {
			sourceSet[string(source)] = true
		}
		allSources := true
		for _, source := range expected.Sources {
			if !sourceSet[source] {
				allSources = false
				break
			}
		}
		if allSources {
			return true
		}
	}
	return false
}

// resultSupportsMusts checks only claims made by the compiled query. Unknown
// is deliberately not accepted here; this is the evidence-precision floor,
// while expected relevance remains a separate ranking judgment.
func resultSupportsMusts(raw string, item search.ResultItem) bool {
	q := search.Compile(raw)
	for _, must := range q.Must {
		found := false
		for _, evidence := range item.Evidence {
			if evidence.Negated || evidence.Value != must.Value {
				continue
			}
			if evidence.State == search.EvidenceConfirmed || evidence.State == search.EvidencePossible {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, mustNot := range q.MustNot {
		for _, evidence := range item.Evidence {
			if evidence.Negated && evidence.Value == mustNot.Value && (evidence.State == search.EvidenceConfirmed || evidence.State == search.EvidencePossible) {
				return false
			}
		}
	}
	return true
}

func stableShotKey(item search.ResultItem, ids map[string]string) string {
	for key, id := range ids {
		if id == item.ShotID {
			return key
		}
	}
	return fmt.Sprintf("%s:%d", item.AssetID, item.StartMS)
}

func validateSearchEvalFixtures(t *testing.T, corpus searchEvalCorpusFile, judgments searchEvalJudgmentsFile, baseline searchEvalBaselineFile) {
	t.Helper()
	if corpus.Version != 1 || corpus.IDFormat != "asset_id:ordinal" {
		t.Fatalf("unsupported search eval corpus metadata: version=%d id_format=%q", corpus.Version, corpus.IDFormat)
	}
	if judgments.Version != 1 || baseline.Version != 1 {
		t.Fatalf("unsupported search eval fixture version: judgments=%d baseline=%d", judgments.Version, baseline.Version)
	}
	manifest := make(map[string]searchEvalCorpusShot, len(corpus.Shots))
	for _, shot := range corpus.Shots {
		if shot.ShotID == "" || shot.AssetID == "" || shot.Ordinal < 0 {
			t.Fatalf("invalid corpus shot: %+v", shot)
		}
		if _, exists := manifest[shot.ShotID]; exists {
			t.Fatalf("duplicate corpus shot %q", shot.ShotID)
		}
		want := fmt.Sprintf("%s:%d", shot.AssetID, shot.Ordinal)
		if shot.ShotID != want {
			t.Fatalf("corpus shot %q does not match asset:ordinal %q", shot.ShotID, want)
		}
		manifest[shot.ShotID] = shot
	}
	seenQueries := map[string]bool{}
	languages := map[string]int{}
	for _, query := range judgments.Queries {
		if query.ID == "" || query.Query == "" || query.Family == "" || query.Language == "" || query.Intent == "" {
			t.Fatalf("invalid relevance judgment metadata: %+v", query)
		}
		if query.Language != "zh" && query.Language != "en" && query.Language != "mixed" {
			t.Fatalf("query %s has unsupported language %q", query.ID, query.Language)
		}
		languages[query.Language]++
		if seenQueries[query.ID] {
			t.Fatalf("duplicate relevance query %q", query.ID)
		}
		seenQueries[query.ID] = true
		if query.ExpectedEmpty && len(query.Relevant) != 0 {
			t.Fatalf("query %s is expected empty but has relevant shots", query.ID)
		}
		for _, relevant := range query.Relevant {
			if manifest[relevant.ShotID].ShotID == "" || relevant.Grade < 1 || relevant.Grade > 3 {
				t.Fatalf("query %s has invalid relevant judgment %+v", query.ID, relevant)
			}
		}
		for _, key := range query.NotRelevant {
			if manifest[key].ShotID == "" {
				t.Fatalf("query %s references unmanifested not_relevant shot %q", query.ID, key)
			}
		}
		for _, evidence := range query.Evidence {
			if manifest[evidence.ShotID].ShotID == "" || evidence.Constraint == "" || evidence.Value == "" || len(evidence.States) == 0 {
				t.Fatalf("query %s has invalid evidence judgment %+v", query.ID, evidence)
			}
		}
	}
	for _, language := range []string{"zh", "en", "mixed"} {
		if languages[language] == 0 {
			t.Errorf("search relevance corpus has no %s queries", language)
		}
	}
	wantRunner := searchEvalRunner{Mode: "auto", Limit: 20, IncludeEvidence: true, IncludeContext: false, ProfileVersion: search.DefaultOptions().ProfileVersion, Provider: "offline"}
	if baseline.Runner != wantRunner {
		t.Fatalf("baseline runner changed: got %+v want %+v", baseline.Runner, wantRunner)
	}
}

func assertSearchEvalBaseline(t *testing.T, baseline searchEvalBaselineFile, actual searchEvalMetrics) {
	t.Helper()
	const tolerance = 0.02
	for _, metric := range []struct {
		name      string
		got, want float64
	}{
		{"Recall@20", actual.RecallAt20, baseline.Metrics.RecallAt20},
		{"Precision@10", actual.PrecisionAt10, baseline.Metrics.PrecisionAt10},
		{"nDCG@10", actual.NDCGAt10, baseline.Metrics.NDCGAt10},
	} {
		if metric.got+tolerance < metric.want {
			t.Errorf("%s regressed: got %.4f baseline %.4f tolerance %.2f", metric.name, metric.got, metric.want, tolerance)
		}
	}
	for _, metric := range []struct {
		name      string
		got, want float64
	}{
		{"empty-result rate", actual.EmptyResultRate, baseline.Metrics.EmptyResultRate},
		{"expected-empty accuracy", actual.ExpectedEmptyAccuracy, baseline.Metrics.ExpectedEmptyAccuracy},
		{"intent accuracy", actual.IntentAccuracy, baseline.Metrics.IntentAccuracy},
		{"evidence correctness", actual.EvidenceCorrectness, baseline.Metrics.EvidenceCorrectness},
		{"evidence precision", actual.EvidencePrecision, baseline.Metrics.EvidencePrecision},
	} {
		if metric.got+1e-9 < metric.want {
			t.Errorf("%s regressed: got %.4f baseline %.4f", metric.name, metric.got, metric.want)
		}
	}
	if actual.EmptyResultRate > baseline.Metrics.EmptyResultRate+1e-9 {
		t.Errorf("empty-result rate regressed upward: got %.4f baseline %.4f", actual.EmptyResultRate, baseline.Metrics.EmptyResultRate)
	}
	if actual.NotRelevantHits > baseline.Metrics.NotRelevantHits {
		t.Errorf("hard-negative hits regressed: got %d baseline %d", actual.NotRelevantHits, baseline.Metrics.NotRelevantHits)
	}
}

func formatSearchEvalMetrics(m searchEvalMetrics) string {
	return fmt.Sprintf("R@20=%.3f P@10=%.3f nDCG@10=%.3f empty=%.3f expected-empty=%.3f intent=%.3f hard-negative-hits=%d evidence-correct=%.3f evidence-precision=%.3f", m.RecallAt20, m.PrecisionAt10, m.NDCGAt10, m.EmptyResultRate, m.ExpectedEmptyAccuracy, m.IntentAccuracy, m.NotRelevantHits, m.EvidenceCorrectness, m.EvidencePrecision)
}

func searchEvalDataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../testdata/search_eval"))
}

func readSearchEvalJSON[T any](t *testing.T, path string) (T, []byte) {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value T
	if err := json.Unmarshal(bytes, &value); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return value, bytes
}

func readSearchEvalJSONFile[T any](t *testing.T, path string) T {
	t.Helper()
	value, _ := readSearchEvalJSON[T](t, path)
	return value
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeSearchEvalBaseline(t *testing.T, path string, baseline searchEvalBaselineFile) {
	t.Helper()
	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		t.Fatalf("marshal search eval baseline: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write search eval baseline: %v", err)
	}
}
