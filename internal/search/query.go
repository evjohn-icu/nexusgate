// Package search is the Search Architecture v2 engine: a local-first footage
// retrieval and selection layer. It owns query compilation, intent routing,
// channel retrieval, fusion, the evidence gate, reranking (interface only this
// round) and selection/diversity. All persistence stays in
// internal/repository/sqlite behind the narrow ShotStore interface declared in
// store.go; this package contains no SQL and no I/O beyond the store.
//
// The guiding principle is "Recall may be fuzzy. Claims may not." Semantic
// similarity may recall a shot, but the system never asserts a shot contains
// an object, behaviour or scene without shot-level evidence behind it — and
// the API reports evidence states (confirmed/possible/contradicted/unknown)
// so every consumer (UI, MCP, ChatCut, other agents) can tell a retrieval
// signal from an observational claim.
package search

import (
	"context"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// SearchIntent classifies what kind of answer a query wants. The user never
// picks one directly: the router derives it from the query text, and "auto"
// means "let the router decide" (the default).
type SearchIntent string

const (
	IntentAuto     SearchIntent = "auto"
	IntentFact     SearchIntent = "fact"
	IntentSpeech   SearchIntent = "speech"
	IntentSemantic SearchIntent = "semantic"
	IntentSimilar  SearchIntent = "similar"
	IntentCreative SearchIntent = "creative"
)

// ConstraintType is the semantic family a constraint belongs to. The list is
// deliberately not a finished ontology: it is the extensible skeleton future
// OCR/visual/temporal capabilities plug into.
type ConstraintType string

const (
	ConstraintObject   ConstraintType = "object"
	ConstraintAction   ConstraintType = "action"
	ConstraintScene    ConstraintType = "scene"
	ConstraintWeather  ConstraintType = "weather"
	ConstraintTime     ConstraintType = "time"
	ConstraintMood     ConstraintType = "mood"
	ConstraintSpeech   ConstraintType = "speech"
	ConstraintTag      ConstraintType = "tag"
	ConstraintCamera   ConstraintType = "camera"
	ConstraintShotSize ConstraintType = "shot_size"
	ConstraintMetadata ConstraintType = "metadata"
)

// Constraint is one canonical controlled-vocabulary term extracted from the
// query. Value is the canonical family name (e.g. "car" for 汽车/轿车/vehicle),
// never the raw surface form.
type Constraint struct {
	Type  ConstraintType `json:"type"`
	Value string         `json:"value"`
}

// SearchFilters narrows retrieval; the zero value matches everything.
// Facets are validated by callers (see domain.FacetFilter's doc comment).
type SearchFilters struct {
	Facets domain.FacetFilter
}

// SearchQuery is the compiled form of a raw search string. Recall may use the
// shoulds; the musts are what the evidence gate holds the result to.
type SearchQuery struct {
	Raw    string
	Intent SearchIntent

	// SpeechPhrase is the compiled, exact-match phrase for speech retrieval,
	// extracted from quotes/markers (e.g. 他说“明天见” → "明天见"), so the
	// transcript channel matches the phrase rather than the raw query. Empty
	// when the query carries no speech claim; retrievers then fall back to
	// Raw, which keeps behavior unchanged for non-speech queries.
	SpeechPhrase string

	Must    []Constraint
	Should  []Constraint
	MustNot []Constraint

	Filters SearchFilters

	Limit     int
	Diversity float64
}

// SearchRequest is the API-facing structured search request (POST
// /api/v1/search/shots). Mode maps to SearchIntent; "" and "auto" both mean
// "let the router decide".
type SearchRequest struct {
	Query string `json:"query"`
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
	// Offset pages the final ranked result list, AFTER selection/diversity —
	// it never trims the recall pool. A request with offset+limit within the
	// selected list pages through it; an offset beyond the list yields empty
	// results. Negative or zero means no offset.
	Offset          int                `json:"offset"`
	Diversity       float64            `json:"diversity"`
	IncludeEvidence bool               `json:"include_evidence"`
	IncludeContext  bool               `json:"include_context"`
	Facets          domain.FacetFilter `json:"facets,omitempty"`
}

// DefaultLimit applies when SearchRequest.Limit is <= 0.
const DefaultLimit = 20

// Options are the engine-wide settings the Hub wires once at construction.
type Options struct {
	Selection SelectionOptions
	// GateFact turns the evidence gate on for fact/negative intents. When
	// false the gate still computes evidence (for the response) but never
	// filters or downranks. True is the product default.
	GateFact bool
	// Fusion selects the active fusion strategy; nil means RRF (k=60). The
	// field exists so the benchmark can compare weighted blend vs RRF over
	// identical recall.
	Fusion FusionStrategy
	// Embedder is the optional text-embedding provider. When nil (not
	// configured) the text_embedding channel degrades to a no-op, exactly
	// like the transcript channel without aligned words — recall keeps
	// working, the channel just has nothing to say.
	Embedder TextEmbedder
	// ProfileVersion is stamped into query_hash so a vocabulary or profile
	// change invalidates cached feedback keys. Bump it on any change to
	// vocabulary.go, profile.go or the router.
	ProfileVersion string
}

// DefaultOptions returns the measured default settings.
func DefaultOptions() Options {
	return Options{
		Selection:      DefaultSelectionOptions(),
		GateFact:       true,
		Fusion:         nil, // RRF with k=60
		ProfileVersion: "v2-profile-1",
	}
}

// SearchQueryInfo is the echo of how the query was understood.
type SearchQueryInfo struct {
	Raw    string       `json:"raw"`
	Intent SearchIntent `json:"intent"`
}

// SearchResponse is the structured v2 search response. SearchID and QueryHash
// exist so future feedback hooks can correlate a result with the query and
// session that produced it (spec: feedback hooks round).
type SearchResponse struct {
	Query     SearchQueryInfo `json:"query"`
	SearchID  string          `json:"search_id"`
	QueryHash string          `json:"query_hash"`
	Results   []ResultItem    `json:"results"`
}

// ResultItem is one selected shot with its fused score, per-signal scores,
// evidence verdicts and (optionally) timeline neighbours. The shot text fields
// (description/tags/objects/actions/mood) are repeated here so a result is
// renderable and inspectable without a second round trip.
type ResultItem struct {
	ShotID      string   `json:"shot_id"`
	AssetID     string   `json:"asset_id"`
	Filename    string   `json:"filename,omitempty"`
	StartMS     int64    `json:"start_ms"`
	EndMS       int64    `json:"end_ms"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Objects     []string `json:"objects,omitempty"`
	Actions     []string `json:"actions,omitempty"`
	Mood        []string `json:"mood,omitempty"`
	Score       float64  `json:"score"`
	Rank        int      `json:"rank"`
	// Scores carries the per-signal scores (lexical, heuristic_semantic,
	// transcript, metadata) plus the fused value under the fusion strategy's
	// name ("blend" or "rrf").
	Scores   map[string]float64 `json:"scores,omitempty"`
	Evidence []Evidence         `json:"evidence,omitempty"`
	Context  *ShotContext       `json:"context,omitempty"`
}

// ShotContext carries the neighbouring shots of a matched shot. Neighbour
// metadata never participates in scoring or evidence: it exists so a UI or
// editing agent can show [前镜头][命中镜头][后镜头].
type ShotContext struct {
	PreviousShot *ContextShot `json:"previous_shot,omitempty"`
	NextShot     *ContextShot `json:"next_shot,omitempty"`
}

// ContextShot is the minimal projection of a neighbouring shot.
type ContextShot struct {
	ShotID      string `json:"shot_id"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Description string `json:"description,omitempty"`
}

// EvidenceSource names where an observation came from. visual/ocr exist so
// future channels join without API churn; this round they are never produced.
type EvidenceSource string

const (
	SourceObjects     EvidenceSource = "objects"
	SourceActions     EvidenceSource = "actions"
	SourceTags        EvidenceSource = "tags"
	SourceMood        EvidenceSource = "mood"
	SourceDescription EvidenceSource = "description"
	SourceTranscript  EvidenceSource = "transcript"
	SourceVisual      EvidenceSource = "visual"
	SourceOCR         EvidenceSource = "ocr"
	SourceMetadata    EvidenceSource = "metadata"
)

// EvidenceState is how much support a shot actually has for a constraint.
//
//	confirmed    - the canonical term appears in a structured observation
//	               field (objects/actions/tags/mood), optionally plus
//	               description
//	possible     - the term appears only in description (narrative) or in
//	               transcript (speech mentions it; that is not a visual claim)
//	contradicted - the shot's text explicitly negates the constraint
//	unknown      - nothing found; NOT "confirmed absent"
//
// For MustNot constraints (Evidence.Negated = true) the states read the other
// way: confirmed = the forbidden term WAS observed in this shot (it is why
// the shot would be excluded), unknown = not observed. Absence is never
// asserted: unknown is not "确定无人".
type EvidenceState string

const (
	EvidenceConfirmed    EvidenceState = "confirmed"
	EvidencePossible     EvidenceState = "possible"
	EvidenceContradicted EvidenceState = "contradicted"
	EvidenceUnknown      EvidenceState = "unknown"
)

// Evidence is the per-constraint verdict for one shot.
type Evidence struct {
	Constraint ConstraintType   `json:"constraint_type"`
	Value      string           `json:"constraint"`
	State      EvidenceState    `json:"state"`
	Sources    []EvidenceSource `json:"sources,omitempty"`
	Negated    bool             `json:"negated,omitempty"`
}

// Candidate is one shot under consideration with its per-signal scores.
// Signals map uses the Signal* constants. Score is assigned by the active
// fusion strategy; candidates keep their shot text fields so the evidence
// gate can evaluate them without another store round trip.
type Candidate struct {
	ShotID      string
	AssetID     string
	Filename    string
	Ordinal     int
	StartMS     int64
	EndMS       int64
	Description string
	Tags        []string
	Objects     []string
	Actions     []string
	Mood        []string
	Confidence  float64
	SessionID   string
	Signals     map[string]float64
	Score       float64
}

// Signal names. heuristic_semantic is deliberately the honest name for the
// deterministic FNV feature hash — see discovery.HeuristicVectorModel.
const (
	SignalLexical           = "lexical"
	SignalHeuristicSemantic = "heuristic_semantic"
	SignalTranscript        = "transcript"
	SignalMetadata          = "metadata"
	SignalTextEmbedding     = "text_embedding"
	SignalRRF               = "rrf"
	SignalBlend             = "blend"
)

// ChannelResult is one retrieval channel's ranked output. Fusion consumes the
// list of ChannelResults (one per channel/signal).
type ChannelResult struct {
	Signal     string
	Candidates []Candidate
}

// CandidateRetriever is one logical retrieval channel. Each channel ranks the
// same candidate universe against one signal and reports it in
// Candidate.Signals under its Name().
type CandidateRetriever interface {
	Name() string
	Retrieve(ctx context.Context, q SearchQuery, limit int) ([]Candidate, error)
}

// FusionStrategy merges per-channel ranked lists into one scored candidate
// list. WeightedBlend and RRF implement it; the active one is chosen per
// request by the service.
type FusionStrategy interface {
	// Name returns the signal key the fused score is stored under
	// (SignalBlend or SignalRRF).
	Name() string
	Fuse(results []ChannelResult) []Candidate
}

// Reranker is a post-fusion, post-gate optional pass that only ever sees a
// bounded top-K (recall top N -> rerank top M -> return top L). The default
// is NoneReranker; real text/multimodal rerankers plug in later.
type Reranker interface {
	Name() string
	Rerank(ctx context.Context, q SearchQuery, candidates []Candidate) ([]Candidate, error)
}

// TextEmbedder is the text-embedding provider boundary the engine consumes.
// Its batch shape matches providers.Embedder exactly (the OpenAI-compatible
// and Gemini adapters satisfy it structurally — no adapter code). Model()
// must return the same identifier that gets persisted with embeddings so a
// model change is detected rather than silently cross-scored: vectors written
// under one model are never read under another.
//
// The embedder embeds strings only; it never sees assets, shots or the
// database. That boundary is what keeps the vector layer from turning into
// an opaque asset search path.
type TextEmbedder interface {
	Name() string
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// ShotSearchDocument is the derived text representation of a shot that gets
// embedded. It is deliberately NOT the canonical shot: embeddings are
// derived, rebuildable, replaceable. Transcript is absent on purpose — speech
// retrieval belongs to the transcript channel, and letting speech into the
// embedding text would blur "visually described" and "spoken".
type ShotSearchDocument struct {
	ShotID      string
	Description string
	Tags        []string
	Objects     []string
	Actions     []string
	Mood        []string
}

// SelectionOptions tunes the diversity pass. Penalties are subtracted from a
// candidate's fused score when it repeats a dimension an already-selected
// shot covers; Diversity (0..1) scales all penalties.
type SelectionOptions struct {
	Diversity          float64
	SameAssetPenalty   float64
	SameSessionPenalty float64
	NearTimePenalty    float64
	NearTimeWindowMS   int64
}

// DefaultSelectionOptions returns the starting penalties. Same-asset and
// near-time penalties stop a burst of near-duplicate shots (A001 10.1s, 10.9s,
// 11.5s ...) from occupying the whole top-10; SameSessionPenalty spreads
// creative mode across different shooting sessions.
func DefaultSelectionOptions() SelectionOptions {
	return SelectionOptions{
		Diversity:          0.2,
		SameAssetPenalty:   0.25,
		SameSessionPenalty: 0.20,
		NearTimePenalty:    0.35,
		NearTimeWindowMS:   5000,
	}
}

// SearchResponseMeta is reserved for future pagination/feedback metadata.
type SearchResponseMeta struct {
	GeneratedAt time.Time `json:"generated_at"`
}
