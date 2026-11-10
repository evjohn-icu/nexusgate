// Package inferencecatalog is the Go view of `inference/models.lock.json`.
//
// The JSON file stays the authority — the Python dispatcher, the installer and
// the operator docs all read it. This package mirrors only the fields the Hub
// needs to build a request: the immutable identity, the output dimension and
// the preprocessing/runtime digests that make up a space id. A test parses the
// lock file and fails when the two drift, so the mirror cannot silently diverge
// from what the inference process actually loads.
package inferencecatalog

import (
	"fmt"
	"sort"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// Model is one locked model identity.
type Model struct {
	CatalogID         string
	ModelID           string
	Revision          string
	Operations        []domain.InferenceOperation
	Dimension         int
	PreprocessingJSON string
	RuntimeDigest     string
	Normalization     string
	Provider          string
	// Matryoshka lists the dimensions the model documents. Empty means the
	// model has no MRL support, so a truncation request is unsupported_input.
	Matryoshka []int
	// NativeVideo reports whether the model has a native-video encoder, which
	// decides whether a native-video profile is ever offered.
	NativeVideo bool
}

// LockSchemaVersion is the schema of the file this package mirrors.
const LockSchemaVersion = "inference.models.lock.v1"

// models is the Go mirror. Keep it in lock order.
var models = []Model{
	{
		CatalogID:     "visual-siglip2-base",
		ModelID:       "google/siglip2-base-patch16-224",
		Revision:      "75de2d55ec2d0b4efc50b3e9ad70dba96a7b2fa2",
		Operations:    []domain.InferenceOperation{domain.OpVisualEmbedding},
		Dimension:     768,
		Provider:      "huggingface",
		Normalization: "l2",
		PreprocessingJSON: `{"image":{"resize":[224,224],"resample":"bicubic","rescale_factor":0.00392156862745098,` +
			`"mean":[0.5,0.5,0.5],"std":[0.5,0.5,0.5],"color":"RGB"},` +
			`"text":{"max_tokens":64,"padding":"max_length","truncation":true},` +
			`"sampling":"boundary_aware_2_4_6"}`,
		RuntimeDigest: "torch-cu126-fp32-fp16",
		NativeVideo:   true,
	},
	{
		CatalogID:     "visual-wemm-embedding-2b",
		ModelID:       "tencent/WeMM-Embedding-2B",
		Revision:      "bbd6cd4bf52cfc6716f752a2df80b2706720bd95",
		Operations:    []domain.InferenceOperation{domain.OpVisualEmbedding},
		Dimension:     2048,
		Provider:      "huggingface",
		Normalization: "l2",
		PreprocessingJSON: `{"image":{"patch_size":16,"spatial_merge_size":2,"color":"RGB"},` +
			`"video":{"temporal_patch_size":2},"text":{"max_tokens":512},` +
			`"sampling":"boundary_aware_2_4_6"}`,
		RuntimeDigest: "torch-cu126-fp16",
		Matryoshka:    []int{64, 128, 256, 512, 1024, 2048},
		NativeVideo:   true,
	},
	{
		CatalogID:         "audio-clap-htsat-unfused",
		ModelID:           "laion/clap-htsat-unfused",
		Revision:          "8fa0f1c6d0433df6e97c127f64b2a1d6c0dcda8a",
		Operations:        []domain.InferenceOperation{domain.OpAudioEmbedding},
		Dimension:         512,
		Provider:          "huggingface",
		Normalization:     "l2",
		PreprocessingJSON: `{"audio":{"sample_rate":48000,"channels":1,"window_ms":10000,"hop_ms":5000,"tail":"zero_pad"},"text":{"max_tokens":77}}`,
		RuntimeDigest:     "torch-cu126-fp32-fp16",
	},
	{
		CatalogID:         "sound-yamnet",
		ModelID:           "google/yamnet",
		Revision:          "kaggle:google/yamnet/TensorFlow2/yamnet/1",
		Operations:        []domain.InferenceOperation{domain.OpSoundEvents},
		Dimension:         0,
		Provider:          "kaggle_models",
		PreprocessingJSON: `{"audio":{"sample_rate":16000,"channels":1,"patch_ms":960,"hop_ms":480},"postprocessing":{"cell_ms":100,"smoothing":"centered_3_cell_median","start_score":0.35,"close_score":0.2,"close_consecutive_cells":2,"merge_gap_ms":300,"min_span_ms":200}}`,
		RuntimeDigest:     "tensorflow-cpu-fp32",
	},
	{
		CatalogID:         "diarization-pyannote-community-1",
		ModelID:           "pyannote/speaker-diarization-community-1",
		Revision:          "3533c8cf8e369892e6b79ff1bf80f7b0286a54ee",
		Operations:        []domain.InferenceOperation{domain.OpDiarization},
		Provider:          "huggingface_gated",
		PreprocessingJSON: `{"audio":{"sample_rate":16000,"channels":1}}`,
		RuntimeDigest:     "torch-cu126-fp32",
	},
	{
		CatalogID:         "speaker-campplus",
		ModelID:           "iic/speech_campplus_sv_zh-cn_16k-common",
		Revision:          "013c18884dffb82846cf472dd06dd4a08d97e2cd",
		Operations:        []domain.InferenceOperation{domain.OpSpeakerEmbedding},
		Dimension:         192,
		Provider:          "modelscope",
		Normalization:     "l2",
		PreprocessingJSON: `{"audio":{"sample_rate":16000,"channels":1,"features":"80-dim fbank","min_speech_ms":3000}}`,
		RuntimeDigest:     "torch-cu126-fp32",
	},
	{
		CatalogID:         "speaker-eres2netv2",
		ModelID:           "iic/speech_eres2netv2_sv_zh-cn_16k-common",
		Revision:          "1cf80d41fb3435bd3d8df185b5c423333b2db42a",
		Operations:        []domain.InferenceOperation{domain.OpSpeakerEmbedding},
		Dimension:         192,
		Provider:          "modelscope",
		Normalization:     "l2",
		PreprocessingJSON: `{"audio":{"sample_rate":16000,"channels":1,"features":"80-dim fbank","min_speech_ms":3000}}`,
		RuntimeDigest:     "torch-cu126-fp32",
	},
}

// All returns every locked model in lock order.
func All() []Model { return append([]Model(nil), models...) }

// Lookup returns the model with the given catalog id.
func Lookup(catalogID string) (Model, bool) {
	for _, m := range models {
		if m.CatalogID == catalogID {
			return m, true
		}
	}
	return Model{}, false
}

// ForOperation returns the models that can execute op, sorted by catalog id.
func ForOperation(op domain.InferenceOperation) []Model {
	var out []Model
	for _, m := range models {
		for _, candidate := range m.Operations {
			if candidate == op {
				out = append(out, m)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CatalogID < out[j].CatalogID })
	return out
}

// DefaultFor returns the catalog default for op. Vector operations have one
// default space; sound events and diarization have exactly one provider each.
func DefaultFor(op domain.InferenceOperation) (Model, bool) {
	switch op {
	case domain.OpVisualEmbedding:
		return Lookup("visual-siglip2-base")
	case domain.OpAudioEmbedding:
		return Lookup("audio-clap-htsat-unfused")
	case domain.OpSoundEvents:
		return Lookup("sound-yamnet")
	case domain.OpDiarization:
		return Lookup("diarization-pyannote-community-1")
	case domain.OpSpeakerEmbedding:
		return Lookup("speaker-campplus")
	}
	return Model{}, false
}

// Validate checks that a mirror entry is internally consistent.
func (m Model) Validate() error {
	if m.CatalogID == "" || m.ModelID == "" || m.Revision == "" {
		return fmt.Errorf("catalog model lacks identity")
	}
	if len(m.Operations) == 0 {
		return fmt.Errorf("catalog model %q has no operations", m.CatalogID)
	}
	for _, op := range m.Operations {
		if !op.Valid() {
			return fmt.Errorf("catalog model %q names unknown operation %q", m.CatalogID, op)
		}
		if op.NeedsSpace() && m.Dimension <= 0 {
			return fmt.Errorf("catalog model %q executes %q without a dimension", m.CatalogID, op)
		}
	}
	if m.PreprocessingJSON == "" || m.RuntimeDigest == "" {
		return fmt.Errorf("catalog model %q lacks preprocessing/runtime identity", m.CatalogID)
	}
	return nil
}
