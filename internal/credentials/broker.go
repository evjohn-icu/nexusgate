// Package credentials selects a short-lived, in-memory provider credential for
// a Worker-owned task. It deliberately has no persistence dependency: callers
// may persist only LeaseAudit, never Credential.
package credentials

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
)

type Operation string

const (
	OperationASR           Operation = "asr"
	OperationVideoAnalysis Operation = "video_analysis"
	OperationEmbedding     Operation = "embedding"
	OperationTagCurator    Operation = "tag_curator"
)

type Credential struct {
	BaseURL        string            `json:"base_url"`
	Path           string            `json:"path,omitempty"`
	APIKey         string            `json:"api_key"`
	Model          string            `json:"model"`
	Protocol       string            `json:"protocol,omitempty"`
	AuthHeader     string            `json:"auth_header,omitempty"`
	AuthScheme     string            `json:"auth_scheme,omitempty"`
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// MarshalJSON replaces the APIKey with [redacted] so that accidental
// serialisation — through a log line, an error message, or an API response —
// never exposes a provider credential. Callers that need the real key must
// read the field directly; MarshalJSON is a safety net, not an access path.
func (c Credential) MarshalJSON() ([]byte, error) {
	type Alias Credential
	safe := Alias(c)
	if safe.APIKey != "" {
		safe.APIKey = "[redacted]"
	}
	return json.Marshal(safe)
}

type Lease struct {
	JobID      string     `json:"job_id"`
	WorkerID   string     `json:"worker_id"`
	Provider   string     `json:"provider"`
	Operation  Operation  `json:"operation"`
	ExpiresAt  string     `json:"expires_at"`
	Credential Credential `json:"credential"`
}

// LeaseAudit is safe to persist in Hub audit tables and event logs.
type LeaseAudit struct {
	JobID     string    `json:"job_id"`
	WorkerID  string    `json:"worker_id"`
	Provider  string    `json:"provider"`
	Operation Operation `json:"operation"`
	ExpiresAt time.Time `json:"expires_at"`
	// These explicit blanks make it impossible to accidentally treat an audit
	// record as a usable provider configuration.
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

func (l Lease) Audit() LeaseAudit {
	expires, _ := time.Parse(time.RFC3339, l.ExpiresAt)
	return LeaseAudit{JobID: l.JobID, WorkerID: l.WorkerID, Provider: l.Provider, Operation: l.Operation, ExpiresAt: expires}
}

type Broker struct {
	providers config.ProvidersConfig
	now       func() time.Time
}

func NewBroker(providers config.ProvidersConfig, now func() time.Time) Broker {
	if now == nil {
		now = time.Now
	}
	return Broker{providers: providers, now: now}
}

func (b Broker) Issue(jobID, workerID string, operation Operation, ttl time.Duration) (Lease, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" {
		return Lease{}, fmt.Errorf("job ID and worker ID are required")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	provider, configured, err := b.resolve(operation)
	if err != nil {
		return Lease{}, err
	}
	if !configured.Enabled {
		return Lease{}, fmt.Errorf("provider %q for %s is not enabled", provider, operation)
	}
	if strings.TrimSpace(configured.BaseURL) == "" {
		return Lease{}, fmt.Errorf("provider %q for %s has no endpoint", provider, operation)
	}
	expires := b.now().UTC().Add(ttl)
	return Lease{
		JobID: jobID, WorkerID: workerID, Provider: provider, Operation: operation, ExpiresAt: expires.Format(time.RFC3339),
		Credential: Credential{BaseURL: configured.BaseURL, Path: configured.Path, APIKey: configured.APIKey, Model: configured.Model, Protocol: configured.Protocol, AuthHeader: configured.AuthHeader, AuthScheme: configured.AuthScheme, ExtraHeaders: configured.ExtraHeaders, TimeoutSeconds: configured.TimeoutSeconds},
	}, nil
}

func (b Broker) resolve(operation Operation) (string, config.ProviderConfig, error) {
	p := b.providers
	switch operation {
	case OperationVideoAnalysis:
		switch p.VisionPrimary {
		case "gemini":
			return "gemini", p.Gemini, nil
		case "qwen_video":
			return "qwen_video", p.QwenVideo, nil
		case "volcengine_video":
			return "volcengine_video", p.VolcVideo, nil
		case "local_vlm":
			return "local_vlm", p.LocalVLM, nil
		}
	case OperationEmbedding:
		switch p.EmbeddingPrimary {
		case "openai_embeddings", "gemini_embed_content":
			return p.EmbeddingPrimary, p.Embedding, nil
		case "volc_agent_plan_embedding":
			return p.EmbeddingPrimary, p.VolcAgentPlanEmbedding, nil
		case "volc_coding_plan_embedding":
			return p.EmbeddingPrimary, p.VolcCodingPlanEmbedding, nil
		}
	case OperationTagCurator:
		switch p.TagCuratorPrimary {
		case "openai_chat":
			return p.TagCuratorPrimary, p.TagCurator, nil
		case "volc_agent_plan":
			return p.TagCuratorPrimary, p.VolcAgentPlan, nil
		case "volc_coding_plan":
			return p.TagCuratorPrimary, p.VolcCodingPlan, nil
		}
	case OperationASR:
		switch p.ASRPrimary {
		case "stepfun":
			return p.ASRPrimary, p.StepFun, nil
		case "qwen":
			return p.ASRPrimary, p.Qwen, nil
		}
	}
	return "", config.ProviderConfig{}, fmt.Errorf("no configured provider for %s", operation)
}
