package search

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// Service is the Search v2 engine over a ShotStore. Construction is cheap and
// safe with a nil store (tests that never search); every method guards it.
//
// Pipeline: Compile -> Route -> profile -> channels (recall) -> fusion ->
// evidence gate -> reranker (none) -> selection -> response assembly.
//
// LegacySearch reproduces repository.HybridSearchShots exactly (the golden
// set's equality test pins it); Search is the v2 pipeline.
type Service struct {
	store ShotStore
	opts  Options
	gate  *EvidenceGate
}

// NewService wires the engine. A nil store is tolerated: searching methods
// return an error while nothing else on the Service touches the store.
func NewService(store ShotStore, opts Options) *Service {
	return &Service{
		store: store,
		opts:  opts,
		gate:  NewEvidenceGate(opts),
	}
}

// MaxSearchLimit bounds the API-facing limit; a request above it is clamped.
const MaxSearchLimit = 100

// MaxSearchWindow bounds the ranked list that one request may traverse.
const MaxSearchWindow = 200

// ErrSearchPaginationWindow means offset+limit would exceed the search window.
var ErrSearchPaginationWindow = errors.New("search: pagination window exceeded")

// recallMultiplier sizes the per-channel recall pool relative to the result
// limit: recall top 3x (at least 30, at most 200) so fusion, the gate and
// selection all have room to move before the final top-K.
const recallMultiplier = 3
const minRecallPool = 30
const maxRecallPool = 200

// Search runs the full v2 pipeline and assembles the structured response.
func (s *Service) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	response := &SearchResponse{Results: []ResultItem{}}
	if s.store == nil {
		return nil, errors.New("search: store not wired")
	}
	raw := strings.TrimSpace(req.Query)
	response.Query.Raw = raw
	if raw == "" {
		response.Query.Intent = IntentAuto
		return response, nil
	}
	intent, ok := normalizeMode(req.Mode)
	if !ok {
		return nil, errors.New("search: unknown mode " + req.Mode)
	}
	limit, target, err := normalizePagination(req.Limit, req.Offset)
	if err != nil {
		return nil, err
	}
	pageOffset := req.Offset
	if pageOffset < 0 {
		pageOffset = 0
	}

	q := Compile(raw)
	if intent != IntentAuto {
		q.Intent = intent
	}
	q.Limit = limit
	q.Filters = SearchFilters{Facets: req.Facets}
	if req.AssetFilter != nil {
		q.Filters.AssetFilter = *req.AssetFilter
	}

	// Similar intent is a real nearest-neighbour search, not a generic text
	// pass: embed the query text and cosine-scan the text embeddings, falling
	// back to the heuristic semantic vectors when no embeddings exist. The
	// response shape matches Search so every consumer is agnostic. The ranked
	// list is retrieved at most MaxSearchWindow wide, and when a facet or
	// asset-context filter is set it is intersected with the same v2 universe
	// the semantic channel defines — asset_filter is never silently ignored.
	if q.Intent == IntentSimilar {
		candidates, err := s.SimilarByText(ctx, q.Raw, MaxSearchWindow)
		if errors.Is(err, ErrNoEmbeddingSearch) {
			candidates, err = s.SimilarByHeuristic(ctx, q.Raw, MaxSearchWindow)
		}
		if err != nil {
			return nil, err
		}
		if hasAnySearchFilter(q.Filters.Facets, q.Filters.AssetFilter) {
			universe, err := s.similarUniverse(ctx, q.Raw, q.Filters.Facets, q.Filters.AssetFilter)
			if err != nil {
				return nil, err
			}
			kept := candidates[:0]
			for _, c := range candidates {
				if universe[c.ShotID] {
					kept = append(kept, c)
				}
			}
			candidates = kept
		}
		// Page the ranked nearest-neighbour list: at most effectiveLimit results
		// starting at the offset; an offset beyond the list is an empty page.
		total := len(candidates)
		var page []Candidate
		if pageOffset < total {
			end := pageOffset + limit
			if end > total {
				end = total
			}
			page = candidates[pageOffset:end]
		}
		pageLen := len(page)
		moreBeyond := pageLen > 0 && (pageOffset+pageLen < total || (total >= MaxSearchWindow && pageOffset+pageLen >= MaxSearchWindow))
		hasMore, nextOffset, windowExhausted := paginationMetadata(pageOffset, pageLen, moreBeyond)
		response.Offset = pageOffset
		response.Limit = limit
		response.HasMore = hasMore
		response.NextOffset = nextOffset
		response.WindowExhausted = windowExhausted
		response.Query.Intent = q.Intent
		response.SearchID = randomHex(8)
		response.QueryHash = queryProfileFingerprint(q, req.Mode, s.opts.ProfileVersion)
		for i, candidate := range page {
			item := ResultItem{
				ShotID:      candidate.ShotID,
				AssetID:     candidate.AssetID,
				Filename:    candidate.Filename,
				StartMS:     candidate.StartMS,
				EndMS:       candidate.EndMS,
				Description: candidate.Description,
				Tags:        candidate.Tags,
				Objects:     candidate.Objects,
				Actions:     candidate.Actions,
				Mood:        candidate.Mood,
				Score:       candidate.Score,
				Rank:        pageOffset + i + 1,
				Scores:      map[string]float64{},
			}
			for signal, value := range candidate.Signals {
				item.Scores[signal] = value
			}
			response.Results = append(response.Results, item)
		}
		return response, nil
	}

	profile := Profiles()[q.Intent]
	recallLimit := target * recallMultiplier
	if recallLimit < minRecallPool {
		recallLimit = minRecallPool
	}
	if recallLimit > maxRecallPool {
		recallLimit = maxRecallPool
	}

	// Routing selects recall channels only; it does not establish what a shot
	// claims. Channel construction is intent-aware: a channel whose profile weight is
	// zero (or absent) is not executed at all — no DB query, no embedding call,
	// no candidate noise. The transcript channel belongs to speech intent; the
	// embedding channel needs a configured provider AND a positive profile
	// weight. Auto/fact keep the full silent-channel set so the default path
	// ranks exactly as v0.27's plain RRF did.
	channels := []CandidateRetriever{}
	if profile.ChannelWeights[SignalLexical] > 0 {
		channels = append(channels, NewLexicalRetriever(s.store, profile.FieldWeights))
	}
	if profile.ChannelWeights[SignalHeuristicSemantic] > 0 {
		channels = append(channels, NewHeuristicSemanticRetriever(s.store))
	}
	if profile.ChannelWeights[SignalTranscript] > 0 {
		channels = append(channels, NewTranscriptRetriever(s.store))
	}
	if profile.ChannelWeights[SignalMetadata] > 0 {
		channels = append(channels, NewMetadataRetriever(s.store))
	}
	if s.opts.Embedder != nil && profile.ChannelWeights[SignalTextEmbedding] > 0 {
		channels = append(channels, NewTextEmbeddingRetriever(s.store, s.opts.Embedder))
	}
	results := make([]ChannelResult, 0, len(channels))
	// When facets are set, the semantic channel (the full-library scorer)
	// defines the candidate universe: other channels' candidates outside it
	// are dropped rather than bypassing the facet constraint. The semantic
	// retriever retains facet-matched semantic-0 shots, so an exact lexical
	// match under a facet survives the universe gate.
	var universe map[string]bool
	for _, channel := range channels {
		candidates, err := channel.Retrieve(ctx, q, recallLimit)
		if err != nil {
			return nil, err
		}
		if channel.Name() == SignalHeuristicSemantic && hasAnySearchFilter(q.Filters.Facets, q.Filters.AssetFilter) {
			universe = make(map[string]bool, len(candidates))
			for _, c := range candidates {
				universe[c.ShotID] = true
			}
		}
		results = append(results, ChannelResult{Signal: channel.Name(), Candidates: candidates})
	}

	// Fusion is per-intent: auto/fact keep the plain RRF (k=60) so the default
	// path is byte-identical to the pre-weights engine; the differentiated
	// intents (speech/semantic/creative/similar) use WeightedRRF so the
	// profile's ChannelWeights actually rank. An explicit Fusion (the
	// benchmark, the legacy endpoints) always wins.
	fusion := s.opts.Fusion
	if fusion == nil {
		fusion = defaultFusion(q.Intent)
	}
	fused := fusion.Fuse(results)
	if universe != nil {
		kept := fused[:0]
		for _, c := range fused {
			if universe[c.ShotID] {
				kept = append(kept, c)
			}
		}
		fused = kept
	}

	// Evidence + gate. The gate always computes evidence for the pool (so the
	// response can explain any result); it filters only for fact intents when
	// enabled. Transcript-span lookups are the only extra store round trip,
	// bounded by the recall pool.
	needEvidence := req.IncludeEvidence || q.Intent == IntentFact
	var evidenceByID map[string][]Evidence
	if needEvidence {
		var err error
		fused, evidenceByID, err = s.gate.Gate(ctx, s.store, q, fused, q.Intent == IntentFact && s.opts.GateFact)
		if err != nil {
			return nil, err
		}
	}
	sortCandidates(fused)

	reranked, err := s.reranker().Rerank(ctx, q, fused)
	if err != nil {
		return nil, err
	}

	// Session diversity needs the shoot-session id per surviving candidate,
	// but only when selection will actually penalize sessions. One batched
	// fetch (post-gate, pre-selection) keeps the query count at exactly one
	// per search instead of one per result.
	diversity := req.Diversity
	if diversity <= 0 {
		diversity = s.opts.Selection.Diversity
		if q.Intent == IntentCreative {
			diversity = 0.6
		}
	}
	if diversity > 0 && s.opts.Selection.SameSessionPenalty > 0 {
		if ids := assetIDs(reranked); len(ids) > 0 {
			sessions, err := s.store.ShotSessions(ctx, ids)
			if err != nil {
				return nil, err
			}
			for i := range reranked {
				reranked[i].SessionID = sessions[reranked[i].AssetID]
			}
		}
	}
	sel := NewSelection(SelectionOptions{
		Diversity:          diversity,
		SameAssetPenalty:   s.opts.Selection.SameAssetPenalty,
		SameSessionPenalty: s.opts.Selection.SameSessionPenalty,
		NearTimePenalty:    s.opts.Selection.NearTimePenalty,
		NearTimeWindowMS:   s.opts.Selection.NearTimeWindowMS,
	})
	// Selection asks for one extra result beyond the target so the caller can
	// detect whether another page exists: moreBeyond means the pool held at
	// least one candidate past this page. The extra probe is trimmed before
	// paging so the returned page shape is unchanged.
	selected := sel.Select(reranked, target+1)
	moreBeyond := len(selected) > target
	if moreBeyond {
		selected = selected[:target]
	}
	// Offset pages the FINAL ranked list — after selection/diversity, so the
	// recall pool and the diversity choices are never re-run or re-trimmed.
	// An offset at or beyond the list length yields empty results, not a
	// wrapped page.
	selected = pageResults(selected, pageOffset)
	hasMore, nextOffset, windowExhausted := paginationMetadata(pageOffset, len(selected), moreBeyond)
	response.Offset = pageOffset
	response.Limit = limit
	response.HasMore = hasMore
	response.NextOffset = nextOffset
	response.WindowExhausted = windowExhausted
	var neighborsByID map[string]Neighbors
	if req.IncludeContext && len(selected) > 0 {
		requests := make([]NeighborRequest, 0, len(selected))
		for _, candidate := range selected {
			requests = append(requests, NeighborRequest{
				ShotID:  candidate.ShotID,
				AssetID: candidate.AssetID,
				Ordinal: candidate.Ordinal,
			})
		}
		neighborsByID, err = s.store.NeighborShotsBatch(ctx, requests)
		if err != nil {
			return nil, err
		}
	}

	response.Query.Intent = q.Intent
	response.SearchID = randomHex(8)
	response.QueryHash = queryProfileFingerprint(q, req.Mode, s.opts.ProfileVersion)
	for i, candidate := range selected {
		item := ResultItem{
			ShotID:      candidate.ShotID,
			AssetID:     candidate.AssetID,
			Filename:    candidate.Filename,
			StartMS:     candidate.StartMS,
			EndMS:       candidate.EndMS,
			Description: candidate.Description,
			Tags:        candidate.Tags,
			Objects:     candidate.Objects,
			Actions:     candidate.Actions,
			Mood:        candidate.Mood,
			Score:       candidate.Score,
			Rank:        pageOffset + i + 1,
			Scores:      map[string]float64{},
		}
		for signal, value := range candidate.Signals {
			item.Scores[signal] = value
		}
		item.Scores[fusion.Name()] = candidate.Score
		if needEvidence {
			item.Evidence = evidenceByID[candidate.ShotID]
		}
		if req.IncludeContext {
			item.Context = contextFromNeighbors(neighborsByID[candidate.ShotID])
		}
		response.Results = append(response.Results, item)
	}
	return response, nil
}

// normalizePagination applies defaults and validates the reachable ranked
// window before adding offset and limit, preventing integer overflow.
func normalizePagination(limit, offset int) (effectiveLimit, target int, err error) {
	if limit <= 0 {
		effectiveLimit = DefaultLimit
	} else if limit > MaxSearchLimit {
		effectiveLimit = MaxSearchLimit
	} else {
		effectiveLimit = limit
	}
	if offset < 0 {
		offset = 0
	}
	if offset > MaxSearchWindow-effectiveLimit || offset > math.MaxInt-effectiveLimit {
		return 0, 0, fmt.Errorf("%w: offset %d and limit %d exceed %d", ErrSearchPaginationWindow, offset, effectiveLimit, MaxSearchWindow)
	}
	return effectiveLimit, offset + effectiveLimit, nil
}

// ValidatePagination validates API pagination. Limits above MaxSearchLimit are
// valid because the service clamps them.
func ValidatePagination(limit, offset int) error {
	_, _, err := normalizePagination(limit, offset)
	return err
}

// reranker returns the configured reranker (none this round).
func (s *Service) reranker() Reranker {
	return NoneReranker{}
}

// contextFromNeighbors projects a batch lookup result into the public search
// response. Neighbour metadata never participates in scoring or evidence.
func contextFromNeighbors(neighbors Neighbors) *ShotContext {
	context := &ShotContext{}
	if neighbors.Previous != nil {
		prev := neighbors.Previous
		context.PreviousShot = &ContextShot{ShotID: prev.ID, StartMS: prev.StartMS, EndMS: prev.EndMS, Description: prev.Description}
	}
	if neighbors.Next != nil {
		next := neighbors.Next
		context.NextShot = &ContextShot{ShotID: next.ID, StartMS: next.StartMS, EndMS: next.EndMS, Description: next.Description}
	}
	return context
}

// hasAnyFacet reports whether the facet filter carries any constraint at all.
func hasAnyFacet(f domain.FacetFilter) bool {
	return len(f.AssetTypes) > 0 || len(f.ShotSizes) > 0 || len(f.CameraMotions) > 0 ||
		len(f.AudioTypes) > 0 || len(f.Qualities) > 0 || len(f.UsableAs) > 0 ||
		f.MinDurationMS != nil || f.MaxDurationMS != nil
}

// hasAnySearchFilter reports whether facets OR the asset-context filter carry
// any constraint; when either is set, the heuristic-semantic universe gate
// constrains every recall channel so no channel can bypass the filters.
func hasAnySearchFilter(f domain.FacetFilter, a domain.AssetContextFilter) bool {
	if hasAnyFacet(f) {
		return true
	}
	return a.CapturedFrom != nil || a.CapturedTo != nil ||
		strings.TrimSpace(a.RegionLabel) != "" || strings.TrimSpace(a.CameraModel) != "" ||
		strings.TrimSpace(a.SessionID) != "" || a.Status != ""
}

// defaultFusion picks the fusion strategy for the default (unset) options
// path. Auto/fact rank on plain RRF — byte-identical to the pre-weights
// engine, which is what the golden/benchmark floor assumes. The differentiated
// intents apply their profile ChannelWeights through WeightedRRF so the
// weights are effective in production, not decorative.
func defaultFusion(intent SearchIntent) FusionStrategy {
	weights, ok := fusionWeights[intent]
	if !ok {
		return &RRF{K: DefaultRRFK}
	}
	return &WeightedRRF{Weights: weights, K: DefaultRRFK}
}

// fusionWeights maps the intents whose profile weights actually rank the
// default path. Auto and fact are deliberately absent: their default path is
// plain RRF, the lease that keeps the golden set byte-identical (their
// ChannelWeights still gate channel construction and apply under an explicit
// WeightedBlend fusion — see profile.go).
var fusionWeights = map[SearchIntent]map[string]float64{
	IntentSpeech:   {SignalTranscript: 0.60, SignalLexical: 0.25, SignalHeuristicSemantic: 0.15},
	IntentSemantic: {SignalHeuristicSemantic: 0.45, SignalTextEmbedding: 0.35, SignalLexical: 0.20},
	IntentCreative: {SignalLexical: 0.30, SignalHeuristicSemantic: 0.45, SignalTranscript: 0.05, SignalMetadata: 0.20},
	IntentSimilar:  {SignalHeuristicSemantic: 0.45, SignalTextEmbedding: 0.35, SignalLexical: 0.20},
}

// pageResults applies offset pagination to a final ranked list: offset <= 0
// returns it unchanged, offset >= len yields empty results (a page beyond the
// end is empty, never wrapped). Slicing shrinks the list; the caller's
// rank/score fields stay intact, so a response is re-numbered by its own loop.
func pageResults(results []Candidate, offset int) []Candidate {
	if offset <= 0 || len(results) == 0 {
		return results
	}
	if offset >= len(results) {
		return nil
	}
	return results[offset:]
}

// paginationMetadata derives the paging flags for a page that already knows
// whether recall holds more results beyond it. windowExhausted reports a page
// ending exactly at the hard MaxSearchWindow boundary — more may exist past it,
// but the window caps how far one request may traverse. hasMore means a next
// page exists inside the window, and nextOffset names where it starts.
func paginationMetadata(offset, pageLen int, moreBeyond bool) (hasMore bool, nextOffset *int, windowExhausted bool) {
	windowEnd := offset + pageLen
	windowExhausted = moreBeyond && windowEnd >= MaxSearchWindow
	hasMore = moreBeyond && !windowExhausted && pageLen > 0
	if hasMore {
		next := windowEnd
		nextOffset = &next
	}
	return
}

// assetIDs collects the distinct asset ids of a candidate list, in first-seen
// order, for the batched session lookup.
func assetIDs(candidates []Candidate) []string {
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.AssetID]; ok {
			continue
		}
		seen[c.AssetID] = struct{}{}
		out = append(out, c.AssetID)
	}
	return out
}

// queryProfileFingerprint identifies the raw query, requested mode and profile
// version. It deliberately is not a complete execution fingerprint: facets,
// diversity and runtime search configuration are not included. The public
// field remains query_hash for API compatibility.
func queryProfileFingerprint(q SearchQuery, mode, profileVersion string) string {
	sum := sha256.Sum256([]byte(profileVersion + "|" + mode + "|" + q.Raw))
	return hex.EncodeToString(sum[:8])
}

// randomHex returns n random bytes as hex (the per-search session id).
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf)
}

// LegacySearch reproduces repository.HybridSearchShots exactly: the legacy
// candidate universe (shots with a semantic vector, facet-constrained via
// ScoreCandidates), the weighted blend 0.70 semantic / 0.30 lexical, the
// Score > 0 filter and the deterministic score-descending / ID-ascending
// tie-break. The retrieval golden set's equality test pins this to the
// repository implementation; do not "improve" it here.
func (s *Service) LegacySearch(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	if s.store == nil {
		return nil, errors.New("search: store not wired")
	}
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	candidates, err := s.store.ScoreCandidates(ctx, q, facets)
	if err != nil {
		return nil, err
	}
	const semanticWeight, lexicalWeight = 0.70, 0.30
	scored := make([]domain.ShotSearchResult, 0, len(candidates))
	for _, c := range candidates {
		c.Score = semanticWeight*c.SemanticScore + lexicalWeight*c.LexicalScore
		if c.Score > 0 {
			scored = append(scored, c)
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score == scored[j].Score {
			return scored[i].ID < scored[j].ID
		}
		return scored[i].Score > scored[j].Score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}
