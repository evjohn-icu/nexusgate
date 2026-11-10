package remote

import (
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestCapabilityValidateAndMatches(t *testing.T) {
	base := InferenceCapability{
		Operation: domain.OpVisualEmbedding, SchemaVersion: domain.InferenceSchemaVersion,
		ModelID: "model", ModelRevision: "rev", ConfigHash: "cfg", SpaceID: "space", Backend: "cpu", Available: true,
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion, Operation: domain.OpVisualEmbedding,
		ModelID: "model", ModelRevision: "rev", ConfigHash: "cfg", SpaceID: "space",
		Visual: &domain.VisualSettings{},
	}
	available := base
	available.Available = true
	if !available.Matches(request) {
		t.Fatal("matching capability did not match")
	}
	for name, mutate := range map[string]func(*InferenceCapability){
		"unavailable": func(c *InferenceCapability) { c.Available = false },
		"operation":   func(c *InferenceCapability) { c.Operation = domain.OpAudioEmbedding },
		"model":       func(c *InferenceCapability) { c.ModelID = "other" },
		"revision":    func(c *InferenceCapability) { c.ModelRevision = "other" },
		"config":      func(c *InferenceCapability) { c.ConfigHash = "other" },
		"space":       func(c *InferenceCapability) { c.SpaceID = "other" },
	} {
		candidate := available
		mutate(&candidate)
		if candidate.Matches(request) {
			t.Fatalf("%s capability matched", name)
		}
	}
	broken := base
	broken.Operation = "unknown"
	if err := broken.Validate(); err == nil {
		t.Fatal("unknown operation validated")
	}
	broken = base
	broken.SchemaVersion = "v0"
	if err := broken.Validate(); err == nil {
		t.Fatal("wrong schema validated")
	}
	broken = base
	broken.ModelID = ""
	if err := broken.Validate(); err == nil {
		t.Fatal("missing model validated")
	}
	broken = base
	broken.ModelRevision = ""
	if err := broken.Validate(); err == nil {
		t.Fatal("missing revision validated")
	}
	broken = base
	broken.Backend = ""
	if err := broken.Validate(); err == nil {
		t.Fatal("missing backend validated")
	}
	broken = base
	broken.Dimension = -1
	if err := broken.Validate(); err == nil {
		t.Fatal("negative dimension validated")
	}
	intersected := IntersectInferenceCapabilities([]InferenceCapability{available, base}, []string{string(domain.OpVisualEmbedding)})
	if len(intersected) != 2 || !intersected[0].Available {
		t.Fatalf("intersection = %+v", intersected)
	}
}
