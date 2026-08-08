package search

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
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
	store   ShotStore
	opts    Options
	gate    *EvidenceGate
	select_ *Selection
}

// NewService wires the engine. A nil store is tolerated: searching methods
// return an error while nothing else on the Service touches the store.
func NewService(store ShotStore, opts Options) *Service {
	return &Service{
		store:   store,
		opts:    opts,
		gate:    NewEvidenceGate(opts),
		select_: NewSelection(opts.Selection),
	}
}

// maxSearchLimit bounds the API-facing limit; a request above it is clamped.
const maxSearchLimit = 100

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
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	q := Compile(raw)
	if intent != IntentAuto {
		q.Intent = intent
	}
	q.Limit = limit
	q.Filters = SearchFilters{Facets: req.Facets}

	profile := Profiles()[q.Intent]
	recallLimit := limit * recallMultiplier
	if recallLimit < minRecallPool {
		recallLimit = minRecallPool
	}
	if recallLimit > maxRecallPool {
		recallLimit = maxRecallPool
	}

	channels := []CandidateRetriever{
		NewLexicalRetriever(s.store, profile.FieldWeights),
		NewHeuristicSemanticRetriever(s.store),
		NewTranscriptRetriever(s.store),
		NewMetadataRetriever(s.store),
	}
	// The embedding channel joins when the provider is configured AND the
	// intent's profile assigns it weight. Speech queries keep embedding out
	// (the transcript channel owns speech); semantic/fact queries let the
	// real vectors contribute. Without an embedder, the channel is never
	// constructed and the pipeline is byte-identical to v0.27.
	if s.opts.Embedder != nil && profile.ChannelWeights[SignalTextEmbedding] > 0 {
		channels = append(channels, NewTextEmbeddingRetriever(s.store, s.opts.Embedder))
	}
	results := make([]ChannelResult, 0, len(channels))
	// When facets are set, the semantic channel (the full-library scorer)
	// defines the candidate universe: other channels' candidates outside it
	// are dropped rather than bypassing the facet constraint.
	var universe map[string]bool
	for _, channel := range channels {
		candidates, err := channel.Retrieve(ctx, q, recallLimit)
		if err != nil {
			return nil, err
		}
		if channel.Name() == SignalHeuristicSemantic && hasAnyFacet(q.Filters.Facets) {
			universe = make(map[string]bool, len(candidates))
			for _, c := range candidates {
				universe[c.ShotID] = true
			}
		}
		results = append(results, ChannelResult{Signal: channel.Name(), Candidates: candidates})
	}

	fusion := s.opts.Fusion
	if fusion == nil {
		fusion = &RRF{K: DefaultRRFK}
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

	diversity := req.Diversity
	if diversity <= 0 {
		diversity = s.opts.Selection.Diversity
		if q.Intent == IntentCreative {
			diversity = 0.6
		}
	}
	sel := NewSelection(SelectionOptions{
		Diversity:          diversity,
		SameAssetPenalty:   s.opts.Selection.SameAssetPenalty,
		SameSessionPenalty: s.opts.Selection.SameSessionPenalty,
		NearTimePenalty:    s.opts.Selection.NearTimePenalty,
		NearTimeWindowMS:   s.opts.Selection.NearTimeWindowMS,
	})
	selected := sel.Select(reranked, limit)

	response.Query.Intent = q.Intent
	response.SearchID = randomHex(8)
	response.QueryHash = queryHash(q, req.Mode, s.opts.ProfileVersion)
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
			Rank:        i + 1,
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
			context, err := s.shotContext(ctx, candidate)
			if err != nil {
				return nil, err
			}
			item.Context = context
		}
		response.Results = append(response.Results, item)
	}
	return response, nil
}

// reranker returns the configured reranker (none this round).
func (s *Service) reranker() Reranker {
	return NoneReranker{}
}

// shotContext fetches the previous/next shots of a selected shot. Neighbour
// metadata never participates in scoring or evidence.
func (s *Service) shotContext(ctx context.Context, c Candidate) (*ShotContext, error) {
	prev, next, err := s.store.NeighborShots(ctx, c.AssetID, c.Ordinal)
	if err != nil {
		return nil, err
	}
	context := &ShotContext{}
	if prev != nil {
		context.PreviousShot = &ContextShot{ShotID: prev.ID, StartMS: prev.StartMS, EndMS: prev.EndMS, Description: prev.Description}
	}
	if next != nil {
		context.NextShot = &ContextShot{ShotID: next.ID, StartMS: next.StartMS, EndMS: next.EndMS, Description: next.Description}
	}
	return context, nil
}

// hasAnyFacet reports whether the facet filter carries any constraint at all.
func hasAnyFacet(f domain.FacetFilter) bool {
	return len(f.AssetTypes) > 0 || len(f.ShotSizes) > 0 || len(f.CameraMotions) > 0 ||
		len(f.AudioTypes) > 0 || len(f.Qualities) > 0 || len(f.UsableAs) > 0 ||
		f.MinDurationMS != nil || f.MaxDurationMS != nil
}

// queryHash is a deterministic fingerprint of how the query was understood,
// so future feedback hooks can correlate a result with the exact pipeline
// that produced it.
func queryHash(q SearchQuery, mode, profileVersion string) string {
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
