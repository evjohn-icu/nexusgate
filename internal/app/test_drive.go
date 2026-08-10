package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrTestDriveInvalid is the single identity for a Test Drive request that
// cannot mean what it asks: an empty batch, a batch above the cap, or an
// asset that does not exist. One identity for all three keeps the API's
// status choice (400) structural — the classifier never reads the message —
// while the wrapped text names the offending value for the operator.
var ErrTestDriveInvalid = errors.New("invalid test drive request")

// maxTestDriveAssets caps a Test Drive batch. The feature exists so an
// operator can feel a provider out on a handful of clips before committing
// the library; a batch this size is a first look, not a scan substitute —
// root scans and reanalysis are the paths that exist for the library.
const maxTestDriveAssets = 3

// defaultTestDriveSuggestions is how many suggested query strings
// TestDriveSuggestions returns when the caller does not ask for a specific
// number.
const defaultTestDriveSuggestions = 5

// TestDriveResult reports what a Test Drive batch did with the queue.
type TestDriveResult struct {
	Enqueued        int  `json:"enqueued"`
	AlreadyAnalyzed int  `json:"already_analyzed"`
	Ran             bool `json:"ran"`
}

// TestDrive enqueues a small first batch of assets through the pipeline and
// runs one pass so the operator has committed analysis to look at. An asset
// that already has committed shots is skipped rather than re-enqueued, which
// is what makes the batch idempotent: the same call twice leaves the queue
// unchanged. The pipeline run is TryRunPipeline — the same path the
// progress-page "run" button uses — so it honours the single-pass guard and
// reports whether the pass actually ran.
func (s *Service) TestDrive(ctx context.Context, assetIDs []string) (TestDriveResult, error) {
	var result TestDriveResult
	if len(assetIDs) == 0 {
		return result, fmt.Errorf("%w: select at least one asset", ErrTestDriveInvalid)
	}
	if len(assetIDs) > maxTestDriveAssets {
		return result, fmt.Errorf("%w: test drive accepts at most %d assets, got %d", ErrTestDriveInvalid, maxTestDriveAssets, len(assetIDs))
	}
	seen := make(map[string]struct{}, len(assetIDs))
	for _, id := range assetIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		asset, err := s.repo.GetAssetDetail(ctx, id)
		if err != nil {
			return result, err
		}
		if asset == nil {
			return result, fmt.Errorf("%w: asset %s not found", ErrTestDriveInvalid, id)
		}
		shots, err := s.repo.ListAssetShots(ctx, id)
		if err != nil {
			return result, err
		}
		if len(shots) > 0 {
			result.AlreadyAnalyzed++
			continue
		}
		if err := s.pipeline.EnqueueAsset(ctx, id); err != nil {
			return result, fmt.Errorf("enqueue test drive asset %s: %w", id, err)
		}
		result.Enqueued++
	}
	ran, err := s.TryRunPipeline(ctx)
	if err != nil {
		return result, err
	}
	result.Ran = ran
	return result, nil
}

// TestDriveSuggestions distils the given assets' committed analysis into
// ready-made query strings: every distinct object, action and tag appearing
// in any committed shot is counted, and the most frequent terms are returned
// in deterministic order (count descending, then term ascending) so the same
// library always yields the same suggestions. The canonical English terms are
// exactly what the search vocabulary accepts, so a suggestion can be passed
// straight to a search. A missing asset simply contributes no shots.
func (s *Service) TestDriveSuggestions(ctx context.Context, assetIDs []string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = defaultTestDriveSuggestions
	}
	counts := make(map[string]int)
	for _, id := range assetIDs {
		shots, err := s.repo.ListAssetShots(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, shot := range shots {
			// A shot contributes a term at most once; the unit of frequency is
			// "how many committed shots carry this term", not raw list length,
			// so a duplicated entry inside one shot cannot inflate its count.
			seen := make(map[string]struct{}, len(shot.Objects)+len(shot.Actions)+len(shot.Tags))
			for _, term := range append(append(shot.Objects, shot.Actions...), shot.Tags...) {
				if term = strings.TrimSpace(term); term == "" {
					continue
				}
				if _, dup := seen[term]; dup {
					continue
				}
				seen[term] = struct{}{}
				counts[term]++
			}
		}
	}
	terms := make([]string, 0, len(counts))
	for term := range counts {
		terms = append(terms, term)
	}
	sort.Slice(terms, func(i, j int) bool {
		if counts[terms[i]] != counts[terms[j]] {
			return counts[terms[i]] > counts[terms[j]]
		}
		return terms[i] < terms[j]
	})
	if len(terms) > limit {
		terms = terms[:limit]
	}
	return terms, nil
}
