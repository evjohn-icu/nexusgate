package app

import (
	"context"
	"errors"
	"log/slog"
	"math"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/providers"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

// searchEmbedderAdapter adapts the Hub's embedding provider (legacy config or
// provider channel — the channel runtime wrapper resolves both) to the
// search engine's TextEmbedder contract, with one behavioural override: an
// unconfigured embedding capability is an EXPECTED state, not a fault, so
// Embed reports "no vectors" instead of an error. Without this, a Hub that
// never configured embedding would fail every semantic/fact search — the
// retriever treats an empty response as "this channel has nothing to say",
// exactly like the transcript channel without aligned words.
type searchEmbedderAdapter struct {
	embedder providers.Embedder
}

func (a searchEmbedderAdapter) Name() string { return a.embedder.Name() }

func (a searchEmbedderAdapter) Model() string { return a.embedder.Model() }

func (a searchEmbedderAdapter) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	vectors, err := a.embedder.Embed(ctx, texts)
	if err != nil {
		if errors.Is(err, ErrProviderChannelNotConfigured) {
			return nil, nil
		}
		return nil, err
	}
	return vectors, nil
}

// ensureShotTextEmbeddings is the pipeline's post-commit hook: after shot
// rows become canonical it re-embeds exactly the shots whose derived text
// changed under the current model. It is synchronous (the pipeline's
// "process exited means no call in flight" invariant) and never fails the
// job — the embedding layer is derived, rebuildable and best-effort, so every
// failure path ends in a log line instead of an error.
func (s *Service) ensureShotTextEmbeddings(ctx context.Context, assetID string) {
	store, ok := s.repo.(search.ShotStore)
	if !ok || s.embedder == nil {
		return
	}
	adapter := searchEmbedderAdapter{embedder: s.embedder}
	model := adapter.Model()
	docs, err := store.ShotTextDocumentsByAsset(ctx, assetID)
	if err != nil {
		slog.Warn("shot text embedding: read documents", "asset_id", assetID, "error", err)
		return
	}
	existing, err := store.ShotTextEmbeddingHashes(ctx, model, assetID)
	if err != nil {
		slog.Warn("shot text embedding: read hashes", "asset_id", assetID, "error", err)
		return
	}
	var changed []search.ShotSearchDocument
	for _, doc := range docs {
		if existing[doc.ShotID] != search.ShotTextSourceHash(doc) {
			changed = append(changed, doc)
		}
	}
	if len(changed) == 0 {
		return
	}
	slog.Debug("shot text embedding: incremental embed", "asset_id", assetID, "shots", len(changed))
	if _, err := embedShotDocuments(ctx, store, adapter, changed); err != nil {
		// Never fail the job: log and move on. The full rebuild command
		// (nexusgate search rebuild-embeddings) can repair the gap.
		slog.Warn("shot text embedding: embed failed (analysis job unaffected)", "asset_id", assetID, "error", err)
	}
}

// RebuildShotTextEmbeddings is the full retrieval_generation rebuild: every
// shot whose derived text hash differs from the stored (current-model) one is
// re-embedded, in bounded batches. Switching the embedding model therefore
// means running this once — the canonical analysis is never touched.
func (s *Service) RebuildShotTextEmbeddings(ctx context.Context) (int, error) {
	store, ok := s.repo.(search.ShotStore)
	if !ok || s.embedder == nil {
		return 0, errors.New("search engine or embedding provider not available")
	}
	adapter := searchEmbedderAdapter{embedder: s.embedder}
	docs, err := store.AllShotTextDocuments(ctx)
	if err != nil {
		return 0, err
	}
	existing, err := store.ListShotTextEmbeddings(ctx, adapter.Model())
	if err != nil {
		return 0, err
	}
	hashes := make(map[string]string, len(existing))
	for _, row := range existing {
		hashes[row.Shot.ID] = row.SourceTextHash
	}
	var changed []search.ShotSearchDocument
	for _, doc := range docs {
		if hashes[doc.ShotID] != search.ShotTextSourceHash(doc) {
			changed = append(changed, doc)
		}
	}
	if len(changed) == 0 {
		return 0, nil
	}
	total := 0
	const batchSize = 64
	for start := 0; start < len(changed); start += batchSize {
		end := start + batchSize
		if end > len(changed) {
			end = len(changed)
		}
		batch := changed[start:end]
		persisted, err := embedShotDocuments(ctx, store, adapter, batch)
		if err != nil {
			return total, err
		}
		total += persisted
	}
	return total, nil
}

// embedShotDocuments embeds one batch of shot documents and persists the
// rows under the current model. An unconfigured provider reports no vectors
// (the adapter's contract): that is the expected no-op state, not an error —
// the caller logs nothing and moves on, and the rebuild command reports zero
// embeddings so an operator knows there is nothing to build.
func embedShotDocuments(ctx context.Context, store search.ShotStore, embedder search.TextEmbedder, docs []search.ShotSearchDocument) (int, error) {
	texts := make([]string, len(docs))
	for i, doc := range docs {
		texts[i] = search.ShotTextSource(doc)
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	if len(vectors) == 0 {
		// Embedding not configured: nothing to persist, nothing failed.
		return 0, nil
	}
	if len(vectors) != len(docs) {
		return 0, errors.New("embedding provider returned fewer vectors than inputs")
	}
	for _, vector := range vectors {
		if len(vector) == 0 {
			return 0, nil
		}
		var norm float64
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				break
			}
			norm += value * value
		}
		if norm == 0 {
			return 0, nil
		}
	}
	rows := make([]search.ShotEmbeddingRow, 0, len(docs))
	for i, doc := range docs {
		rows = append(rows, search.ShotEmbeddingRow{
			Shot:           domainShotForEmbedding(doc),
			Model:          embedder.Model(),
			Vector:         toFloat32(vectors[i]),
			SourceTextHash: search.ShotTextSourceHash(doc),
		})
	}
	if err := store.UpsertShotTextEmbeddings(ctx, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// domainShotForEmbedding builds the minimal shot row an embedding row needs
// (the engine re-reads full rows from ListShotTextEmbeddings; the upsert only
// stores identity).
func domainShotForEmbedding(doc search.ShotSearchDocument) domain.ShotSearchResult {
	return domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: doc.ShotID}}
}

func toFloat32(values []float64) []float32 {
	out := make([]float32, len(values))
	for i, v := range values {
		out[i] = float32(v)
	}
	return out
}
