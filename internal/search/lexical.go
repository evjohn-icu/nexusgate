package search

import (
	"context"
	"strings"
)

// LexicalRetriever is the field-aware FTS channel: weighted bm25 over the
// shot search index. A flat profile (all 1.0 weights) reproduces the legacy
// lexical score exactly, so field weighting is a strict superset — intent
// profiles can tilt speech away from description or fact queries toward
// objects/actions without a second ranking implementation.
type LexicalRetriever struct {
	store   ShotStore
	weights [5]float64
}

// NewLexicalRetriever wraps the store with the profile's field weights.
func NewLexicalRetriever(store ShotStore, weights [5]float64) *LexicalRetriever {
	return &LexicalRetriever{store: store, weights: weights}
}

func (r *LexicalRetriever) Name() string { return SignalLexical }

func (r *LexicalRetriever) Retrieve(ctx context.Context, q SearchQuery, limit int) ([]Candidate, error) {
	shots, err := r.store.LexicalRankedShots(ctx, q.Raw, r.weights, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(shots))
	for _, shot := range shots {
		if shot.LexicalScore <= 0 {
			continue
		}
		c := toCandidate(shot)
		c.Signals[SignalLexical] = shot.LexicalScore
		out = append(out, c)
	}
	return out, nil
}

// HeuristicSemanticRetriever is the deterministic semantic channel: the
// legacy candidate scorer (cosine over the FNV feature vectors with the
// shared-token gate). It is the only channel that applies facets, because
// scoreShotCandidates is the full-library scorer — when facets are set it
// defines the candidate universe and the other channels' candidates outside
// it are dropped by the service.
type HeuristicSemanticRetriever struct {
	store ShotStore
}

func NewHeuristicSemanticRetriever(store ShotStore) *HeuristicSemanticRetriever {
	return &HeuristicSemanticRetriever{store: store}
}

func (r *HeuristicSemanticRetriever) Name() string { return SignalHeuristicSemantic }

func (r *HeuristicSemanticRetriever) Retrieve(ctx context.Context, q SearchQuery, limit int) ([]Candidate, error) {
	shots, err := r.store.ScoreCandidates(ctx, q.Raw, q.Filters.Facets)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(shots))
	for _, shot := range shots {
		if shot.SemanticScore <= 0 {
			continue
		}
		c := toCandidate(shot)
		c.Signals[SignalHeuristicSemantic] = shot.SemanticScore
		out = append(out, c)
	}
	return out, nil
}

// TranscriptRetriever is the speech channel: aligned transcript words
// overlapping the shot's time range. A shot only scores where the words
// actually fall, so speech evidence never leaks to the whole asset.
type TranscriptRetriever struct {
	store ShotStore
}

func NewTranscriptRetriever(store ShotStore) *TranscriptRetriever {
	return &TranscriptRetriever{store: store}
}

func (r *TranscriptRetriever) Name() string { return SignalTranscript }

func (r *TranscriptRetriever) Retrieve(ctx context.Context, q SearchQuery, limit int) ([]Candidate, error) {
	shots, err := r.store.TranscriptRankedShots(ctx, q.Raw, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(shots))
	for _, shot := range shots {
		if shot.TranscriptScore <= 0 {
			continue
		}
		c := toCandidate(shot)
		c.Signals[SignalTranscript] = shot.TranscriptScore
		out = append(out, c)
	}
	return out, nil
}

// MetadataRetriever is the weak asset-level channel (filename, then summary).
// A metadata hit is a retrieval hint, never shot-level evidence: the evidence
// gate ignores it, so an asset-global tag cannot confirm an object for a shot
// that never saw it.
type MetadataRetriever struct {
	store ShotStore
}

func NewMetadataRetriever(store ShotStore) *MetadataRetriever {
	return &MetadataRetriever{store: store}
}

func (r *MetadataRetriever) Name() string { return SignalMetadata }

func (r *MetadataRetriever) Retrieve(ctx context.Context, q SearchQuery, limit int) ([]Candidate, error) {
	if strings.TrimSpace(q.Raw) == "" {
		return nil, nil
	}
	shots, err := r.store.MetadataRankedShots(ctx, q.Raw, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(shots))
	for _, shot := range shots {
		if shot.MetadataScore <= 0 {
			continue
		}
		c := toCandidate(shot)
		c.Signals[SignalMetadata] = shot.MetadataScore
		out = append(out, c)
	}
	return out, nil
}

// ensure the interface is satisfied at compile time.
var (
	_ CandidateRetriever = (*LexicalRetriever)(nil)
	_ CandidateRetriever = (*HeuristicSemanticRetriever)(nil)
	_ CandidateRetriever = (*TranscriptRetriever)(nil)
	_ CandidateRetriever = (*MetadataRetriever)(nil)
)
