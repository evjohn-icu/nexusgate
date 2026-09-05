package search

import (
	"context"
	"errors"
	"math"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// ErrNoEmbeddingSearch is the "embeddings unavailable" verdict for the
// nearest-neighbour path. SimilarByText returns it — and nothing else — when
// no embedder is configured or the library holds no vectors under the current
// model, so the integrator can fall back to heuristic similarity with a plain
// errors.Is check instead of inspecting configuration itself. It is not a
// search failure: the query is fine, the evidence is just not on disk.
var ErrNoEmbeddingSearch = errors.New("search: text embedding unavailable for similar")

// SimilarByText is the real text-embedding nearest-neighbour path for
// mode=similar. It embeds the query with the configured embedder, cosine-scans
// the library's stored vectors (the same SQLite-first full pass the embedding
// retrieval channel uses — no vector DB, no duplicated cosine logic) and
// returns the top `limit` shots ranked by similarity.
//
// Use it first: when an embedder is configured and vectors exist under its
// model this is the honest NN answer, unlike the generic 5-channel pipeline
// that mode=similar currently inherits. The candidate's Score is the cosine
// similarity itself and Signals[SignalTextEmbedding] carries the same value,
// so a caller can render the number under the signal's name.
//
// It never degrades silently: a nil embedder or an empty vector library
// returns ErrNoEmbeddingSearch so the caller knows the result would be
// guessed, not measured.
func (s *Service) SimilarByText(ctx context.Context, query string, limit int) ([]Candidate, error) {
	if s.store == nil {
		return nil, errors.New("search: store not wired")
	}
	if s.opts.Embedder == nil {
		return nil, ErrNoEmbeddingSearch
	}
	vectors, err := s.opts.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, ErrNoEmbeddingSearch
	}
	rows, err := s.store.ListShotTextEmbeddings(ctx, s.opts.Embedder.Model())
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNoEmbeddingSearch
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	queryVector := vectors[0]
	if err := validateQueryEmbedding(queryVector); err != nil {
		return nil, err
	}
	scored := make([]Candidate, 0, len(rows))
	for _, row := range rows {
		// A stale-model row (dimension mismatch) is skipped entirely, not
		// scored as a zero that could fill the top-N.
		if len(queryVector) != len(row.Vector) {
			continue
		}
		similarity := embeddingCosine(queryVector, row.Vector)
		if similarity <= 0 || math.IsNaN(similarity) || math.IsInf(similarity, 0) {
			continue
		}
		candidate := toCandidate(row.Shot)
		candidate.Signals[SignalTextEmbedding] = similarity
		candidate.Score = similarity
		scored = append(scored, candidate)
	}
	sortCandidates(scored)
	if len(scored) == 0 {
		// Every stored vector is a different model's artifact (dimension
		// mismatch after a model switch without a rebuild): the library has no
		// vectors under the current model, so the "embeddings unavailable"
		// verdict applies and the caller falls back to heuristic similarity.
		// Returning an empty nil-error would read as a genuine no-match.
		return nil, ErrNoEmbeddingSearch
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// SimilarByHeuristic is the fallback nearest-neighbour path for mode=similar:
// the legacy heuristic vectors (deterministic FNV feature hashes) scored by
// ScoreCandidates with its shared-token gate. It mirrors the legacy
// similarShots semantics — keep only SemanticScore > 0, rank descending — but
// through the engine's ShotStore, so the integrator never reaches into SQL.
//
// Use it only when SimilarByText answered ErrNoEmbeddingSearch: heuristic
// cosine is a real similarity signal, just a weaker one than the model's
// embeddings, and it is what mode=similar already effectively served before
// the NN path existed. The candidate's Score is the SemanticScore and
// Signals[SignalHeuristicSemantic] carries the same value.
func (s *Service) SimilarByHeuristic(ctx context.Context, query string, limit int) ([]Candidate, error) {
	if s.store == nil {
		return nil, errors.New("search: store not wired")
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	shots, err := s.store.ScoreCandidates(ctx, query, domain.FacetFilter{})
	if err != nil {
		return nil, err
	}
	scored := make([]Candidate, 0, len(shots))
	for _, shot := range shots {
		if shot.SemanticScore <= 0 {
			continue
		}
		candidate := toCandidate(shot)
		candidate.Signals[SignalHeuristicSemantic] = shot.SemanticScore
		candidate.Score = shot.SemanticScore
		scored = append(scored, candidate)
	}
	sortCandidates(scored)
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// similarUniverse is the v2 candidate universe for mode=similar: the set of
// shot ids whose owning asset passes the facet + asset-context filter, as
// ScoreCandidatesV2 reports it. The ranked nearest-neighbour list is
// intersected with it so an asset_filter (or facet) is never silently
// ignored in similar mode.
func (s *Service) similarUniverse(ctx context.Context, q string, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) (map[string]bool, error) {
	shots, err := s.store.ScoreCandidatesV2(ctx, q, facets, assetFilter)
	if err != nil {
		return nil, err
	}
	universe := make(map[string]bool, len(shots))
	for _, shot := range shots {
		universe[shot.ID] = true
	}
	return universe, nil
}
