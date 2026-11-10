package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// modalityBundle is everything one modality channel produced: the candidates
// fusion consumes, the windows the evidence layer reports, and the coverage
// statement the response must not overstate.
type modalityBundle struct {
	signal     string
	candidates []Candidate
	windows    []ModalityWindow
	coverage   Coverage
}

// retrieveModalities runs every requested new channel. It never invents a
// channel: a requested modality without a store or an encoder is an error the
// caller can report, not a silently narrower result set.
func (s *Service) retrieveModalities(ctx context.Context, req SearchRequest, q SearchQuery, limit int, universe map[string]bool, universeReady bool) ([]modalityBundle, error) {
	if !hasMultimodalRequest(req) {
		return nil, nil
	}
	if s.opts.ModalityStore == nil {
		return nil, fmt.Errorf("%w: no multimodal store is configured", ErrModalitiesUnavailable)
	}
	wanted := requestedModalities(req)
	if len(wanted) == 0 && req.Temporal == nil && req.SpeakerFilter == nil {
		return nil, nil
	}
	bundles := make([]modalityBundle, 0, len(wanted))
	for _, modality := range wanted {
		bundle, err := s.retrieveModality(ctx, modality, req, q, limit, universe, universeReady)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}
	return bundles, nil
}

func requestedModalities(req SearchRequest) []string {
	seen := make(map[string]bool, len(req.Modalities))
	out := make([]string, 0, len(req.Modalities))
	for _, modality := range req.Modalities {
		modality = strings.TrimSpace(modality)
		switch modality {
		case ModalityVisual, ModalityAudio, ModalitySoundEvent, ModalitySpeaker:
		default:
			// lexical/transcript are legacy channels; selecting them adds
			// nothing here.
			continue
		}
		if seen[modality] {
			continue
		}
		seen[modality] = true
		out = append(out, modality)
	}
	sort.Strings(out)
	return out
}

func (s *Service) retrieveModality(ctx context.Context, modality string, req SearchRequest, q SearchQuery, limit int, universe map[string]bool, universeReady bool) (modalityBundle, error) {
	retriever := &ModalityRetriever{Store: s.opts.ModalityStore, Modality: modality, Universe: universe, UniverseReady: universeReady}
	switch modality {
	case ModalityVisual, ModalityAudio:
		if s.opts.ModalityEncoder == nil {
			return modalityBundle{}, fmt.Errorf("%w: %s retrieval needs a query encoder", ErrModalitiesUnavailable, modality)
		}
		spaceID, vector, err := s.opts.ModalityEncoder.Encode(ctx, modality, q.Raw, req.ReferenceID)
		if err != nil {
			// Preserve a typed "pending with job ids" error; only a real
			// unavailability is wrapped as one.
			var pending *PendingModalitiesError
			if errors.As(err, &pending) {
				return modalityBundle{}, fmt.Errorf("%s: %w", modality, err)
			}
			return modalityBundle{}, fmt.Errorf("%w: %s: %v", ErrModalitiesUnavailable, modality, err)
		}
		retriever.SpaceID = spaceID
		retriever.Vector = vector
	case ModalitySoundEvent:
		retriever.SoundLabels = soundEventLabels(q.Raw, nil)
		if len(retriever.SoundLabels) == 0 {
			return modalityBundle{}, fmt.Errorf("%w: sound_event needs a recognised sound word in the query", ErrModalitiesUnavailable)
		}
	case ModalitySpeaker:
		if req.SpeakerFilter == nil {
			return modalityBundle{}, fmt.Errorf("%w: speaker retrieval needs a speaker_filter", ErrModalitiesUnavailable)
		}
		retriever.SpeakerFilter = *req.SpeakerFilter
	}
	candidates, windows, err := retriever.RetrieveWithWindows(ctx, q, limit)
	if err != nil {
		return modalityBundle{}, err
	}
	state := "complete"
	if len(candidates) >= limit {
		// The channel filled its pool, which means more eligible rows may
		// exist. Saying "partial" here is honest; saying "complete" would
		// turn a truncated list into a claim about the whole library.
		state = "partial"
	}
	intervals := make([]Interval, 0, len(windows))
	for _, window := range windows {
		intervals = append(intervals, Interval{StartMS: window.StartMS, EndMS: window.EndMS})
	}
	return modalityBundle{
		signal:     modality,
		candidates: candidates,
		windows:    windows,
		coverage:   Coverage{Modality: modality, State: state, Intervals: normalizeCoverageIntervals(intervals)},
	}, nil
}

// temporalEvidence evaluates an explicit temporal query at asset level: every
// predicate must have at least one satisfying pair inside the asset. It returns
// the allowed assets and the supporting windows, or a typed error when a
// condition cannot be retrieved.
func (s *Service) temporalEvidence(ctx context.Context, req SearchRequest, q SearchQuery, limit int, universe map[string]bool, universeReady bool) (map[string]bool, []Match, error) {
	if req.Temporal == nil {
		return nil, nil, nil
	}
	query := *req.Temporal
	if len(query.Conditions) == 0 {
		return nil, nil, fmt.Errorf("%w: temporal query has no conditions", ErrInvalidMultimodal)
	}
	if s.opts.ModalityStore == nil {
		return nil, nil, fmt.Errorf("%w: temporal retrieval needs a multimodal store", ErrModalitiesUnavailable)
	}
	windowsByCondition := make(map[string][]ModalityWindow, len(query.Conditions))
	for _, condition := range query.Conditions {
		retriever := &ModalityRetriever{Store: s.opts.ModalityStore, Modality: condition.Modality, Universe: universe, UniverseReady: universeReady}
		switch condition.Modality {
		case ModalityVisual, ModalityAudio:
			if s.opts.ModalityEncoder == nil {
				return nil, nil, fmt.Errorf("%w: temporal condition %q needs a query encoder", ErrModalitiesUnavailable, condition.ID)
			}
			spaceID, vector, err := s.opts.ModalityEncoder.Encode(ctx, condition.Modality, condition.Query, req.ReferenceID)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: temporal condition %q: %v", ErrModalitiesUnavailable, condition.ID, err)
			}
			retriever.SpaceID = spaceID
			retriever.Vector = vector
		case ModalitySoundEvent:
			retriever.SoundLabels = soundEventLabels(condition.Query, nil)
			if len(retriever.SoundLabels) == 0 {
				return nil, nil, fmt.Errorf("%w: temporal condition %q names no recognised sound", ErrModalitiesUnavailable, condition.ID)
			}
		case ModalitySpeaker:
			if req.SpeakerFilter == nil {
				return nil, nil, fmt.Errorf("%w: temporal condition %q needs a speaker filter", ErrModalitiesUnavailable, condition.ID)
			}
			retriever.SpeakerFilter = *req.SpeakerFilter
		default:
			return nil, nil, fmt.Errorf("%w: temporal condition %q uses unsupported modality %q", ErrInvalidMultimodal, condition.ID, condition.Modality)
		}
		// Predicates must see the complete condition set: a supporting pair
		// beyond the response top-K is still a valid temporal fact. The final
		// matches are truncated to limit after the asset-level join.
		_, windows, err := retriever.RetrieveWithWindows(ctx, q, -1)
		if err != nil {
			return nil, nil, err
		}
		windowsByCondition[condition.ID] = windows
	}

	// Every condition is conjunctive: an asset must contain a match for all of
	// them, even when no predicate references a condition. Start from the
	// intersection of the per-condition asset sets, then require every
	// predicate to hold in the same asset.
	var allowed map[string]bool
	for _, condition := range query.Conditions {
		conditionAssets := make(map[string]bool)
		for _, window := range windowsByCondition[condition.ID] {
			conditionAssets[window.AssetID] = true
		}
		if allowed == nil {
			allowed = conditionAssets
			continue
		}
		for assetID := range allowed {
			if !conditionAssets[assetID] {
				delete(allowed, assetID)
			}
		}
	}
	for _, predicate := range query.Predicates {
		satisfied := map[string]bool{}
		for _, left := range windowsByCondition[predicate.LeftID] {
			for _, right := range windowsByCondition[predicate.RightID] {
				if left.AssetID != right.AssetID {
					continue
				}
				leftInterval := Interval{StartMS: left.StartMS, EndMS: left.EndMS}
				rightInterval := Interval{StartMS: right.StartMS, EndMS: right.EndMS}
				ok, err := EvaluatePredicate(predicate.Relation, leftInterval, rightInterval, predicate.MaxGapMS)
				if err != nil {
					return nil, nil, fmt.Errorf("%w: %v", ErrInvalidMultimodal, err)
				}
				if ok {
					satisfied[left.AssetID] = true
				}
			}
		}
		if allowed == nil {
			allowed = satisfied
			continue
		}
		for assetID := range allowed {
			if !satisfied[assetID] {
				delete(allowed, assetID)
			}
		}
	}

	var matches []Match
	for _, condition := range query.Conditions {
		perCondition := 0
		for _, window := range windowsByCondition[condition.ID] {
			if allowed != nil && !allowed[window.AssetID] {
				continue
			}
			if limit > 0 && perCondition >= limit {
				break
			}
			matches = append(matches, BuildMatches(condition.ID, []ModalityWindow{window})...)
			perCondition++
		}
	}
	return allowed, dedupeMatches(matches), nil
}

// dedupeMatches keeps the highest-score record per (condition, asset, interval)
// and sorts deterministically for the response.
func dedupeMatches(matches []Match) []Match {
	best := make(map[string]Match, len(matches))
	for _, match := range matches {
		key := fmt.Sprintf("%s|%s|%d|%d", match.ConditionID, match.AssetID, match.StartMS, match.EndMS)
		existing, ok := best[key]
		if !ok || match.RetrievalScore > existing.RetrievalScore {
			best[key] = match
		}
	}
	out := make([]Match, 0, len(best))
	for _, match := range best {
		out = append(out, match)
	}
	sort.Slice(out, func(i, j int) bool { return matchLess(out[i], out[j]) })
	return out
}

// matchesForCandidate collects the windows that support one result. Temporal
// matches are already condition-keyed; modality matches use the channel name as
// the condition id. Both are deduplicated so a frame and its pooled shot record
// do not appear as two identical claims.
func matchesForCandidate(candidate Candidate, bundles []modalityBundle, temporal []Match) []Match {
	var out []Match
	for _, match := range temporal {
		if match.AssetID == candidate.AssetID {
			out = append(out, match)
		}
	}
	for _, bundle := range bundles {
		for _, window := range bundle.windows {
			if window.ShotID != candidate.ShotID {
				continue
			}
			out = append(out, BuildMatches(bundle.signal, []ModalityWindow{window})...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return dedupeMatches(out)
}

// coverageForBundles reports what each requested channel actually achieved.
func coverageForBundles(bundles []modalityBundle) []Coverage {
	if len(bundles) == 0 {
		return nil
	}
	out := make([]Coverage, 0, len(bundles))
	for _, bundle := range bundles {
		out = append(out, bundle.coverage)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modality < out[j].Modality })
	return out
}

// normalizeCoverageIntervals sorts and merges overlapping or touching spans so
// a coverage statement counts each millisecond once.
func normalizeCoverageIntervals(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].StartMS != in[j].StartMS {
			return in[i].StartMS < in[j].StartMS
		}
		return in[i].EndMS < in[j].EndMS
	})
	out := []Interval{in[0]}
	for _, interval := range in[1:] {
		last := &out[len(out)-1]
		if interval.StartMS <= last.EndMS {
			if interval.EndMS > last.EndMS {
				last.EndMS = interval.EndMS
			}
			continue
		}
		out = append(out, interval)
	}
	return out
}
