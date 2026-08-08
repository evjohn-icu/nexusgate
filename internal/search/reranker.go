package search

import "context"

// NoneReranker is the default reranker: pass-through. Real text or
// multimodal rerankers plug in behind the same interface later; a reranker
// only ever sees a bounded top-K pool, never the whole library.
type NoneReranker struct{}

func (NoneReranker) Name() string { return "none" }

func (NoneReranker) Rerank(_ context.Context, _ SearchQuery, candidates []Candidate) ([]Candidate, error) {
	return candidates, nil
}
