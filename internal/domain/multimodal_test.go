package domain

import (
	"math"
	"testing"
)

func validRequest() InferenceRequest {
	return InferenceRequest{
		SchemaVersion: InferenceSchemaVersion,
		OperationID:   "op-1",
		Operation:     OpSoundEvents,
		ModelID:       "google/yamnet",
		ModelRevision: "rev-1",
		InputHash:     "sha256:abc",
		ConfigHash:    "cfg-1",
		TimeoutMS:     1000,
		Input: InferenceInput{
			Kind: InputKindAsset, AssetID: "asset-1", DurationMS: 40000,
		},
		SoundEvent: &SoundEventSettings{},
	}
}

func TestInferenceRequestValidateRejectsStructuralErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*InferenceRequest)
	}{
		{"wrong schema", func(r *InferenceRequest) { r.SchemaVersion = "inference.v0" }},
		{"unknown operation", func(r *InferenceRequest) { r.Operation = "describe" }},
		{"missing revision", func(r *InferenceRequest) { r.ModelRevision = "" }},
		{"zero timeout", func(r *InferenceRequest) { r.TimeoutMS = 0 }},
		{"two settings", func(r *InferenceRequest) { r.Visual = &VisualSettings{} }},
		{"no settings", func(r *InferenceRequest) { r.SoundEvent = nil }},
		{"space required", func(r *InferenceRequest) {
			r.Operation = OpAudioEmbedding
			r.SoundEvent = nil
			r.Audio = &AudioSettings{}
		}},
		{"text too long", func(r *InferenceRequest) {
			r.Operation = OpVisualEmbedding
			r.SoundEvent = nil
			r.Visual = &VisualSettings{}
			r.Input = InferenceInput{Kind: InputKindText, Text: string(make([]byte, MaxQueryTextBytes+1))}
		}},
		{"image without digest", func(r *InferenceRequest) {
			r.Operation = OpVisualEmbedding
			r.SoundEvent = nil
			r.Visual = &VisualSettings{}
			r.Input = InferenceInput{Kind: InputKindImage, ReferenceID: "ref-1"}
		}},
		{"reversed interval", func(r *InferenceRequest) {
			r.Input.Interval = &TimeInterval{StartMS: 1000, EndMS: 1000}
		}},
		{"interval past duration", func(r *InferenceRequest) {
			r.Input.Interval = &TimeInterval{StartMS: 1, EndMS: 50000}
		}},
		{"shot sample outside shot", func(r *InferenceRequest) {
			r.Operation = OpVisualEmbedding
			r.SpaceID = "space-1"
			r.SoundEvent = nil
			r.Visual = &VisualSettings{Strategy: "shot_frame_pooled", Shots: []VisualShot{
				{ShotID: "s1", StartMS: 0, EndMS: 1000, TimestampsMS: []int64{1200}},
			}}
		}},
	}
	for _, tc := range cases {
		req := validRequest()
		tc.mutate(&req)
		if err := req.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}

func TestInferenceRequestValidateAcceptsTheHappyPaths(t *testing.T) {
	asset := validRequest()
	if err := asset.Validate(); err != nil {
		t.Fatalf("sound asset request: %v", err)
	}
	text := validRequest()
	text.Operation = OpVisualEmbedding
	text.SpaceID = "space-1"
	text.SoundEvent = nil
	text.Visual = &VisualSettings{}
	text.Input = InferenceInput{Kind: InputKindText, Text: "雨夜 撑伞"}
	if err := text.Validate(); err != nil {
		t.Fatalf("visual text request: %v", err)
	}
	shots := validRequest()
	shots.Operation = OpVisualEmbedding
	shots.SpaceID = "space-1"
	shots.SoundEvent = nil
	shots.Visual = &VisualSettings{Strategy: "shot_frame_pooled", Shots: []VisualShot{
		{ShotID: "s1", StartMS: 0, EndMS: 10000, TimestampsMS: []int64{2000, 7000}},
	}}
	if err := shots.Validate(); err != nil {
		t.Fatalf("visual shot request: %v", err)
	}
}

func TestTimeIntervalOverlapIsHalfOpen(t *testing.T) {
	a := TimeInterval{StartMS: 0, EndMS: 1000}
	b := TimeInterval{StartMS: 1000, EndMS: 2000}
	if a.Overlaps(b) || b.Overlaps(a) {
		t.Fatal("touching intervals must not overlap")
	}
	if _, ok := a.Intersect(b); ok {
		t.Fatal("touching intervals must not intersect")
	}
	c := TimeInterval{StartMS: 999, EndMS: 2000}
	if !a.Overlaps(c) {
		t.Fatal("a one-millisecond overlap is an overlap")
	}
}

func TestIntervalSetNormalizeMergesTouching(t *testing.T) {
	set := IntervalSet{
		{StartMS: 3000, EndMS: 4000},
		{StartMS: 1000, EndMS: 2000},
		{StartMS: 2000, EndMS: 2500},
	}
	got := set.Normalize()
	if len(got) != 2 {
		t.Fatalf("want 2 merged intervals, got %+v", got)
	}
	if got[0] != (TimeInterval{StartMS: 1000, EndMS: 2500}) || got[1] != (TimeInterval{StartMS: 3000, EndMS: 4000}) {
		t.Fatalf("unexpected normalize result: %+v", got)
	}
	if !got.Covers(TimeInterval{StartMS: 1200, EndMS: 2400}) {
		t.Fatal("merged interval must cover its interior")
	}
	if got.Covers(TimeInterval{StartMS: 2400, EndMS: 3100}) {
		t.Fatal("a gap must not be covered")
	}
}

func TestInferenceResultValidateForRejectsBrokenOutputs(t *testing.T) {
	req := InferenceRequest{
		SchemaVersion: InferenceSchemaVersion,
		OperationID:   "op-1",
		Operation:     OpVisualEmbedding,
		ModelID:       "m",
		ModelRevision: "r",
		SpaceID:       "s",
		InputHash:     "i",
		ConfigHash:    "c",
		TimeoutMS:     1000,
		Input:         InferenceInput{Kind: InputKindAsset, AssetID: "a", DurationMS: 40000},
		Visual:        &VisualSettings{Strategy: "native_video"},
	}
	good := InferenceResult{
		SchemaVersion: InferenceSchemaVersion,
		OperationID:   "op-1",
		Operation:     OpVisualEmbedding,
		ModelID:       "m",
		ModelRevision: "r",
		SpaceID:       "s",
		InputHash:     "i",
		ConfigHash:    "c",
		Backend:       "cuda",
		Embeddings: []EmbeddingRecord{
			{ShotID: "s1", SampleIndex: -1, StartMS: 0, EndMS: 1000, Vector: []float32{0.6, 0.8}},
		},
	}
	if err := good.ValidateFor(req, req.Input.DurationMS, 2); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}

	badDimension := good
	badDimension.Embeddings = []EmbeddingRecord{{Vector: []float32{0.6, 0.8}, SampleIndex: -1}}
	if err := badDimension.ValidateFor(req, 40000, 3); err == nil {
		t.Error("dimension mismatch must be rejected")
	}

	nan := good
	nan.Embeddings = []EmbeddingRecord{{Vector: []float32{float32(math.NaN()), 1}, SampleIndex: -1}}
	if err := nan.ValidateFor(req, 40000, 2); err == nil {
		t.Error("NaN vector must be rejected")
	}

	zero := good
	zero.Embeddings = []EmbeddingRecord{{Vector: []float32{0, 0}, SampleIndex: -1}}
	if err := zero.ValidateFor(req, 40000, 2); err == nil {
		t.Error("zero-norm vector must be rejected")
	}

	mismatchedID := good
	mismatchedID.OperationID = "other"
	if err := mismatchedID.ValidateFor(req, 40000, 2); err == nil {
		t.Error("operation id mismatch must be rejected")
	}

	twoPayloads := good
	twoPayloads.Events = []SoundEvent{{Label: "Rain", StartMS: 0, EndMS: 1, EvidenceStatus: "possible"}}
	if err := twoPayloads.ValidateFor(req, 40000, 2); err == nil {
		t.Error("two typed payloads must be rejected")
	}
}

func TestSoundEventValidationRejectsReversedIntervals(t *testing.T) {
	req := validRequest()
	res := InferenceResult{
		SchemaVersion: InferenceSchemaVersion,
		OperationID:   req.OperationID,
		Operation:     req.Operation,
		ModelID:       req.ModelID,
		ModelRevision: req.ModelRevision,
		InputHash:     req.InputHash,
		ConfigHash:    req.ConfigHash,
		Backend:       "cpu",
		Events: []SoundEvent{
			{Label: "Rain", StartMS: 2000, EndMS: 1000, RawScore: 0.5, EvidenceStatus: "possible"},
		},
	}
	if err := res.ValidateFor(req, 40000, 0); err == nil {
		t.Fatal("reversed event must be rejected")
	}
	res.Events[0] = SoundEvent{Label: "Rain", StartMS: 1000, EndMS: 2000, RawScore: 0.5, EvidenceStatus: "possible"}
	if err := res.ValidateFor(req, 40000, 0); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
}

func TestInferenceResultAcceptsEmptyEventAndTurnPayloads(t *testing.T) {
	soundReq := validRequest()
	sound := InferenceResult{
		SchemaVersion: InferenceSchemaVersion, OperationID: soundReq.OperationID,
		Operation: soundReq.Operation, ModelID: soundReq.ModelID, ModelRevision: soundReq.ModelRevision,
		InputHash: soundReq.InputHash, ConfigHash: soundReq.ConfigHash, Backend: "cpu",
		// A silent recording legitimately yields no events.
	}
	if err := sound.ValidateFor(soundReq, 40000, 0); err != nil {
		t.Fatalf("empty sound events must be valid: %v", err)
	}

	diarReq := validRequest()
	diarReq.Operation = OpDiarization
	diarReq.SoundEvent = nil
	diarReq.Diarization = &DiarizationSettings{}
	diar := InferenceResult{
		SchemaVersion: InferenceSchemaVersion, OperationID: diarReq.OperationID,
		Operation: diarReq.Operation, ModelID: diarReq.ModelID, ModelRevision: diarReq.ModelRevision,
		InputHash: diarReq.InputHash, ConfigHash: diarReq.ConfigHash, Backend: "cpu",
	}
	if err := diar.ValidateFor(diarReq, 40000, 0); err != nil {
		t.Fatalf("empty diarization turns must be valid: %v", err)
	}

	wrongKind := InferenceResult{
		SchemaVersion: InferenceSchemaVersion, OperationID: soundReq.OperationID,
		Operation: soundReq.Operation, ModelID: soundReq.ModelID, ModelRevision: soundReq.ModelRevision,
		InputHash: soundReq.InputHash, ConfigHash: soundReq.ConfigHash, Backend: "cpu",
		Turns: []SpeakerTurn{{StartMS: 0, EndMS: 1000}},
	}
	if err := wrongKind.ValidateFor(soundReq, 40000, 0); err == nil {
		t.Fatal("a sound-events result carrying turns must be rejected")
	}
}
