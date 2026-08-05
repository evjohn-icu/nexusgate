// Package credentials selects a short-lived, in-memory provider credential for
// a Worker-owned task. It deliberately has no persistence dependency: callers
// may persist only LeaseAudit, never Credential.
package credentials

import (
	"encoding/json"
	"fmt"
	"net/url"
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

// authHeaderNames lists HTTP header names that may carry credentials.
// Matching is case-insensitive prefix comparison against the canonical
// form (e.g. "authorization" matches "Authorization", "authorization").
var authHeaderNames = []string{
	"authorization",
	"x-api-key",
	"api-key",
	"proxy-authorization",
}

// isAuthHeader reports whether name (case-insensitive) is an
// authentication-related HTTP header whose value should be redacted.
func isAuthHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, candidate := range authHeaderNames {
		if strings.EqualFold(lower, candidate) {
			return true
		}
	}
	return false
}

// sanitizeURL returns a copy of raw with userinfo and sensitive query
// parameters (api_key, key, token, secret, password) removed. If raw
// is not a valid URL it is returned unchanged. This prevents
// credentials embedded in URLs from leaking through serialisation.
func sanitizeURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Clear userinfo (e.g. https://user:pass@host).
	u.User = nil
	// Remove sensitive query parameters.
	q := u.Query()
	stripped := false
	for _, param := range []string{"api_key", "key", "token", "secret", "password"} {
		if q.Has(param) {
			q.Del(param)
			stripped = true
		}
	}
	if stripped {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// MarshalJSON replaces the APIKey with [redacted], redacts ExtraHeaders
// values, and strips credentials from BaseURL so that accidental
// serialisation — through a log line, an error message, or an API
// response — never exposes a provider credential.
//
// URL sanitisation (P1): BaseURL is parsed and userinfo + sensitive
// query params (api_key, key, token, secret, password) are removed
// before serialisation. This covers URLs like
// https://key:secret@host/path?api_key=leak.
func (c Credential) MarshalJSON() ([]byte, error) {
	type Alias Credential
	safe := Alias(c)
	if safe.APIKey != "" {
		safe.APIKey = "[redacted]"
	}
	// Strip embedded credentials from BaseURL.
	safe.BaseURL = sanitizeURL(safe.BaseURL)
	if len(safe.ExtraHeaders) > 0 {
		redacted := make(map[string]string, len(safe.ExtraHeaders))
		for k, v := range safe.ExtraHeaders {
			if v == "" {
				redacted[k] = ""
			} else if isAuthHeader(k) {
				redacted[k] = "[redacted]"
			} else {
				// Conservatively redact all ExtraHeaders values;
				// configuration may carry credentials under arbitrary
				// header names.
				redacted[k] = "[redacted]"
			}
		}
		safe.ExtraHeaders = redacted
	}
	return json.Marshal(safe)
}

// GoString prevents %#v from exposing the API key by delegating to the safe
// JSON representation.
func (c Credential) GoString() string {
	safe, _ := c.MarshalJSON()
	return string(safe)
}

// Format implements fmt.Formatter so that %v, %+v, and %s never expose the
// API key — even when Credential is a field of Lease printed with %+v.
func (c Credential) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v', 's':
		safe, _ := c.MarshalJSON()
		f.Write(safe)
	default:
		fmt.Fprintf(f, "%%!%c(credentials.Credential)", verb)
	}
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
// json:"-" prevents JSON serialisation of APIKey and BaseURL.
// GoString and Format provide the same protection for %#v/%+v/%v/%s
// so that a hand-crafted LeaseAudit{APIKey:"secret"} never leaks
// through fmt formatting.
type LeaseAudit struct {
	JobID     string    `json:"job_id"`
	WorkerID  string    `json:"worker_id"`
	Provider  string    `json:"provider"`
	Operation Operation `json:"operation"`
	ExpiresAt time.Time `json:"expires_at"`
	// These explicit blanks make it impossible to accidentally treat an audit
	// record as a usable provider configuration. json:"-" prevents even
	// hand-crafted structs from leaking a key through serialisation.
	APIKey  string `json:"-"`
	BaseURL string `json:"-"`
}

// GoString prevents %#v from exposing APIKey and BaseURL. It builds a
// safe representation manually so that json:"-" fields are never
// included.
func (a LeaseAudit) GoString() string {
	return fmt.Sprintf("credentials.LeaseAudit{JobID:%q WorkerID:%q Provider:%q Operation:%q ExpiresAt:%s APIKey:[redacted] BaseURL:[redacted]}",
		a.JobID, a.WorkerID, a.Provider, a.Operation, a.ExpiresAt.Format(time.RFC3339))
}

// Format implements fmt.Formatter so that %v, %+v, and %s never expose
// APIKey or BaseURL. Delegates to GoString for a safe representation.
func (a LeaseAudit) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v', 's':
		f.Write([]byte(a.GoString()))
	default:
		fmt.Fprintf(f, "%%!%c(credentials.LeaseAudit)", verb)
	}
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
