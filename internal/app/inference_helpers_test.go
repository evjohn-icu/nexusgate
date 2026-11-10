package app

import (
	"context"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

type helperExecutor struct{}

func (helperExecutor) Capabilities(context.Context) ([]remote.InferenceCapability, error) {
	return []remote.InferenceCapability{{Operation: domain.OpVisualEmbedding, ModelID: "m", ModelRevision: "r", Available: true}}, nil
}
func (helperExecutor) Execute(context.Context, domain.InferenceRequest) (domain.InferenceResult, error) {
	return domain.InferenceResult{}, nil
}

type helperManager struct{}

func (helperManager) Install(context.Context, domain.ModelManagementTask) error { return nil }
func (helperManager) Probe(context.Context, domain.ModelManagementTask) error   { return nil }

func TestInferenceCapabilityHelpers(t *testing.T) {
	caps := []remote.InferenceCapability{
		{Operation: domain.OpVisualEmbedding, SchemaVersion: domain.InferenceSchemaVersion, ModelID: "m", ModelRevision: "r", ConfigHash: "z", Backend: "cpu", Available: true},
		{Operation: domain.OpAudioEmbedding, SchemaVersion: domain.InferenceSchemaVersion, ModelID: "m", ModelRevision: "r", ConfigHash: "a", Backend: "cpu", Available: true},
	}
	sorted := SortedCapabilities(caps)
	if sorted[0].Operation != domain.OpAudioEmbedding || sorted[1].Operation != domain.OpVisualEmbedding {
		t.Fatalf("sorted capabilities = %+v", sorted)
	}
	if caps[0].Operation != domain.OpVisualEmbedding {
		t.Fatal("SortedCapabilities mutated its input")
	}
	req := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion, Operation: domain.OpVisualEmbedding,
		ModelID: "m", ModelRevision: "r", ConfigHash: "z", Visual: &domain.VisualSettings{},
	}
	if cap, ok := CapabilityFor(caps, req); !ok || cap.ConfigHash != "z" {
		t.Fatalf("CapabilityFor = %+v %v", cap, ok)
	}
	req.ConfigHash = "missing"
	if _, ok := CapabilityFor(caps, req); ok {
		t.Fatal("CapabilityFor matched the wrong config")
	}

	service := &Service{pipeline: &Pipeline{}}
	if service.InferenceExecutorStatus() {
		t.Fatal("fresh service reports a runtime")
	}
	service.AttachInferenceExecutor(helperExecutor{})
	if !service.InferenceExecutorStatus() || service.pipeline.inferenceExec == nil || service.pipeline.speakerCommit == nil {
		t.Fatal("AttachInferenceExecutor did not wire the pipeline")
	}
	service.AttachModelManager(helperManager{})
	if service.pipeline.modelManager == nil {
		t.Fatal("AttachModelManager did not wire the pipeline")
	}
	var nilService *Service
	nilService.AttachInferenceExecutor(helperExecutor{})
	nilService.AttachModelManager(helperManager{})
	if nilService.InferenceExecutorStatus() {
		t.Fatal("nil service reports a runtime")
	}
	if got := inferenceConfigHash("a", "b"); got != inferenceConfigHash("a", "b") || got == inferenceConfigHash("a", "c") {
		t.Fatalf("inferenceConfigHash = %q", got)
	}
}
