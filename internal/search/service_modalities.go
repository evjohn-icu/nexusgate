package search

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// modalityBundle is everything one modality channel produced: the candidates
// fusion consumes, the windows the evidence layer reports, and the coverage
// statement the response must not overstate. The speaker bundle additionally
// carries the complete eligible-shot allowlist its filter demands.
type modalityBundle struct {
	signal     string
	candidates []Candidate
	windows    []ModalityWindow
	coverage   Coverage
	// truncated reports honest storage truncation of the window pool: the
	// store returned its limit+1th row. A single shot with many matching
	// windows still counts, so coverage can never call a cut pool complete.
	truncated bool
	// allowlist is the complete set of eligible shots the speaker channel
	// matched (nontruncated matching), keyed by shot id. Every fused result
	// must be restricted to it, even when the speaker_filter was the only
	// selector in the request.
	allowlist map[string]bool
}

// retrieveModalities runs every requested new channel. It never invents a
// channel: a requested modality without a store or an encoder is an error the
// caller can report, not a silently narrower result set. A speaker_filter by
// itself implicitly requests the speaker channel, because its allowlist is
// what restricts the other channels.
func (s *Service) retrieveModalities(ctx context.Context, req SearchRequest, q SearchQuery, limit int, universe map[string]bool, universeReady bool) ([]modalityBundle, error) {
	if !hasMultimodalRequest(req) {
		return nil, nil
	}
	if s.opts.ModalityStore == nil {
		return nil, fmt.Errorf("%w: no multimodal store is configured", ErrModalitiesUnavailable)
	}
	wanted := requestedModalities(req)
	if req.SpeakerFilter != nil && !slices.Contains(wanted, ModalitySpeaker) {
		wanted = append(wanted, ModalitySpeaker)
		sort.Strings(wanted)
	}
	if len(wanted) == 0 && req.Temporal == nil {
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
		// The speaker channel is the filter channel: it must produce the
		// complete eligible-shot set, so a speaker_filter restricts every
		// fused result instead of only the speaker channel's own top-K.
		// Nothing is truncated, so the coverage statement stays complete.
		limit = -1
	}
	candidates, windows, truncated, err := retriever.RetrieveWithWindows(ctx, q, limit)
	if err != nil {
		return modalityBundle{}, err
	}
	state := "complete"
	if truncated {
		// The channel's storage pool was cut, which means more eligible rows
		// may exist. Saying "partial" here is honest; saying "complete" would
		// turn a truncated list into a claim about the whole library. This
		// counts even when one shot occupied every row.
		state = "partial"
	}
	intervals := make([]Interval, 0, len(windows))
	for _, window := range windows {
		intervals = append(intervals, Interval{StartMS: window.StartMS, EndMS: window.EndMS})
	}
	bundle := modalityBundle{
		signal:     modality,
		candidates: candidates,
		windows:    windows,
		coverage:   Coverage{Modality: modality, State: state, Intervals: normalizeCoverageIntervals(intervals)},
		truncated:  truncated,
	}
	if modality == ModalitySpeaker {
		bundle.allowlist = make(map[string]bool, len(candidates))
		for _, candidate := range candidates {
			bundle.allowlist[candidate.ShotID] = true
		}
	}
	return bundle, nil
}

// speakerAllowlist returns the complete eligible-shot set the speaker channel
// matched, or nil when no speaker bundle ran. The caller intersects every
// fused result with it so a speaker_filter — even a filter-only request —
// restricts all channels, not just the speaker channel itself.
func speakerAllowlist(bundles []modalityBundle) map[string]bool {
	for _, bundle := range bundles {
		if bundle.signal == ModalitySpeaker {
			return bundle.allowlist
		}
	}
	return nil
}

// temporalEvidence evaluates an explicit temporal query at asset level: every
// predicate must have at least one satisfying pair inside the asset. It
// returns the allowed assets, the supporting eligible shots of those assets
// (so a compound query whose raw text matches no single shot can still return
// the temporal evidence in the candidate pool) and the supporting matches, or
// a typed error when a condition cannot be retrieved.
func (s *Service) temporalEvidence(ctx context.Context, req SearchRequest, q SearchQuery, limit int, universe map[string]bool, universeReady bool) (map[string]bool, []Candidate, []Match, error) {
	if req.Temporal == nil {
		return nil, nil, nil, nil
	}
	query := *req.Temporal
	if len(query.Conditions) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: temporal query has no conditions", ErrInvalidMultimodal)
	}
	windowsByCondition := make(map[string][]ModalityWindow, len(query.Conditions))
	candidatesByCondition := make(map[string][]Candidate, len(query.Conditions))
	for _, condition := range query.Conditions {
		windows, candidates, err := s.temporalConditionWindows(ctx, req, q, condition, universe, universeReady)
		if err != nil {
			return nil, nil, nil, err
		}
		windowsByCondition[condition.ID] = windows
		candidatesByCondition[condition.ID] = candidates
	}

	// Every condition is conjunctive: an asset must contain a match for all of
	// them, even when no predicate references a condition. Start from the
	// intersection of the per-condition asset sets, then require every
	// predicate to hold in the same asset. Windows never match across assets.
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
				// The original full record interval is what temporal checks
				// evaluate; repeated (record, shot) join copies of one window
				// dedupe again by record id when the matches are built.
				leftInterval := Interval{StartMS: left.StartMS, EndMS: left.EndMS}
				rightInterval := Interval{StartMS: right.StartMS, EndMS: right.EndMS}
				ok, err := EvaluatePredicate(predicate.Relation, leftInterval, rightInterval, predicate.MaxGapMS)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("%w: %v", ErrInvalidMultimodal, err)
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
	seenShots := make(map[string]bool)
	var supporting []Candidate
	for _, condition := range query.Conditions {
		var allowedWindows []ModalityWindow
		for _, window := range windowsByCondition[condition.ID] {
			if allowed != nil && !allowed[window.AssetID] {
				continue
			}
			allowedWindows = append(allowedWindows, window)
		}
		conditionMatches := BuildMatches(condition.ID, allowedWindows)
		if limit > 0 && len(conditionMatches) > limit {
			conditionMatches = conditionMatches[:limit]
		}
		matches = append(matches, conditionMatches...)
		for _, candidate := range candidatesByCondition[condition.ID] {
			if allowed != nil && !allowed[candidate.AssetID] {
				continue
			}
			if seenShots[candidate.ShotID] {
				continue
			}
			seenShots[candidate.ShotID] = true
			supporting = append(supporting, candidate)
		}
	}
	sortCandidates(supporting)
	return allowed, supporting, dedupeMatches(matches), nil
}

// temporalConditionWindows retrieves one condition's complete matching window
// set with the condition's own query — never the request's raw text — and the
// candidates that carry them. Predicates must see the complete condition set:
// a supporting pair beyond the response top-K is still a valid temporal fact.
func (s *Service) temporalConditionWindows(ctx context.Context, req SearchRequest, q SearchQuery, condition TemporalCondition, universe map[string]bool, universeReady bool) ([]ModalityWindow, []Candidate, error) {
	conditionQuery := SearchQuery{Raw: condition.Query}
	switch condition.Modality {
	case ModalityLexical:
		// Lexical windows are the matched shots' canonical intervals. A
		// lexical hit is shot-level text evidence, not a timed observation,
		// so it reports "possible".
		candidates, err := NewLexicalRetriever(s.store, Profiles()[q.Intent].FieldWeights).Retrieve(ctx, conditionQuery, -1)
		if err != nil {
			return nil, nil, err
		}
		candidates = filterUniverseCandidates(candidates, universe, universeReady)
		windows := make([]ModalityWindow, 0, len(candidates))
		for _, candidate := range candidates {
			windows = append(windows, ModalityWindow{
				AssetID:        candidate.AssetID,
				ShotID:         candidate.ShotID,
				RecordID:       "lexical-" + candidate.ShotID,
				Modality:       ModalityLexical,
				StartMS:        candidate.StartMS,
				EndMS:          candidate.EndMS,
				ShotStartMS:    candidate.StartMS,
				ShotEndMS:      candidate.EndMS,
				Score:          candidate.Signals[SignalLexical],
				EvidenceStatus: string(EvidenceStatusPossible),
			})
		}
		return windows, candidates, nil
	case ModalityTranscript:
		candidates, err := NewTranscriptRetriever(s.store).Retrieve(ctx, conditionQuery, -1)
		if err != nil {
			return nil, nil, err
		}
		candidates = filterUniverseCandidates(candidates, universe, universeReady)
		requests := make([]TranscriptSpanRequest, 0, len(candidates))
		for _, candidate := range candidates {
			requests = append(requests, TranscriptSpanRequest{
				ShotID:  candidate.ShotID,
				AssetID: candidate.AssetID,
				StartMS: candidate.StartMS,
				EndMS:   candidate.EndMS,
			})
		}
		spansByID, err := s.store.ShotTranscriptSpansBatch(ctx, requests)
		if err != nil {
			return nil, nil, err
		}
		var windows []ModalityWindow
		for _, candidate := range candidates {
			score := candidate.Signals[SignalTranscript]
			for _, interval := range speechMatchIntervals(condition.Query, spansByID[candidate.ShotID]) {
				windows = append(windows, ModalityWindow{
					AssetID:  candidate.AssetID,
					ShotID:   candidate.ShotID,
					RecordID: fmt.Sprintf("transcript-%s-%d-%d", candidate.ShotID, interval.StartMS, interval.EndMS),
					Modality: ModalityTranscript,
					// Only the matching located spans are evidence; the rest
					// of the shot's transcript is not part of this claim.
					StartMS:        interval.StartMS,
					EndMS:          interval.EndMS,
					ShotStartMS:    candidate.StartMS,
					ShotEndMS:      candidate.EndMS,
					Score:          score,
					EvidenceStatus: string(EvidenceStatusConfirmed),
				})
			}
		}
		return windows, candidates, nil
	case ModalityVisual, ModalityAudio:
		if s.opts.ModalityStore == nil {
			return nil, nil, fmt.Errorf("%w: temporal retrieval needs a multimodal store", ErrModalitiesUnavailable)
		}
		if s.opts.ModalityEncoder == nil {
			return nil, nil, fmt.Errorf("%w: temporal condition %q needs a query encoder", ErrModalitiesUnavailable, condition.ID)
		}
		spaceID, vector, err := s.opts.ModalityEncoder.Encode(ctx, condition.Modality, condition.Query, req.ReferenceID)
		if err != nil {
			// A pending encode of the condition's query text must surface as
			// "wait", not as "unavailable".
			var pending *PendingModalitiesError
			if errors.As(err, &pending) {
				return nil, nil, fmt.Errorf("%s: %w", condition.Modality, err)
			}
			return nil, nil, fmt.Errorf("%w: temporal condition %q: %v", ErrModalitiesUnavailable, condition.ID, err)
		}
		retriever := &ModalityRetriever{Store: s.opts.ModalityStore, Modality: condition.Modality, SpaceID: spaceID, Vector: vector, Universe: universe, UniverseReady: universeReady}
		candidates, windows, _, err := retriever.RetrieveWithWindows(ctx, conditionQuery, -1)
		return windows, candidates, err
	case ModalitySoundEvent:
		if s.opts.ModalityStore == nil {
			return nil, nil, fmt.Errorf("%w: temporal retrieval needs a multimodal store", ErrModalitiesUnavailable)
		}
		retriever := &ModalityRetriever{Store: s.opts.ModalityStore, Modality: condition.Modality, Universe: universe, UniverseReady: universeReady}
		candidates, windows, _, err := retriever.RetrieveWithWindows(ctx, conditionQuery, -1)
		return windows, candidates, err
	case ModalitySpeaker:
		if s.opts.ModalityStore == nil {
			return nil, nil, fmt.Errorf("%w: temporal retrieval needs a multimodal store", ErrModalitiesUnavailable)
		}
		if req.SpeakerFilter == nil {
			return nil, nil, fmt.Errorf("%w: temporal condition %q needs a speaker filter", ErrModalitiesUnavailable, condition.ID)
		}
		retriever := &ModalityRetriever{Store: s.opts.ModalityStore, Modality: condition.Modality, SpeakerFilter: *req.SpeakerFilter, Universe: universe, UniverseReady: universeReady}
		candidates, windows, _, err := retriever.RetrieveWithWindows(ctx, conditionQuery, -1)
		return windows, candidates, err
	default:
		return nil, nil, fmt.Errorf("%w: temporal condition %q uses unsupported modality %q", ErrInvalidMultimodal, condition.ID, condition.Modality)
	}
}

// filterUniverseCandidates applies the precomputed facet/asset universe to a
// candidate list the store could not filter itself (the lexical and transcript
// rankers take no universe). A ready empty universe means nothing is eligible.
func filterUniverseCandidates(candidates []Candidate, universe map[string]bool, universeReady bool) []Candidate {
	if !universeReady || universe == nil {
		return candidates
	}
	kept := candidates[:0]
	for _, candidate := range candidates {
		if universe[candidate.ShotID] {
			kept = append(kept, candidate)
		}
	}
	return kept
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
