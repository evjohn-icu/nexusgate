package search

import (
	"context"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// ShotEmbeddingRow is one persisted embedding: the owning shot (full row —
// the engine builds candidates straight from it), the model it was written
// under, the vector (float32 little-endian) and the hash of the source text
// it was derived from. The hash is what makes embeddings incrementally
// rebuildable: a shot whose derived text changed gets re-embedded without
// touching the canonical analysis.
type ShotEmbeddingRow struct {
	Shot           domain.ShotSearchResult
	Model          string
	Vector         []float32
	SourceTextHash string
}

// TranscriptSpanRequest identifies one shot's transcript window. ShotID is
// the result key used by the search engine; AssetID and the time bounds are
// the storage lookup coordinates.
type TranscriptSpanRequest struct {
	ShotID  string
	AssetID string
	StartMS int64
	EndMS   int64
}

// NeighborRequest identifies one shot whose adjacent shots are needed for
// result context. ShotID is the result key; ordinals may be non-contiguous.
type NeighborRequest struct {
	ShotID  string
	AssetID string
	Ordinal int
}

// Neighbors is the minimal neighboring-shot projection returned by a batch
// lookup. A missing side is nil, just as NeighborShots reported previously.
type Neighbors struct {
	Previous *domain.AssetShot
	Next     *domain.AssetShot
}

// ShotStore is the narrow persistence contract the Search v2 engine consumes,
// implemented by internal/repository/sqlite. Declared here, at the consumer,
// per the repository-interface convention in CLAUDE.md. The methods are the
// retrieval channels plus the evidence/context lookups; the engine never sees
// SQL.
//
// Contract notes for implementers:
//
//   - ScoreCandidates is the legacy candidate scorer (lexical bm25 + heuristic
//     semantic cosine with the shared-token gate, facet-aware). It powers the
//     compatibility path so the old GET endpoints and the golden set keep the
//     exact legacy ranking, and the heuristic semantic channel.
//   - LexicalRankedShots is field-aware weighted bm25. weights is indexed
//     [description, tags, objects, actions, mood]. A flat all-1.0 weight set
//     must equal the legacy bm25(asset_shot_search) score exactly, so
//     field-aware retrieval is a strict superset of the legacy lexical signal.
//   - TranscriptRankedShots matches query tokens against transcript_words and
//     scores only shots whose time range overlaps a matched word: speech
//     evidence never leaks to the whole asset.
//   - MetadataRankedShots is the weak asset-level channel (filename first,
//     summary only as a weak signal). The evidence gate must never treat a
//     metadata hit as shot-level evidence.
//   - ShotTranscriptSpansBatch returns aligned words overlapping each
//     [startMS, endMS] for evidence attribution. The result is keyed by the
//     request ShotID; absent keys mean the shot has no aligned words.
//   - ShotTranscriptSpans is retained for direct/single-shot repository
//     callers, but Search hot paths must use the batch method.
//   - NeighborShots returns the previous/next shot by (asset_id, ordinal) —
//     ordinals may be non-contiguous, so look up by < and > with ORDER BY
//     ... LIMIT 1, not by arithmetic.
//   - NeighborShotsBatch is the batch form used to assemble context for all
//     selected results in one store round trip. The result is keyed by the
//     request ShotID; absent keys mean both sides are missing.
//   - ShotSession returns the shoot-session id of an asset, "" when none.
//   - ShotSessions is the batch form of ShotSession: it returns the
//     asset_id -> session_id mapping for a whole candidate set in one
//     query. Session diversity must consume this and never loop per
//     result, so selection pays one round trip per search, not one per
//     candidate. Assets with no shoot session are simply absent from the
//     map.
type ShotStore interface {
	ScoreCandidates(ctx context.Context, q string, facets domain.FacetFilter) ([]domain.ShotSearchResult, error)
	ScoreCandidatesV2(ctx context.Context, q string, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) ([]domain.ShotSearchResult, error)
	LexicalRankedShots(ctx context.Context, q string, weights [5]float64, limit int) ([]domain.ShotSearchResult, error)
	TranscriptRankedShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error)
	MetadataRankedShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error)
	ShotTranscriptSpans(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AlignmentWord, error)
	ShotTranscriptSpansBatch(ctx context.Context, requests []TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error)
	NeighborShots(ctx context.Context, assetID string, ordinal int) (*domain.AssetShot, *domain.AssetShot, error)
	NeighborShotsBatch(ctx context.Context, requests []NeighborRequest) (map[string]Neighbors, error)
	ShotSession(ctx context.Context, assetID string) (string, error)
	ShotSessions(ctx context.Context, assetIDs []string) (map[string]string, error)
	// Text embedding storage (derived, rebuildable representations — see
	// docs/search-architecture.md "retrieval_generation").
	//
	//   - UpsertShotTextEmbeddings writes the batch under the given model.
	//   - ListShotTextEmbeddings streams every vector of one model so the
	//     engine can cosine-scan the library (SQLite-first: no vector DB).
	//     Rows must be returned in a deterministic order (shot_id).
	//   - AllShotTextDocuments returns the derived text projection of every
	//     shot (description + tags + objects + actions + mood; transcript
	//     deliberately excluded) for (re)building embeddings without touching
	//     the canonical analysis.
	//   - ShotTextDocumentsByAsset / ShotTextEmbeddingHashes are the
	//     per-asset incremental path the post-commit hook uses, so a normal
	//     analysis commit re-embeds at most the shots whose derived text
	//     actually changed.
	UpsertShotTextEmbeddings(ctx context.Context, rows []ShotEmbeddingRow) error
	ListShotTextEmbeddings(ctx context.Context, model string) ([]ShotEmbeddingRow, error)
	AllShotTextDocuments(ctx context.Context) ([]ShotSearchDocument, error)
	ShotTextDocumentsByAsset(ctx context.Context, assetID string) ([]ShotSearchDocument, error)
	ShotTextEmbeddingHashes(ctx context.Context, model, assetID string) (map[string]string, error)
}

// Service API contract (implemented in service.go by the engine track):
//
//	type Service struct { ... }
//
//	// NewService wires the engine over a ShotStore. store may be nil only in
//	// tests that never search; every method guards it.
//	func NewService(store ShotStore, opts Options) *Service
//
//	// Search runs the full v2 pipeline: compile -> route -> retrieve channels
//	// -> fuse -> evidence gate -> (reranker) -> selection -> assemble response.
//	func (s *Service) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error)
//
//	// LegacySearch reproduces the legacy hybrid ranking exactly (candidate
//	// universe = ScoreCandidates, weighted blend 0.70/0.30, no gate, no
//	// diversity) and returns legacy-shaped results. It is what the old GET
//	// /api/v1/search/shots/hybrid endpoint serves after the internal takeover,
//	// and a golden equality test pins it to repository.HybridSearchShots.
//	func (s *Service) LegacySearch(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error)
//
// Defaults: Limit<=0 becomes DefaultLimit; Diversity<=0 becomes the intent
// default (creative 0.6, otherwise Selection.Diversity); Mode ""/"auto" routes
// via Router; IncludeEvidence implies the result carries per-constraint
// evidence computed by the gate; IncludeContext fetches prev/next shots.
