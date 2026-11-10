package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

const (
	ModalityLexical    = "lexical"
	ModalityTranscript = "transcript"
	ModalityVisual     = "visual"
	ModalityAudio      = "audio"
	ModalitySoundEvent = "sound_event"
	ModalitySpeaker    = "speaker"

	SignalVisual     = ModalityVisual
	SignalAudio      = ModalityAudio
	SignalSoundEvent = ModalitySoundEvent
	SignalSpeaker    = ModalitySpeaker
)

// TemporalCondition names one conjunctive observation requested by a temporal
// search. Query is the condition's text, not a guessed relation.
type TemporalCondition struct {
	ID             string `json:"id"`
	Modality       string `json:"modality"`
	Query          string `json:"query"`
	AssetID        string `json:"asset_id,omitempty"`
	LocalSpeakerID string `json:"local_speaker_id,omitempty"`
}

// TemporalPredicate expresses one relation between two condition records.
type TemporalPredicate struct {
	ID       string `json:"id"`
	Relation string `json:"relation"`
	LeftID   string `json:"left_id"`
	RightID  string `json:"right_id"`
	MaxGapMS int64  `json:"max_gap_ms,omitempty"`
}

// TemporalQuery is an explicitly constrained temporal request. Interpretation
// retains the matched user template verbatim so an API can echo what it parsed.
type TemporalQuery struct {
	Conditions     []TemporalCondition `json:"conditions"`
	Predicates     []TemporalPredicate `json:"predicates"`
	Interpretation string              `json:"interpretation,omitempty"`
}

// SpeakerFilter selects one asset-local diarized speaker or an opted-in
// cross-recording profile. The two forms are mutually exclusive.
type SpeakerFilter struct {
	AssetID                 string `json:"asset_id,omitempty"`
	DiarizationGenerationID string `json:"diarization_generation_id,omitempty"`
	LocalSpeakerID          string `json:"local_speaker_id,omitempty"`
	ProfileID               string `json:"profile_id,omitempty"`
}

// MultimodalRequest carries optional retrieval channels and temporal/speaker
// constraints. It is an extension only; callers that omit it retain legacy
// search behavior.
type MultimodalRequest struct {
	Modalities    []string       `json:"modalities,omitempty"`
	ReferenceID   string         `json:"reference_id,omitempty"`
	VisualShotID  string         `json:"visual_shot_id,omitempty"`
	Temporal      *TemporalQuery `json:"temporal,omitempty"`
	SpeakerFilter *SpeakerFilter `json:"speaker_filter,omitempty"`
}

var ErrConflictingReferences = errors.New("search: reference_id and visual_shot_id conflict")

var ErrInvalidMultimodal = errors.New("search: invalid multimodal request")

// ErrModalitiesUnavailable identifies an explicitly requested channel that
// cannot be queried with the configured search capabilities.
var ErrModalitiesUnavailable = errors.New("search: requested modality is not available")

// ModalityQueryEncoder creates a query vector in a modality-specific space.
type ModalityQueryEncoder interface {
	Encode(ctx context.Context, modality, text, referenceID string) (spaceID string, vector []float32, err error)
}

// ConflictingReferencesError is a typed request error so an HTTP layer can map
// conflicting reference selectors to 400. Both selectors may be absent; when
// supplied together, they identify competing visual references and are rejected.
type ConflictingReferencesError struct {
	ReferenceID  string
	VisualShotID string
}

func (e *ConflictingReferencesError) Error() string {
	return ErrConflictingReferences.Error()
}

func (e *ConflictingReferencesError) Is(target error) bool {
	return target == ErrConflictingReferences
}

// ValidateMultimodal validates the new request fields without changing the
// behavior of requests that omit them. References are optional; reference_id
// and visual_shot_id are conflicting selectors when both are non-empty.
func ValidateMultimodal(req MultimodalRequest) error {
	if req.ReferenceID != "" && req.VisualShotID != "" {
		return &ConflictingReferencesError{ReferenceID: req.ReferenceID, VisualShotID: req.VisualShotID}
	}
	for _, modality := range req.Modalities {
		if !validModality(modality) {
			return fmt.Errorf("search: unknown modality %q", modality)
		}
	}
	needsSpeakerFilter := false
	for _, modality := range req.Modalities {
		if modality == ModalitySpeaker {
			needsSpeakerFilter = true
			break
		}
	}
	if req.Temporal != nil {
		for _, condition := range req.Temporal.Conditions {
			if condition.Modality == ModalitySpeaker {
				needsSpeakerFilter = true
				break
			}
		}
	}
	if needsSpeakerFilter && req.SpeakerFilter == nil {
		return errors.New("search: speaker modality requires speaker_filter")
	}
	if req.Temporal != nil {
		if err := validateTemporalQuery(*req.Temporal); err != nil {
			return err
		}
	}
	if req.SpeakerFilter != nil && !validSpeakerFilter(*req.SpeakerFilter) {
		return errors.New("search: speaker_filter must identify exactly one asset-local speaker or one profile")
	}
	return nil
}

func validModality(modality string) bool {
	switch modality {
	case ModalityLexical, ModalityTranscript, ModalityVisual, ModalityAudio, ModalitySoundEvent, ModalitySpeaker:
		return true
	default:
		return false
	}
}

func validateTemporalQuery(query TemporalQuery) error {
	if len(query.Conditions) > 8 {
		return fmt.Errorf("search: temporal query has %d conditions; maximum is 8", len(query.Conditions))
	}
	if len(query.Predicates) > 16 {
		return fmt.Errorf("search: temporal query has %d predicates; maximum is 16", len(query.Predicates))
	}
	// Identity errors come first: a duplicate condition or predicate id is a
	// structural mistake, and reporting "query must not be empty" for it hides
	// the actual problem.
	conditions := make(map[string]struct{}, len(query.Conditions))
	for _, condition := range query.Conditions {
		if strings.TrimSpace(condition.ID) == "" {
			return errors.New("search: temporal condition ID must not be empty")
		}
		if _, exists := conditions[condition.ID]; exists {
			return fmt.Errorf("search: duplicate temporal condition ID %q", condition.ID)
		}
		conditions[condition.ID] = struct{}{}
	}
	predicateIDs := make(map[string]struct{}, len(query.Predicates))
	for _, predicate := range query.Predicates {
		if strings.TrimSpace(predicate.ID) == "" {
			return errors.New("search: temporal predicate ID must not be empty")
		}
		if _, exists := predicateIDs[predicate.ID]; exists {
			return fmt.Errorf("search: duplicate temporal predicate ID %q", predicate.ID)
		}
		predicateIDs[predicate.ID] = struct{}{}
	}
	for _, condition := range query.Conditions {
		if !validModality(condition.Modality) {
			return fmt.Errorf("search: unknown temporal condition modality %q", condition.Modality)
		}
		if strings.TrimSpace(condition.Query) == "" {
			return fmt.Errorf("search: temporal condition %q query must not be empty", condition.ID)
		}
	}
	for _, predicate := range query.Predicates {
		if _, exists := conditions[predicate.LeftID]; !exists {
			return fmt.Errorf("search: temporal predicate %q has unknown left condition %q", predicate.ID, predicate.LeftID)
		}
		if _, exists := conditions[predicate.RightID]; !exists {
			return fmt.Errorf("search: temporal predicate %q has unknown right condition %q", predicate.ID, predicate.RightID)
		}
		if !validTemporalRelation(predicate.Relation) {
			return fmt.Errorf("search: unknown temporal relation %q", predicate.Relation)
		}
		if predicate.MaxGapMS < 0 {
			return fmt.Errorf("search: temporal predicate %q has a negative max_gap_ms", predicate.ID)
		}
		if (predicate.Relation == "before" || predicate.Relation == "after") && predicate.MaxGapMS <= 0 {
			return fmt.Errorf("search: temporal predicate %q relation %q requires max_gap_ms > 0", predicate.ID, predicate.Relation)
		}
	}
	return nil
}

func validSpeakerFilter(filter SpeakerFilter) bool {
	assetForm := nonblank(filter.AssetID) && nonblank(filter.DiarizationGenerationID) && nonblank(filter.LocalSpeakerID) && filter.ProfileID == ""
	profileForm := nonblank(filter.ProfileID) && filter.AssetID == "" && filter.DiarizationGenerationID == "" && filter.LocalSpeakerID == ""
	return assetForm || profileForm
}

func nonblank(value string) bool { return strings.TrimSpace(value) != "" }

// ModalityWindow is one matching timeline record returned by a modality store.
type ModalityWindow struct {
	AssetID        string   `json:"asset_id"`
	ShotID         string   `json:"shot_id"`
	RecordID       string   `json:"record_id"`
	Modality       string   `json:"modality"`
	StartMS        int64    `json:"start_ms"`
	EndMS          int64    `json:"end_ms"`
	Score          float64  `json:"score"`
	Label          string   `json:"label,omitempty"`
	EvidenceStatus string   `json:"evidence_status,omitempty"`
	EvidenceSource string   `json:"evidence_source,omitempty"`
	ModelID        string   `json:"model_id,omitempty"`
	ModelVersion   string   `json:"model_version,omitempty"`
	AnalysisRunID  string   `json:"analysis_run_id,omitempty"`
	Confidence     *float64 `json:"confidence"`
}

// ModalityStore is the read-only retrieval boundary for non-legacy channels.
// Persistence and SQL remain outside internal/search.
type ModalityStore interface {
	FilteredShotUniverse(ctx context.Context, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) (map[string]bool, error)
	VisualWindows(ctx context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]ModalityWindow, error)
	AudioWindows(ctx context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]ModalityWindow, error)
	SoundEvents(ctx context.Context, labels []string, universe map[string]bool, limit int) ([]ModalityWindow, error)
	SpeakerTurns(ctx context.Context, filter SpeakerFilter, universe map[string]bool, limit int) ([]ModalityWindow, error)
}

// ModalityRetriever adapts visual, audio, sound-event and speaker records to
// the existing candidate channel contract. Call RetrieveWithWindows when the
// caller also needs the full matching record intervals: Candidate.Signals can
// carry scores only in the existing search API, so those intervals are returned
// alongside the deduplicated candidates rather than encoded as fake signals.
type ModalityRetriever struct {
	Store         ModalityStore
	Modality      string
	SpaceID       string
	Vector        []float32
	SoundLabels   []string
	SpeakerFilter SpeakerFilter
	Universe      map[string]bool
	UniverseReady bool
}

var _ CandidateRetriever = (*ModalityRetriever)(nil)

func NewModalityRetriever(store ModalityStore, modality, spaceID string, vector []float32) *ModalityRetriever {
	return &ModalityRetriever{Store: store, Modality: modality, SpaceID: spaceID, Vector: vector}
}

func (r *ModalityRetriever) Name() string {
	if r == nil {
		return ""
	}
	return r.Modality
}

func (r *ModalityRetriever) Retrieve(ctx context.Context, query SearchQuery, limit int) ([]Candidate, error) {
	candidates, _, err := r.RetrieveWithWindows(ctx, query, limit)
	return candidates, err
}

// RetrieveWithWindows returns one maximum-score candidate per shot and every
// fetched matching interval for those returned shots. The interval list stays
// one record per matching window, including multiple records from one shot.
func (r *ModalityRetriever) RetrieveWithWindows(ctx context.Context, query SearchQuery, limit int) ([]Candidate, []ModalityWindow, error) {
	if r == nil || r.Store == nil {
		return nil, nil, errors.New("search: modality store not wired")
	}
	if !isRetrievableModality(r.Modality) {
		return nil, nil, fmt.Errorf("search: modality %q is not a multimodal retrieval channel", r.Modality)
	}
	if limit <= 0 {
		return []Candidate{}, []ModalityWindow{}, nil
	}
	labels := []string(nil)
	if r.Modality == ModalitySoundEvent {
		labels = soundEventLabels(query.Raw, r.SoundLabels)
		if len(labels) == 0 {
			return []Candidate{}, []ModalityWindow{}, nil
		}
	}
	if (r.Modality == ModalityVisual || r.Modality == ModalityAudio) && (r.SpaceID == "" || len(r.Vector) == 0) {
		return nil, nil, fmt.Errorf("search: %s retrieval requires a space ID and query vector", r.Modality)
	}
	var err error
	var universe map[string]bool
	if r.UniverseReady {
		universe = r.Universe
	} else {
		universe, err = r.Store.FilteredShotUniverse(ctx, query.Filters.Facets, query.Filters.AssetFilter)
		if err != nil {
			return nil, nil, err
		}
	}
	var windows []ModalityWindow
	switch r.Modality {
	case ModalityVisual:
		windows, err = r.Store.VisualWindows(ctx, r.SpaceID, r.Vector, universe, limit)
	case ModalityAudio:
		windows, err = r.Store.AudioWindows(ctx, r.SpaceID, r.Vector, universe, limit)
	case ModalitySoundEvent:
		windows, err = r.Store.SoundEvents(ctx, labels, universe, limit)
	case ModalitySpeaker:
		windows, err = r.Store.SpeakerTurns(ctx, r.SpeakerFilter, universe, limit)
	}
	if err != nil {
		return nil, nil, err
	}
	candidates, matchingWindows := deduplicateModalityWindows(windows, r.Modality, limit)
	return candidates, matchingWindows, nil
}

func isRetrievableModality(modality string) bool {
	return modality == ModalityVisual || modality == ModalityAudio || modality == ModalitySoundEvent || modality == ModalitySpeaker
}

func deduplicateModalityWindows(windows []ModalityWindow, modality string, limit int) ([]Candidate, []ModalityWindow) {
	bestByShot := make(map[string]ModalityWindow, len(windows))
	allByShot := make(map[string][]ModalityWindow, len(windows))
	for _, window := range windows {
		if window.ShotID == "" {
			continue
		}
		if window.Modality == "" {
			window.Modality = modality
		}
		allByShot[window.ShotID] = append(allByShot[window.ShotID], window)
		best, exists := bestByShot[window.ShotID]
		if !exists || window.Score > best.Score || (window.Score == best.Score && windowLess(window, best)) {
			bestByShot[window.ShotID] = window
		}
	}
	candidates := make([]Candidate, 0, len(bestByShot))
	for shotID, window := range bestByShot {
		signal := modality
		candidates = append(candidates, Candidate{
			ShotID:  shotID,
			AssetID: window.AssetID,
			StartMS: window.StartMS,
			EndMS:   window.EndMS,
			Signals: map[string]float64{signal: window.Score},
			Score:   window.Score,
		})
	}
	sortCandidates(candidates)
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	selected := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		selected[candidate.ShotID] = true
	}
	keptWindows := make([]ModalityWindow, 0, len(windows))
	for shotID := range selected {
		keptWindows = append(keptWindows, allByShot[shotID]...)
	}
	sort.Slice(keptWindows, func(i, j int) bool {
		if keptWindows[i].ShotID != keptWindows[j].ShotID {
			return keptWindows[i].ShotID < keptWindows[j].ShotID
		}
		return windowLess(keptWindows[i], keptWindows[j])
	})
	return candidates, keptWindows
}

func windowLess(a, b ModalityWindow) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	if a.EndMS != b.EndMS {
		return a.EndMS < b.EndMS
	}
	if a.RecordID != b.RecordID {
		return a.RecordID < b.RecordID
	}
	if a.AssetID != b.AssetID {
		return a.AssetID < b.AssetID
	}
	return a.Modality < b.Modality
}

func soundEventLabels(query string, extra []string) []string {
	words := make([]string, 0, len(extra)+1)
	words = append(words, query)
	words = append(words, extra...)
	labels := SoundLabelVocabulary(words)
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// SoundLabelVocabulary maps a deliberately small starter vocabulary onto the
// YAMNet/AudioSet canonical class IDs. These are retrieval labels, not claims of
// calibrated confidence. English matching respects word boundaries; Chinese
// aliases are matched as substrings. The returned IDs are sorted and unique.
//
// AudioSet IDs used here come from the YAMNet class map: rain, thunder, vehicle
// horn, siren, speech, music, dog, glass, footsteps, wind, water and engine.
func SoundLabelVocabulary(words []string) []string {
	matches := make(map[string]struct{})
	for _, word := range words {
		text := strings.ToLower(strings.TrimSpace(word))
		if text == "" {
			continue
		}
		for _, entry := range soundLabelAliases {
			for _, alias := range entry.aliases {
				if containsSoundAlias(text, alias) {
					matches[entry.id] = struct{}{}
					break
				}
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for id := range matches {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type soundLabelAlias struct {
	id      string
	aliases []string
}

var soundLabelAliases = []soundLabelAlias{
	{id: "/m/06mb1", aliases: []string{"rain", "rainfall", "raining", "下雨", "雨声", "雨"}},
	{id: "/m/0ngt1", aliases: []string{"thunder", "thunderstorm", "雷声", "打雷", "雷"}},
	{id: "/m/0912c9", aliases: []string{"vehicle horn", "car horn", "horn", "honking", "鸣笛", "喇叭", "汽车喇叭"}},
	{id: "/m/03kmc9", aliases: []string{"siren", "police siren", "ambulance siren", "警笛", "警报器", "救护车警笛"}},
	{id: "/m/09x0r", aliases: []string{"speech", "speaking", "talking", "conversation", "人声", "说话", "讲话", "对话", "语音"}},
	{id: "/m/04rlf", aliases: []string{"music", "song", "musical", "音乐", "歌曲", "歌声"}},
	{id: "/m/0bt9lr", aliases: []string{"dog", "dogs", "bark", "barking", "狗", "犬", "狗叫"}},
	{id: "/m/039jq", aliases: []string{"glass", "玻璃"}},
	{id: "/m/07rn7sz", aliases: []string{"breaking glass", "glass break", "shattering glass", "glass shatter", "玻璃破碎", "玻璃碎裂"}},
	{id: "/m/07pbtc8", aliases: []string{"footstep", "footsteps", "walking", "walk", "steps", "脚步声", "脚步", "走路"}},
	{id: "/m/03m9d0z", aliases: []string{"wind", "windy", "breeze", "风声", "风"}},
	{id: "/m/0838f", aliases: []string{"water", "flowing water", "流水", "水声", "水"}},
	{id: "/m/02mk9", aliases: []string{"engine", "motor", "idling", "revving", "发动机", "引擎", "马达"}},
}

func containsSoundAlias(text, alias string) bool {
	if alias == "" {
		return false
	}
	if containsCJK(alias) {
		return strings.Contains(text, alias)
	}
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], alias)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(alias)
		leftOK := start == 0 || !isSoundWordRune(runeBefore(text, start))
		rightOK := end == len(text) || !isSoundWordRune(runeAt(text, end))
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func containsCJK(value string) bool {
	for _, r := range value {
		if r >= '\u3400' && r <= '\u9fff' {
			return true
		}
	}
	return false
}

func isSoundWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func runeBefore(value string, byteIndex int) rune {
	if byteIndex <= 0 || byteIndex > len(value) {
		return 0
	}
	r, _ := utf8.DecodeLastRuneInString(value[:byteIndex])
	return r
}

func runeAt(value string, byteIndex int) rune {
	if byteIndex < 0 || byteIndex >= len(value) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(value[byteIndex:])
	return r
}

// Match is one condition's timed supporting record, with retrieval and model
// provenance kept separate from calibrated confidence.
type Match struct {
	ConditionID     string   `json:"condition_id"`
	Modality        string   `json:"modality"`
	RecordID        string   `json:"record_id"`
	AssetID         string   `json:"asset_id"`
	StartMS         int64    `json:"start_ms"`
	EndMS           int64    `json:"end_ms"`
	RetrievalScore  float64  `json:"retrieval_score"`
	ModelConfidence *float64 `json:"model_confidence"`
	EvidenceStatus  string   `json:"evidence_status"`
	EvidenceSource  string   `json:"evidence_source"`
	ModelID         string   `json:"model_id"`
	ModelVersion    string   `json:"model_version"`
	AnalysisRunID   string   `json:"analysis_run_id"`
}

// Interval is the shared half-open asset-relative millisecond interval type.
type Interval = domain.TimeInterval

// Coverage describes how much of a requested modality was indexed or searched.
type Coverage struct {
	Modality  string     `json:"modality"`
	State     string     `json:"state"`
	Intervals []Interval `json:"intervals"`
}

// BuildMatches projects windows into public match records, deduplicating by
// (condition_id, record_id). When duplicate records disagree, the highest score
// wins; ties are resolved by a stable interval/provenance order.
func BuildMatches(conditionID string, windows []ModalityWindow) []Match {
	best := make(map[string]Match, len(windows))
	for _, window := range windows {
		match := Match{
			ConditionID:     conditionID,
			Modality:        window.Modality,
			RecordID:        window.RecordID,
			AssetID:         window.AssetID,
			StartMS:         window.StartMS,
			EndMS:           window.EndMS,
			RetrievalScore:  window.Score,
			ModelConfidence: window.Confidence,
			EvidenceStatus:  window.EvidenceStatus,
			EvidenceSource:  window.EvidenceSource,
			ModelID:         window.ModelID,
			ModelVersion:    window.ModelVersion,
			AnalysisRunID:   window.AnalysisRunID,
		}
		key := conditionID + "\x00" + match.Modality + "\x00" + match.RecordID
		old, exists := best[key]
		if !exists || match.RetrievalScore > old.RetrievalScore || (match.RetrievalScore == old.RetrievalScore && matchLess(match, old)) {
			best[key] = match
		}
	}
	matches := make([]Match, 0, len(best))
	for _, match := range best {
		matches = append(matches, match)
	}
	sort.Slice(matches, func(i, j int) bool { return matchLess(matches[i], matches[j]) })
	return matches
}

func matchLess(a, b Match) bool {
	if a.StartMS != b.StartMS {
		return a.StartMS < b.StartMS
	}
	if a.EndMS != b.EndMS {
		return a.EndMS < b.EndMS
	}
	if a.ConditionID != b.ConditionID {
		return a.ConditionID < b.ConditionID
	}
	if a.RecordID != b.RecordID {
		return a.RecordID < b.RecordID
	}
	if a.Modality != b.Modality {
		return a.Modality < b.Modality
	}
	return a.AssetID < b.AssetID
}

// MergeCoverage joins coverage entries by modality and normalizes their interval
// union. Conflicting states become partial rather than overstating completeness.
func MergeCoverage(coverage ...Coverage) []Coverage {
	byModality := make(map[string]Coverage, len(coverage))
	for _, item := range coverage {
		merged, exists := byModality[item.Modality]
		if !exists {
			merged = Coverage{Modality: item.Modality, State: item.State}
		} else if merged.State != item.State {
			merged.State = mergeCoverageState(merged.State, item.State)
		}
		merged.Intervals = append(merged.Intervals, item.Intervals...)
		byModality[item.Modality] = merged
	}
	out := make([]Coverage, 0, len(byModality))
	for _, item := range byModality {
		intervals := domain.IntervalSet(item.Intervals).Normalize()
		item.Intervals = []Interval(intervals)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modality < out[j].Modality })
	return out
}

func mergeCoverageState(a, b string) string {
	if a == b {
		return a
	}
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return "partial"
}

// PendingModalitiesError reports that a requested channel is being computed and
// carries the job ids the caller should poll. It is deliberately distinct from
// ErrModalitiesUnavailable: one means "wait", the other means "this Hub cannot
// serve the channel at all".
type PendingModalitiesError struct {
	JobIDs []string
	Reason string
}

// Error renders a stable message without the job ids (they travel in JobIDs).
func (e *PendingModalitiesError) Error() string {
	if e == nil {
		return ErrModalitiesUnavailable.Error()
	}
	message := strings.TrimSpace(e.Reason)
	if message == "" {
		message = "requested modality is still being encoded"
	}
	return message
}
