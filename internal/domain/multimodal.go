// Package domain: multimodal inference contracts.
//
// These types are the single vocabulary shared by the Hub, the Worker and the
// Python inference process. Everything here is pure data plus validation: no
// I/O, no model knowledge. The boundary rules they encode are the reason the
// file exists as a separate contract instead of living inside the pipeline:
//
//   - intervals are half-open, asset-relative integer milliseconds
//     [start_ms,end_ms), so two adjacent events never claim the same instant;
//   - a request names a model and its immutable revision, so a lease granted
//     for one artifact can never be executed against another;
//   - external callers never supply executable paths, URLs or output
//     locations — only Hub-issued references.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// InferenceSchemaVersion is the wire schema of every inference request and
// result. A revision that changes a field's meaning must bump it; adding an
// optional field does not.
const InferenceSchemaVersion = "inference.v1"

// InferenceOperation names one unit of work an inference executor can perform.
// The set is closed: an unknown operation is a protocol error, never a reason
// to guess a near neighbour.
type InferenceOperation string

const (
	OpVisualEmbedding  InferenceOperation = "visual_embedding"
	OpSoundEvents      InferenceOperation = "sound_events"
	OpAudioEmbedding   InferenceOperation = "audio_embedding"
	OpDiarization      InferenceOperation = "diarization"
	OpSpeakerEmbedding InferenceOperation = "speaker_embedding"
	OpASR              InferenceOperation = "asr"
	OpAlignment        InferenceOperation = "alignment"
)

// AllInferenceOperations lists the closed operation set in a stable order.
func AllInferenceOperations() []InferenceOperation {
	return []InferenceOperation{
		OpVisualEmbedding, OpSoundEvents, OpAudioEmbedding,
		OpDiarization, OpSpeakerEmbedding, OpASR, OpAlignment,
	}
}

// Valid reports whether o is one of the declared operations.
func (o InferenceOperation) Valid() bool {
	switch o {
	case OpVisualEmbedding, OpSoundEvents, OpAudioEmbedding,
		OpDiarization, OpSpeakerEmbedding, OpASR, OpAlignment:
		return true
	}
	return false
}

// AcceptsTextQuery reports whether the operation can be driven by a text
// query. Everything else needs media bytes.
func (o InferenceOperation) AcceptsTextQuery() bool {
	switch o {
	case OpVisualEmbedding, OpAudioEmbedding:
		return true
	}
	return false
}

// NeedsSpace reports whether the operation writes vectors into an embedding
// space and therefore requires a space identity in the request.
func (o InferenceOperation) NeedsSpace() bool {
	switch o {
	case OpVisualEmbedding, OpAudioEmbedding, OpSpeakerEmbedding:
		return true
	}
	return false
}

// Modality is the timeline track an operation feeds. Sound events are their
// own modality: an environmental event is not a transcript and not speech.
type Modality string

const (
	ModalityVisual     Modality = "visual"
	ModalityAudio      Modality = "audio"
	ModalitySoundEvent Modality = "sound_event"
	ModalitySpeaker    Modality = "speaker"
	ModalityTranscript Modality = "transcript"
	ModalityLexical    Modality = "lexical"
)

// Valid reports whether m is a declared modality.
func (m Modality) Valid() bool {
	switch m {
	case ModalityVisual, ModalityAudio, ModalitySoundEvent, ModalitySpeaker,
		ModalityTranscript, ModalityLexical:
		return true
	}
	return false
}

// InputKind discriminates the three legal inference inputs.
type InputKind string

const (
	InputKindAsset InputKind = "asset"
	InputKindImage InputKind = "image"
	InputKindText  InputKind = "text"
)

// TimeInterval is a half-open [StartMS,EndMS) asset-relative interval.
//
// Half-open matters for the evidence gate: [0,1000) and [1000,2000) touch but
// do not overlap, so a query for "simultaneous" evidence cannot be satisfied
// by two events that merely abut.
type TimeInterval struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// Validate checks 0 <= start < end <= durationMS.
func (iv TimeInterval) Validate(durationMS int64) error {
	if iv.StartMS < 0 {
		return fmt.Errorf("interval start %d is negative", iv.StartMS)
	}
	if iv.EndMS <= iv.StartMS {
		return fmt.Errorf("interval [%d,%d) is empty or reversed", iv.StartMS, iv.EndMS)
	}
	if durationMS > 0 && iv.EndMS > durationMS {
		return fmt.Errorf("interval end %d exceeds duration %d", iv.EndMS, durationMS)
	}
	return nil
}

// Overlaps reports whether the two half-open intervals share any instant.
// Touching intervals do not overlap.
func (iv TimeInterval) Overlaps(other TimeInterval) bool {
	return iv.StartMS < other.EndMS && iv.EndMS > other.StartMS
}

// DurationMS is EndMS-StartMS (never negative for a validated interval).
func (iv TimeInterval) DurationMS() int64 { return iv.EndMS - iv.StartMS }

// Intersect returns the shared interval and whether it is non-empty.
func (iv TimeInterval) Intersect(other TimeInterval) (TimeInterval, bool) {
	start := maxInt64(iv.StartMS, other.StartMS)
	end := minInt64(iv.EndMS, other.EndMS)
	if start >= end {
		return TimeInterval{}, false
	}
	return TimeInterval{StartMS: start, EndMS: end}, true
}

// IntervalSet is an ordered, non-overlapping set of intervals. Used for
// processed/coverage reporting, where overlapping "coverage" would double
// count a millisecond and make a complete generation look partial.
type IntervalSet []TimeInterval

// Normalize sorts and merges touching or overlapping intervals.
func (s IntervalSet) Normalize() IntervalSet {
	if len(s) == 0 {
		return IntervalSet{}
	}
	sorted := append(IntervalSet(nil), s...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StartMS != sorted[j].StartMS {
			return sorted[i].StartMS < sorted[j].StartMS
		}
		return sorted[i].EndMS < sorted[j].EndMS
	})
	out := IntervalSet{sorted[0]}
	for _, iv := range sorted[1:] {
		last := &out[len(out)-1]
		if iv.StartMS <= last.EndMS {
			if iv.EndMS > last.EndMS {
				last.EndMS = iv.EndMS
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// TotalMS returns the covered milliseconds of a normalized set.
func (s IntervalSet) TotalMS() int64 {
	var total int64
	for _, iv := range s {
		total += iv.DurationMS()
	}
	return total
}

// Covers reports whether every millisecond of want lies inside the set.
func (s IntervalSet) Covers(want TimeInterval) bool {
	for _, iv := range s {
		if iv.StartMS <= want.StartMS && iv.EndMS >= want.EndMS {
			return true
		}
	}
	return false
}

// InferenceInput is exactly one of: an asset slice, a Hub-issued image
// reference, or bounded text.
//
// RelativePath is populated only on the Hub->Worker leg and only for asset
// inputs; the HTTP surface rejects a caller-supplied value. Image bytes are
// never carried here — a Worker fetches them through the lease-bound input
// route named by ReferenceID.
type InferenceInput struct {
	Kind         InputKind     `json:"kind"`
	AssetID      string        `json:"asset_id,omitempty"`
	RootID       string        `json:"root_id,omitempty"`
	RelativePath string        `json:"relative_path,omitempty"`
	DurationMS   int64         `json:"duration_ms,omitempty"`
	Interval     *TimeInterval `json:"interval,omitempty"`
	ReferenceID  string        `json:"reference_id,omitempty"`
	Digest       string        `json:"digest,omitempty"`
	Text         string        `json:"text,omitempty"`
}

// MaxQueryTextBytes bounds every text input. Measured in bytes rather than
// runes because the limit exists to bound memory and token counts, both of
// which track bytes for CJK text as well.
const MaxQueryTextBytes = 16 * 1024

// Validate enforces the input discriminant's own rules.
func (in InferenceInput) Validate() error {
	switch in.Kind {
	case InputKindAsset:
		if strings.TrimSpace(in.AssetID) == "" {
			return fmt.Errorf("asset input requires an asset id")
		}
		if in.Text != "" || in.ReferenceID != "" {
			return fmt.Errorf("asset input carries fields of another kind")
		}
		if in.Interval != nil {
			if err := in.Interval.Validate(in.DurationMS); err != nil {
				return err
			}
		}
	case InputKindImage:
		if strings.TrimSpace(in.ReferenceID) == "" {
			return fmt.Errorf("image input requires a reference id")
		}
		if !strings.HasPrefix(in.Digest, "sha256:") {
			return fmt.Errorf("image input requires a sha256 digest")
		}
		if in.Text != "" || in.AssetID != "" || in.RelativePath != "" {
			return fmt.Errorf("image input carries fields of another kind")
		}
	case InputKindText:
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return fmt.Errorf("text input is empty")
		}
		if len(in.Text) > MaxQueryTextBytes {
			return fmt.Errorf("text input exceeds %d bytes", MaxQueryTextBytes)
		}
		if in.AssetID != "" || in.ReferenceID != "" || in.RelativePath != "" {
			return fmt.Errorf("text input carries fields of another kind")
		}
	default:
		return fmt.Errorf("unknown input kind %q", in.Kind)
	}
	return nil
}

// VisualSettings configures one visual embedding call. Sampling decisions are
// made by the Hub (Go) and travel as timestamps; the executor only extracts the
// named instants. That keeps one sampling policy for Hub-local and Worker
// execution and means the request never carries a filesystem path chosen by a
// caller.
type VisualSettings struct {
	Strategy    string       `json:"strategy"`
	ShotSetHash string       `json:"shot_set_hash"`
	Shots       []VisualShot `json:"shots"`
	// TruncateTo requests a Matryoshka dimension. Zero means the model's full
	// output. A value the model does not declare is unsupported_input.
	TruncateTo int `json:"truncate_to"`
	// NativeVideoMaxFrames bounds a native-video encode; zero means the
	// profile's measured default. Over-limit input is unsupported_input, never
	// a silent fallback to frame pooling.
	NativeVideoMaxFrames int `json:"native_video_max_frames,omitempty"`
}

// VisualShot is one canonical shot and the instants sampled from it.
type VisualShot struct {
	ShotID       string  `json:"shot_id"`
	StartMS      int64   `json:"start_ms"`
	EndMS        int64   `json:"end_ms"`
	TimestampsMS []int64 `json:"timestamps_ms"`
}

// AudioSettings configures one audio embedding call.
type AudioSettings struct {
	WindowMS int `json:"window_ms,omitempty"`
	HopMS    int `json:"hop_ms,omitempty"`
}

// DiarizationSettings bounds a whole-recording diarization call. Chunking
// stays inside the provider; this only states the caller's speaker bound.
type DiarizationSettings struct {
	MaxSpeakers int `json:"max_speakers,omitempty"`
	NumSpeakers int `json:"num_speakers,omitempty"`
}

// SpeakerSettings configures one speaker-embedding call over a turn.
type SpeakerSettings struct {
	Interval *TimeInterval `json:"interval"`
	// ProfileID is the eligible-profile identity a biometric extraction is
	// bound to. Empty for anonymous local comparison.
	ProfileID string `json:"profile_id,omitempty"`
	// DiarizationGenerationID records which diarization produced the turn this
	// extraction came from, so the published vector keeps its provenance even
	// after the profile is edited. Optional for older requests.
	DiarizationGenerationID string `json:"diarization_generation_id,omitempty"`
}

// SoundEventSettings is reserved for detector overrides. It is deliberately
// empty in inference.v1: the thresholds are part of the model config hash, and
// letting a request vary them would let two callers publish incomparable
// events under one config identity.
type SoundEventSettings struct{}

// InferenceRequest is the typed body of one inference operation.
type InferenceRequest struct {
	SchemaVersion string             `json:"schema_version"`
	OperationID   string             `json:"operation_id"`
	Operation     InferenceOperation `json:"operation"`
	ModelID       string             `json:"model_id"`
	ModelRevision string             `json:"model_revision"`
	SpaceID       string             `json:"space_id,omitempty"`
	InputHash     string             `json:"input_hash"`
	ConfigHash    string             `json:"config_hash"`
	TimeoutMS     int64              `json:"timeout_ms"`
	Input         InferenceInput     `json:"input"`

	Visual      *VisualSettings      `json:"visual,omitempty"`
	SoundEvent  *SoundEventSettings  `json:"sound_events,omitempty"`
	Audio       *AudioSettings       `json:"audio,omitempty"`
	Diarization *DiarizationSettings `json:"diarization,omitempty"`
	Speaker     *SpeakerSettings     `json:"speaker,omitempty"`
	ASR         *ASRSettings         `json:"asr,omitempty"`
	Alignment   *AlignmentSettings   `json:"alignment,omitempty"`
}

// ASRSettings and AlignmentSettings exist so the operation-specific settings
// rule is uniform across all seven operations even though the Hub executes
// these two through its existing provider adapters rather than Python.
type ASRSettings struct {
	Language string `json:"language,omitempty"`
}

// AlignmentSettings selects which transcript a forced alignment aligns to.
type AlignmentSettings struct {
	TranscriptGenerationHash string `json:"transcript_generation_hash,omitempty"`
	Language                 string `json:"language,omitempty"`
}

// SettingsForOperation returns the one settings value an operation accepts and
// how many were supplied, so validation can reject "exactly one" violations
// uniformly.
func (r InferenceRequest) settingsCount() int {
	count := 0
	for _, present := range []bool{
		r.Visual != nil, r.SoundEvent != nil, r.Audio != nil,
		r.Diarization != nil, r.Speaker != nil, r.ASR != nil, r.Alignment != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

// Validate enforces the request contract. It never consults a model registry:
// it checks shape and self-consistency, and leaves availability to the
// executor's capabilities.
func (r InferenceRequest) Validate() error {
	if r.SchemaVersion != InferenceSchemaVersion {
		return fmt.Errorf("unsupported inference schema %q", r.SchemaVersion)
	}
	if strings.TrimSpace(r.OperationID) == "" {
		return fmt.Errorf("operation id is required")
	}
	if !r.Operation.Valid() {
		return fmt.Errorf("unsupported operation %q", r.Operation)
	}
	if strings.TrimSpace(r.ModelID) == "" || strings.TrimSpace(r.ModelRevision) == "" {
		return fmt.Errorf("model id and immutable revision are required")
	}
	if strings.TrimSpace(r.ConfigHash) == "" || strings.TrimSpace(r.InputHash) == "" {
		return fmt.Errorf("config and input hashes are required")
	}
	if r.TimeoutMS <= 0 {
		return fmt.Errorf("timeout_ms must be positive")
	}
	if err := r.Input.Validate(); err != nil {
		return err
	}
	if r.Operation.NeedsSpace() && strings.TrimSpace(r.SpaceID) == "" {
		return fmt.Errorf("operation %q requires a space id", r.Operation)
	}
	if r.settingsCount() != 1 {
		return fmt.Errorf("request must carry exactly one operation settings object")
	}
	switch r.Operation {
	case OpVisualEmbedding:
		if r.Visual == nil {
			return fmt.Errorf("visual_embedding requires visual settings")
		}
		if r.Input.Kind == InputKindText {
			if r.Operation.NeedsSpace() && r.Visual.Strategy != "" {
				return fmt.Errorf("text query must not name a sampling strategy")
			}
			return nil
		}
		if r.Input.Kind == InputKindImage {
			return nil
		}
		switch r.Visual.Strategy {
		case "shot_frame_pooled":
			if len(r.Visual.Shots) == 0 {
				return fmt.Errorf("shot_frame_pooled requires at least one shot")
			}
			for _, shot := range r.Visual.Shots {
				if strings.TrimSpace(shot.ShotID) == "" {
					return fmt.Errorf("visual shot lacks an id")
				}
				iv := TimeInterval{StartMS: shot.StartMS, EndMS: shot.EndMS}
				if err := iv.Validate(r.Input.DurationMS); err != nil {
					return fmt.Errorf("visual shot %q: %w", shot.ShotID, err)
				}
				if len(shot.TimestampsMS) == 0 {
					return fmt.Errorf("visual shot %q has no sample instants", shot.ShotID)
				}
				for _, ts := range shot.TimestampsMS {
					if ts < shot.StartMS || ts >= shot.EndMS {
						return fmt.Errorf("visual shot %q samples %d outside [%d,%d)", shot.ShotID, ts, shot.StartMS, shot.EndMS)
					}
				}
			}
		case "native_video":
			if r.Input.Interval == nil {
				return fmt.Errorf("native_video requires an input interval")
			}
		default:
			return fmt.Errorf("unknown visual strategy %q", r.Visual.Strategy)
		}
	case OpSoundEvents:
		if r.SoundEvent == nil {
			return fmt.Errorf("sound_events requires sound-event settings")
		}
		if r.Input.Kind != InputKindAsset {
			return fmt.Errorf("sound_events requires an asset input")
		}
	case OpAudioEmbedding:
		if r.Audio == nil {
			return fmt.Errorf("audio_embedding requires audio settings")
		}
		if r.Input.Kind == InputKindImage {
			return fmt.Errorf("audio_embedding does not accept an image input")
		}
	case OpDiarization:
		if r.Diarization == nil {
			return fmt.Errorf("diarization requires diarization settings")
		}
		if r.Input.Kind != InputKindAsset {
			return fmt.Errorf("diarization requires an asset input")
		}
	case OpSpeakerEmbedding:
		if r.Speaker == nil {
			return fmt.Errorf("speaker_embedding requires speaker settings")
		}
		if r.Input.Kind != InputKindAsset || r.Speaker.Interval == nil {
			return fmt.Errorf("speaker_embedding requires an asset interval")
		}
		if err := r.Speaker.Interval.Validate(r.Input.DurationMS); err != nil {
			return err
		}
	case OpASR, OpAlignment:
		if r.Input.Kind != InputKindAsset {
			return fmt.Errorf("%q requires an asset input", r.Operation)
		}
	}
	return nil
}

// EmbeddingRecord is one vector at one position on the asset timeline.
//
// SampleIndex -1 marks an aggregate record (a shot's pooled mean, or a
// native-video clip embedding); non-negative indices are individual frames.
type EmbeddingRecord struct {
	ShotID      string    `json:"shot_id,omitempty"`
	SampleIndex int       `json:"sample_index"`
	TimestampMS int64     `json:"timestamp_ms,omitempty"`
	StartMS     int64     `json:"start_ms,omitempty"`
	EndMS       int64     `json:"end_ms,omitempty"`
	Strategy    string    `json:"strategy,omitempty"`
	SourceHash  string    `json:"source_hash,omitempty"`
	Vector      []float32 `json:"-"`
}

// SoundEvent is one detected environmental event.
//
// EvidenceStatus is "possible" for every automatic detection: a detector's
// score is not a calibrated probability, so it cannot confirm a claim. Only a
// calibration policy that published a measured threshold may promote an event
// to "confirmed", and that promotion is recorded, not inferred.
type SoundEvent struct {
	Label                string   `json:"label"`
	StartMS              int64    `json:"start_ms"`
	EndMS                int64    `json:"end_ms"`
	RawScore             float64  `json:"raw_score"`
	Confidence           *float64 `json:"confidence"`
	EvidenceStatus       string   `json:"evidence_status"`
	BoundaryResolutionMS int      `json:"boundary_resolution_ms"`
}

// SpeakerTurn is one diarization segment. Turns may overlap: two people
// talking at once is a fact about the recording, not an error to resolve.
type SpeakerTurn struct {
	StartMS        int64    `json:"start_ms"`
	EndMS          int64    `json:"end_ms"`
	LocalSpeakerID string   `json:"local_speaker_id,omitempty"`
	Confidence     *float64 `json:"confidence"`
	Overlap        bool     `json:"overlap"`
}

// InferenceResult is the validated product of one operation.
//
// Exactly one payload field is populated, selected by Operation. Carrying them
// in one struct (rather than an interface) keeps the wire type JSON-friendly
// and lets validation compare "which fields are set" in one place.
type InferenceResult struct {
	SchemaVersion string             `json:"schema_version"`
	OperationID   string             `json:"operation_id"`
	Operation     InferenceOperation `json:"operation"`
	ModelID       string             `json:"model_id"`
	ModelRevision string             `json:"model_revision"`
	SpaceID       string             `json:"space_id,omitempty"`
	ConfigHash    string             `json:"config_hash"`
	InputHash     string             `json:"input_hash"`
	Backend       string             `json:"backend"`
	Coverage      []TimeInterval     `json:"coverage,omitempty"`

	Embeddings []EmbeddingRecord   `json:"embeddings,omitempty"`
	Events     []SoundEvent        `json:"events,omitempty"`
	Turns      []SpeakerTurn       `json:"turns,omitempty"`
	Segments   []TranscriptSegment `json:"segments,omitempty"`
	Words      []AlignmentWord     `json:"words,omitempty"`
}

// ValidateFor checks a result against the request that produced it. The
// dimension is passed in because only the space knows it; zero disables the
// dimension check (used by callers that validate shape only).
func (res InferenceResult) ValidateFor(req InferenceRequest, durationMS int64, dimension int) error {
	if res.SchemaVersion != InferenceSchemaVersion {
		return fmt.Errorf("result schema %q does not match request schema %q", res.SchemaVersion, InferenceSchemaVersion)
	}
	if res.OperationID != req.OperationID {
		return fmt.Errorf("result operation id %q does not match request %q", res.OperationID, req.OperationID)
	}
	if res.Operation != req.Operation {
		return fmt.Errorf("result operation %q does not match request %q", res.Operation, req.Operation)
	}
	if res.ModelID != req.ModelID || res.ModelRevision != req.ModelRevision {
		return fmt.Errorf("result model identity does not match the request")
	}
	if req.Operation.NeedsSpace() && res.SpaceID != req.SpaceID {
		return fmt.Errorf("result space %q does not match request %q", res.SpaceID, req.SpaceID)
	}
	if res.ConfigHash != req.ConfigHash || res.InputHash != req.InputHash {
		return fmt.Errorf("result hashes do not match the request")
	}
	if strings.TrimSpace(res.Backend) == "" {
		return fmt.Errorf("result backend is required")
	}
	for _, iv := range res.Coverage {
		if err := iv.Validate(durationMS); err != nil {
			return fmt.Errorf("coverage: %w", err)
		}
	}

	// Exactly one payload KIND may be present, selected by the operation. The
	// kind may legitimately be empty: a silent recording produces no sound
	// events and a recording with no speech produces no turns, and rejecting
	// those would fail every quiet clip permanently. Vectors are the exception
	// — a vector operation that produced none failed.
	payloads := 0
	for _, present := range []bool{
		len(res.Embeddings) > 0, len(res.Events) > 0, len(res.Turns) > 0,
		len(res.Segments) > 0, len(res.Words) > 0,
	} {
		if present {
			payloads++
		}
	}
	if payloads > 1 {
		return fmt.Errorf("result must carry at most one typed payload, got %d", payloads)
	}
	belongs := func(operation InferenceOperation) bool {
		switch operation {
		case OpVisualEmbedding, OpAudioEmbedding, OpSpeakerEmbedding:
			return len(res.Embeddings) > 0 || payloads == 0
		case OpSoundEvents:
			return len(res.Events) > 0 || payloads == 0
		case OpDiarization:
			return len(res.Turns) > 0 || payloads == 0
		case OpASR:
			return len(res.Segments) > 0 || payloads == 0
		case OpAlignment:
			return len(res.Words) > 0 || payloads == 0
		}
		return false
	}
	if !belongs(req.Operation) {
		return fmt.Errorf("operation %q returned a payload of another type", req.Operation)
	}

	switch req.Operation {
	case OpVisualEmbedding, OpAudioEmbedding, OpSpeakerEmbedding:
		if len(res.Embeddings) == 0 {
			return fmt.Errorf("operation %q returned no embeddings", req.Operation)
		}
		for i, rec := range res.Embeddings {
			if err := validateVector(rec.Vector, dimension, i); err != nil {
				return err
			}
			if rec.SampleIndex < -1 {
				return fmt.Errorf("embedding %d has sample index %d", i, rec.SampleIndex)
			}
			if req.Operation == OpAudioEmbedding {
				iv := TimeInterval{StartMS: rec.StartMS, EndMS: rec.EndMS}
				if err := iv.Validate(durationMS); err != nil {
					return fmt.Errorf("audio window %d: %w", i, err)
				}
			}
			if req.Operation == OpSpeakerEmbedding {
				iv := TimeInterval{StartMS: rec.StartMS, EndMS: rec.EndMS}
				if err := iv.Validate(durationMS); err != nil {
					return fmt.Errorf("speaker window %d: %w", i, err)
				}
			}
		}
	case OpSoundEvents:
		// Zero events is a valid answer for a silent recording.
		for i, ev := range res.Events {
			if strings.TrimSpace(ev.Label) == "" {
				return fmt.Errorf("event %d has no label", i)
			}
			iv := TimeInterval{StartMS: ev.StartMS, EndMS: ev.EndMS}
			if err := iv.Validate(durationMS); err != nil {
				return fmt.Errorf("event %d: %w", i, err)
			}
			if math.IsNaN(ev.RawScore) || math.IsInf(ev.RawScore, 0) {
				return fmt.Errorf("event %d has a non-finite score", i)
			}
			switch ev.EvidenceStatus {
			case "possible", "confirmed", "unknown":
			default:
				return fmt.Errorf("event %d has unknown evidence status %q", i, ev.EvidenceStatus)
			}
		}
	case OpDiarization:
		// Zero turns is a valid answer for a recording with no speech.
		for i, turn := range res.Turns {
			iv := TimeInterval{StartMS: turn.StartMS, EndMS: turn.EndMS}
			if err := iv.Validate(durationMS); err != nil {
				return fmt.Errorf("turn %d: %w", i, err)
			}
		}
	case OpASR:
		for i, seg := range res.Segments {
			iv := TimeInterval{StartMS: seg.StartMS, EndMS: seg.EndMS}
			if err := iv.Validate(durationMS); err != nil {
				return fmt.Errorf("segment %d: %w", i, err)
			}
		}
	case OpAlignment:
		for i, word := range res.Words {
			iv := TimeInterval{StartMS: word.StartMS, EndMS: word.EndMS}
			if err := iv.Validate(durationMS); err != nil {
				return fmt.Errorf("word %d: %w", i, err)
			}
		}
	}
	return nil
}

// validateVector rejects NaN/Inf, zero norms and wrong dimensions. A zero
// vector is not "no match": it is a broken output that would silently rank at
// cosine distance 1 against everything.
func validateVector(vec []float32, dimension, index int) error {
	if len(vec) == 0 {
		return fmt.Errorf("embedding %d is empty", index)
	}
	if dimension > 0 && len(vec) != dimension {
		return fmt.Errorf("embedding %d has dimension %d, want %d", index, len(vec), dimension)
	}
	var sumSquares float64
	for _, v := range vec {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("embedding %d contains a non-finite value", index)
		}
		sumSquares += f * f
	}
	if sumSquares <= 0 {
		return fmt.Errorf("embedding %d has zero norm", index)
	}
	return nil
}

// EmbeddingSpace is the identity of a vector space. Two vectors are only
// comparable when every field here matches, which is why the ID is the hash of
// this canonical JSON rather than a human label.
type EmbeddingSpace struct {
	ID                string   `json:"id"`
	Modality          Modality `json:"modality"`
	Provider          string   `json:"provider"`
	ModelID           string   `json:"model_id"`
	ModelRevision     string   `json:"model_revision"`
	Dimension         int      `json:"dimension"`
	Normalization     string   `json:"normalization"`
	PreprocessingJSON string   `json:"preprocessing_json"`
	RuntimeDigest     string   `json:"runtime_digest"`
}

// GenerationState is the lifecycle of one modality generation. Only a fully
// validated commit swaps the head; a failed replacement leaves the previous
// committed generation visible.
type GenerationState string

const (
	GenerationStaged    GenerationState = "staged"
	GenerationCommitted GenerationState = "committed"
	GenerationFailed    GenerationState = "failed"
)

// ModalityGeneration is one attempt to produce the data for one operation on
// one asset under one config hash.
type ModalityGeneration struct {
	ID                 string             `json:"id"`
	AssetID            string             `json:"asset_id"`
	Operation          InferenceOperation `json:"operation"`
	ModelRunID         string             `json:"model_run_id,omitempty"`
	InputHash          string             `json:"input_hash"`
	ConfigHash         string             `json:"config_hash"`
	SpaceID            string             `json:"space_id,omitempty"`
	State              GenerationState    `json:"state"`
	ShotSetHash        string             `json:"shot_set_hash,omitempty"`
	ProcessedIntervals IntervalSet        `json:"processed_intervals"`
	FailureIntervals   IntervalSet        `json:"failure_intervals"`
}

// InferenceTask binds one job to its typed request and staged result.
type InferenceTask struct {
	JobID            string             `json:"job_id"`
	AssetID          string             `json:"asset_id,omitempty"`
	TaskKey          string             `json:"task_key"`
	Operation        InferenceOperation `json:"operation"`
	RequestJSON      string             `json:"request_json"`
	ModelRunID       string             `json:"model_run_id,omitempty"`
	QueryReferenceID string             `json:"query_reference_id,omitempty"`
	StagedJSON       string             `json:"staged_json,omitempty"`
	CancelRequested  bool               `json:"cancel_requested"`
	ConfigHash       string             `json:"config_hash"`
	ModelRevision    string             `json:"model_revision"`
	CreatedAt        string             `json:"created_at,omitempty"`
	UpdatedAt        string             `json:"updated_at,omitempty"`
}

// MultimodalSettings is the singleton operator configuration. Every operation
// defaults to disabled: inference costs device time and, for biometrics, needs
// explicit consent, so an upgrade never starts either silently.
type MultimodalSettings struct {
	Enabled   map[InferenceOperation]bool `json:"enabled"`
	Profiles  map[string]string           `json:"profiles,omitempty"`
	UpdatedAt string                      `json:"updated_at,omitempty"`
}

// DefaultMultimodalSettings is the all-disabled configuration a fresh install
// starts from.
func DefaultMultimodalSettings() MultimodalSettings {
	enabled := make(map[InferenceOperation]bool, len(AllInferenceOperations()))
	for _, op := range AllInferenceOperations() {
		enabled[op] = false
	}
	return MultimodalSettings{Enabled: enabled, Profiles: map[string]string{}}
}

// EnabledOperations returns the operations currently switched on, sorted for
// deterministic enqueue order.
func (s MultimodalSettings) EnabledOperations() []InferenceOperation {
	out := make([]InferenceOperation, 0, len(s.Enabled))
	for op, on := range s.Enabled {
		if on {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SpeakerConsent is the singleton opt-in record for cross-recording speaker
// comparison. Enabling it is a legal act by the operator, not a preference.
type SpeakerConsent struct {
	Enabled    bool   `json:"enabled"`
	Generation int64  `json:"generation"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// SpeakerProfile is one asset-local speaker's eligible sample set, not a real
// identity. The distinction is load-bearing: the UI may show "similar to
// profile X", never a person's name.
type SpeakerProfile struct {
	ID             string `json:"id"`
	Generation     int64  `json:"generation"`
	State          string `json:"state"`
	CreatedAt      string `json:"created_at,omitempty"`
	DeletedAt      string `json:"deleted_at,omitempty"`
	AssetID        string `json:"asset_id,omitempty"`
	LocalSpeakerID string `json:"local_speaker_id,omitempty"`
}

// Speaker association states are a closed set. "unknown" is a real answer:
// without a calibrated threshold the Hub reports raw similarity and refuses to
// call a pair a match.
const (
	AssociationPossible  = "possible_match"
	AssociationConfirmed = "confirmed_by_user"
	AssociationRejected  = "rejected"
	AssociationUnknown   = "unknown"
)

// SpeakerAssociation is one candidate pairing between two profiles.
type SpeakerAssociation struct {
	ID                string  `json:"id"`
	LeftProfileID     string  `json:"left_profile_id"`
	RightProfileID    string  `json:"right_profile_id"`
	State             string  `json:"state"`
	Similarity        float64 `json:"similarity"`
	CalibrationID     string  `json:"calibration_id,omitempty"`
	ConsentGeneration int64   `json:"consent_generation"`
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// SearchReference is a Hub-managed query image. The caller never sees or
// supplies a path: the Hub decodes, re-encodes to strip metadata, stores the
// bytes under a generated name and hands back this opaque identity.
type SearchReference struct {
	ID                string    `json:"id"`
	ContentHash       string    `json:"content_hash"`
	MediaType         string    `json:"media_type"`
	RelativeCachePath string    `json:"-"`
	Width             int       `json:"width"`
	Height            int       `json:"height"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// ErrSearchReferenceExpired reports a reference whose lifetime ended but whose
// row has not been swept yet. It is distinct from "never existed" so the API
// can answer 410 instead of 404.
var ErrSearchReferenceExpired = errors.New("search reference expired")

// SearchReferenceTTL is the lifetime of a query image. A reference exists to
// serve one search round-trip; keeping the pixels longer would turn a transient
// query into an undeclared image library.
const SearchReferenceTTL = 24 * time.Hour

// ModelManagementTask is one queued install/probe unit. It is a maintenance
// job, not an inference operation: it moves and verifies bytes, it never runs
// model code. The row records the catalog identity, manifest digest and
// licence acceptance that authorized the download.
type ModelManagementTask struct {
	JobID             string `json:"job_id"`
	TargetWorkerID    string `json:"target_worker_id"`
	CatalogID         string `json:"catalog_id"`
	ManifestDigest    string `json:"manifest_digest"`
	Action            string `json:"action"`
	LicenseAccepted   bool   `json:"license_accepted"`
	LicenseAcceptedAt string `json:"license_accepted_at,omitempty"`
}

// AudioEventRow is the stored form of one environmental event, joined with the
// generation and model identity that produced it.
type AudioEventRow struct {
	ID                   string   `json:"id"`
	GenerationID         string   `json:"generation_id"`
	AssetID              string   `json:"asset_id"`
	StartMS              int64    `json:"start_ms"`
	EndMS                int64    `json:"end_ms"`
	Label                string   `json:"event_label"`
	Confidence           *float64 `json:"confidence"`
	RawScore             float64  `json:"raw_score"`
	EvidenceStatus       string   `json:"evidence_status"`
	BoundaryResolutionMS int      `json:"boundary_resolution_ms"`
	ModelID              string   `json:"model_id"`
	ModelVersion         string   `json:"model_version"`
	AnalysisRunID        string   `json:"analysis_run_id,omitempty"`
}

// AudioWindowRow is the timing of one stored audio embedding window. The vector
// itself is never part of a timeline response.
type AudioWindowRow struct {
	GenerationID string `json:"generation_id"`
	StartMS      int64  `json:"start_ms"`
	EndMS        int64  `json:"end_ms"`
}

// SpeakerTurnRow is one stored diarization turn.
type SpeakerTurnRow struct {
	ID             string   `json:"id"`
	GenerationID   string   `json:"generation_id"`
	AssetID        string   `json:"asset_id"`
	StartMS        int64    `json:"start_ms"`
	EndMS          int64    `json:"end_ms"`
	LocalSpeakerID string   `json:"local_speaker_id,omitempty"`
	Confidence     *float64 `json:"confidence"`
	Overlap        bool     `json:"overlap"`
}

// ModalityCoverageRow reports what a committed generation actually covered.
// "complete" is a statement about processed coverage, never about the world:
// an event missing from complete coverage is still not proof of absence.
type ModalityCoverageRow struct {
	Operation          InferenceOperation `json:"operation"`
	State              string             `json:"state"`
	ProcessedIntervals IntervalSet        `json:"processed_intervals"`
	FailureIntervals   IntervalSet        `json:"failure_intervals"`
	ModelID            string             `json:"model_id,omitempty"`
	ModelVersion       string             `json:"model_version,omitempty"`
	ConfigHash         string             `json:"config_hash"`
	CommittedAt        string             `json:"committed_at,omitempty"`
}

// AssetTimeline is the trusted-read view of one asset's tracks. Biometric
// profiles and associations are deliberately absent: they are admin-only and
// never appear in a library-facing response.
type AssetTimeline struct {
	AssetID      string                `json:"asset_id"`
	DurationMS   int64                 `json:"duration_ms"`
	Shots        []AssetShot           `json:"shots"`
	VisualRefs   []VisualRef           `json:"visual_refs"`
	Segments     []TranscriptSegment   `json:"transcript_segments"`
	Words        []AlignmentWord       `json:"transcript_words"`
	Turns        []SpeakerTurnRow      `json:"speaker_turns"`
	Events       []AudioEventRow       `json:"sound_events"`
	AudioWindows []AudioWindowRow      `json:"audio_windows"`
	Coverage     []ModalityCoverageRow `json:"coverage"`
}

// VisualRef names one stored visual vector without exposing its bytes.
type VisualRef struct {
	ShotID      string `json:"shot_id"`
	SampleIndex int    `json:"sample_index"`
	TimestampMS int64  `json:"timestamp_ms"`
	StartMS     int64  `json:"start_ms"`
	EndMS       int64  `json:"end_ms"`
	Strategy    string `json:"strategy"`
	SpaceID     string `json:"space_id"`
}

// SpeakerProfileSource names the asset-local speaker a profile was created
// from. It is a pointer to a diarization result, not an identity.
type SpeakerProfileSource struct {
	ProfileID               string `json:"profile_id"`
	AssetID                 string `json:"asset_id"`
	DiarizationGenerationID string `json:"diarization_generation_id"`
	LocalSpeakerID          string `json:"local_speaker_id"`
}

// SpeakerEmbedding is the metadata of one encrypted speaker vector. The vector
// itself lives in the Hub-only secret store under SecretRef and never enters
// SQLite, a model_runs payload or an HTTP response.
type SpeakerEmbedding struct {
	ID                string `json:"id"`
	ProfileID         string `json:"profile_id"`
	AssetID           string `json:"asset_id"`
	GenerationID      string `json:"generation_id"`
	SpaceID           string `json:"space_id"`
	StartMS           int64  `json:"start_ms"`
	EndMS             int64  `json:"end_ms"`
	SpeechDurationMS  int64  `json:"speech_duration_ms"`
	QualityJSON       string `json:"quality_json"`
	SecretRef         string `json:"-"`
	ConsentGeneration int64  `json:"consent_generation"`
	ProfileGeneration int64  `json:"profile_generation"`
}

// SpeakerEmbeddingStaging binds an in-flight extraction to the generations it
// must still match at commit time.
type SpeakerEmbeddingStaging struct {
	OperationID       string `json:"operation_id"`
	JobID             string `json:"job_id"`
	ProfileID         string `json:"profile_id"`
	ProfileGeneration int64  `json:"profile_generation"`
	ConsentGeneration int64  `json:"consent_generation"`
	SecretRef         string `json:"-"`
}

// ModalityMatch is one row a modality channel matched, with the provenance the
// evidence layer needs. Score is a similarity (1 - cosine distance for vectors,
// the detector score for events); it is not a calibrated probability.
type ModalityMatch struct {
	RecordID       string   `json:"record_id"`
	AssetID        string   `json:"asset_id"`
	ShotID         string   `json:"shot_id,omitempty"`
	Modality       Modality `json:"modality"`
	Label          string   `json:"label,omitempty"`
	StartMS        int64    `json:"start_ms"`
	EndMS          int64    `json:"end_ms"`
	Score          float64  `json:"score"`
	EvidenceStatus string   `json:"evidence_status"`
	Confidence     *float64 `json:"confidence"`
	ModelID        string   `json:"model_id"`
	ModelRevision  string   `json:"model_revision"`
	Backend        string   `json:"backend"`
	GenerationID   string   `json:"generation_id"`
}

// CanonicalSpaceID is the SHA-256 of the canonical JSON identity of a space.
// The ID is derived, never assigned, so two installations that agree on every
// field agree on the space and can exchange vectors.
func CanonicalSpaceID(space EmbeddingSpace) (string, error) {
	if !space.Modality.Valid() {
		return "", fmt.Errorf("space: unknown modality %q", space.Modality)
	}
	if space.Dimension <= 0 {
		return "", fmt.Errorf("space: dimension must be positive")
	}
	if strings.TrimSpace(space.ModelRevision) == "" {
		return "", fmt.Errorf("space: immutable model revision is required")
	}
	payload := struct {
		Modality          Modality `json:"modality"`
		Provider          string   `json:"provider"`
		ModelID           string   `json:"model_id"`
		ModelRevision     string   `json:"model_revision"`
		Dimension         int      `json:"dimension"`
		Normalization     string   `json:"normalization"`
		PreprocessingJSON string   `json:"preprocessing_json"`
		RuntimeDigest     string   `json:"runtime_digest"`
	}{
		Modality:          space.Modality,
		Provider:          space.Provider,
		ModelID:           space.ModelID,
		ModelRevision:     space.ModelRevision,
		Dimension:         space.Dimension,
		Normalization:     space.Normalization,
		PreprocessingJSON: space.PreprocessingJSON,
		RuntimeDigest:     space.RuntimeDigest,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("space: encode identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "space-" + hex.EncodeToString(sum[:]), nil
}

// ShotSetHash is the identity of a canonical shot set. Visual vectors are
// rejected when the asset's shots changed since the request was built.
func ShotSetHash(shots []AssetShot) string {
	ordered := append([]AssetShot(nil), shots...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].StartMS != ordered[j].StartMS {
			return ordered[i].StartMS < ordered[j].StartMS
		}
		if ordered[i].EndMS != ordered[j].EndMS {
			return ordered[i].EndMS < ordered[j].EndMS
		}
		return ordered[i].ID < ordered[j].ID
	})
	var b strings.Builder
	for _, shot := range ordered {
		fmt.Fprintf(&b, "%s:%d:%d\n", shot.ID, shot.StartMS, shot.EndMS)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "shots-" + hex.EncodeToString(sum[:])
}
