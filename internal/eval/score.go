package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// RunScore is the retrieval outcome of one run: Precision@5/@10, Recall@10
// and the false-positive count, plus the throughput numbers from its report.
type RunScore struct {
	Label           string        `json:"label"`
	Provider        string        `json:"provider"`
	Model           string        `json:"model"`
	Precision5      float64       `json:"precision_5"`
	Precision10     float64       `json:"precision_10"`
	Recall10        float64       `json:"recall_10"`
	FalsePositives  int           `json:"false_positives"`
	RelevantInTop10 int           `json:"relevant_in_top_10"`
	TotalQueries    int           `json:"total_queries"`
	MeanRTFactor    float64       `json:"mean_real_time_factor"`
	Frames          int           `json:"frames_processed"`
	QueryDetail     []QueryDetail `json:"queries,omitempty"`
}

// QueryDetail is one query's verdict, so an operator can see exactly which
// footage a model lost or invented.
type QueryDetail struct {
	Query          string   `json:"query"`
	HitRelevant    int      `json:"hit_relevant"`
	TotalRelevant  int      `json:"total_relevant"`
	FalsePositives int      `json:"false_positives"`
	Missed         []string `json:"missed,omitempty"`
	FPs            []string `json:"false_positives_ids,omitempty"`
}

// Score evaluates one run's database against the corpus ground truth using
// the same hybrid retrieval the product serves (default weights), and the
// same metric conventions as the retrieval golden set: a fixed K denominator,
// a relevant shot must actually appear in the top-10.
func Score(ctx context.Context, dataDir, label string, corpus *Corpus) (*RunScore, error) {
	report, err := LoadReport(dataDir, label)
	if err != nil {
		return nil, err
	}
	repo, err := sqlite.Open(filepath.Join(dataDir, "timingdex.db"))
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		return nil, err
	}

	score := &RunScore{
		Label: label, Provider: report.Provider, Model: report.Model,
		TotalQueries: len(corpus.Queries),
	}
	var rtTotal float64
	for _, asset := range report.Assets {
		rtTotal += asset.RealTimeFactor
		score.Frames += asset.FramesProcessed
	}
	if len(report.Assets) > 0 {
		score.MeanRTFactor = rtTotal / float64(len(report.Assets))
	}

	assetByClip := make(map[string]string, len(report.Assets))
	for _, asset := range report.Assets {
		assetByClip[asset.Clip] = asset.AssetID
	}
	for _, query := range corpus.Queries {
		results, err := repo.HybridSearchShots(ctx, query.Query, 10)
		if err != nil {
			return nil, err
		}
		relevant := map[string]bool{}
		missed := []string{}
		for _, expected := range query.Expected {
			assetID, ok := assetByClip[expected.Asset]
			if !ok {
				return nil, fmt.Errorf("query %q references clip %q that the run does not contain", query.Query, expected.Asset)
			}
			hit, err := findExpectedShot(ctx, repo, assetID, expected)
			if err != nil {
				return nil, err
			}
			if hit != "" {
				relevant[hit] = true
			} else {
				missed = append(missed, expected.Asset)
			}
		}
		detail := QueryDetail{Query: query.Query, TotalRelevant: len(relevant)}
		hitCount := 0
		for _, r := range results {
			if relevant[r.ID] {
				hitCount++
			} else {
				score.FalsePositives++
				detail.FalsePositives++
				detail.FPs = append(detail.FPs, r.ID)
			}
		}
		detail.HitRelevant = hitCount
		detail.Missed = missed
		p5, p10, r10 := metrics(results, relevant, len(relevant))
		score.Precision5 += p5
		score.Precision10 += p10
		score.Recall10 += r10
		score.RelevantInTop10 += hitCount
		score.QueryDetail = append(score.QueryDetail, detail)
	}
	n := float64(len(corpus.Queries))
	score.Precision5 /= n
	score.Precision10 /= n
	score.Recall10 /= n
	return score, nil
}

// findExpectedShot locates the canonical shot that matches a ground-truth
// span inside one asset: the shot with the largest overlap, requiring at
// least half the expected span. An expected span with no overlapping shot is
// a corpus gap — the footage says one thing, the shots say another — and
// scores as a miss.
func findExpectedShot(ctx context.Context, repo *sqlite.Repository, assetID string, expected ExpectedShot) (string, error) {
	shots, err := repo.ListAssetShots(ctx, assetID)
	if err != nil {
		return "", err
	}
	best := ""
	bestOverlap := int64(0)
	for _, shot := range shots {
		overlap := min64(shot.EndMS, expected.EndMS) - max64(shot.StartMS, expected.StartMS)
		if overlap > bestOverlap {
			bestOverlap = overlap
			best = shot.ID
		}
	}
	if best != "" && bestOverlap*2 >= (expected.EndMS-expected.StartMS) {
		return best, nil
	}
	return "", nil
}

// metrics mirrors the golden set's conventions: P@K uses the fixed K
// denominator even when the corpus returns fewer rows.
func metrics(results []domain.ShotSearchResult, relevant map[string]bool, totalRelevant int) (p5, p10, r10 float64) {
	hit := func(k int) int {
		count := 0
		for i, r := range results {
			if i >= k {
				break
			}
			if relevant[r.ID] {
				count++
			}
		}
		return count
	}
	p5 = float64(hit(5)) / 5.0
	p10 = float64(hit(10)) / 10.0
	if totalRelevant > 0 {
		r10 = float64(hit(10)) / float64(totalRelevant)
	}
	return p5, p10, r10
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// SaveScore persists the scored run for later comparison.
func SaveScore(score *RunScore, dataDir string) error {
	raw, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, "runs", score.Label, "score.json"), raw, 0o600)
}
