package credentials

import (
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
)

func TestBrokerIssuesOnlyConfiguredCredentialForVideoAnalysis(t *testing.T) {
	cfg := config.ProvidersConfig{
		VisionPrimary: "volcengine_video",
		VolcVideo: config.ProviderConfig{
			Enabled: true, BaseURL: "https://vision.example/v3", Path: "chat/completions",
			APIKey: "secret-never-persisted", Model: "vision-1", AuthHeader: "X-Api-Key", AuthScheme: "raw",
		},
	}

	broker := NewBroker(cfg, func() time.Time { return time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC) })
	lease, err := broker.Issue("job-1", "worker-1", OperationVideoAnalysis, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.JobID != "job-1" || lease.WorkerID != "worker-1" || lease.Provider != "volcengine_video" {
		t.Fatalf("unexpected lease identity: %#v", lease)
	}
	if lease.Credential.APIKey != "secret-never-persisted" || lease.Credential.BaseURL != "https://vision.example/v3" {
		t.Fatalf("credential was not resolved from the configured provider: %#v", lease.Credential)
	}
	if lease.ExpiresAt != "2026-07-26T00:05:00Z" {
		t.Fatalf("unexpected expiry: %s", lease.ExpiresAt)
	}
}

func TestBrokerRefusesDisabledOrUnsupportedProvider(t *testing.T) {
	broker := NewBroker(config.ProvidersConfig{VisionPrimary: "volcengine_video"}, time.Now)
	if _, err := broker.Issue("job-1", "worker-1", OperationVideoAnalysis, time.Minute); err == nil {
		t.Fatal("expected disabled provider to be rejected")
	}
	if _, err := broker.Issue("job-1", "worker-1", "unrecognized", time.Minute); err == nil {
		t.Fatal("expected unsupported operation to be rejected")
	}
}

func TestAuditRecordNeverContainsAPIKey(t *testing.T) {
	broker := NewBroker(config.ProvidersConfig{EmbeddingPrimary: "openai_embeddings", Embedding: config.ProviderConfig{Enabled: true, APIKey: "do-not-log", BaseURL: "https://embed.example", Model: "embed-1"}}, time.Now)
	lease, err := broker.Issue("job-2", "worker-2", OperationEmbedding, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	audit := lease.Audit()
	if audit.Provider == "" || audit.Operation != OperationEmbedding || audit.JobID != "job-2" {
		t.Fatalf("missing audit identity: %#v", audit)
	}
	if audit.APIKey != "" || audit.BaseURL != "" {
		t.Fatalf("audit must not contain credential material: %#v", audit)
	}
}
