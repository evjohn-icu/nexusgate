// Package providerchannels bridges public provider-channel metadata to the
// capability-bound provider pool. It stores secret references only; secret
// values are intentionally outside this package's data model.
package providerchannels

import "strings"

// Capability identifies an operation that must remain on its own provider
// route. A request for one capability can never select a member from another.
type Capability string

const (
	CapabilityASR           Capability = "asr"
	CapabilityVideoAnalysis Capability = "video_analysis"
	CapabilityAlignment     Capability = "alignment"
	CapabilityTagCurator    Capability = "tag_curator"
	CapabilityEmbedding     Capability = "embedding"
	CapabilityRepurpose     Capability = "repurpose"
)

// Capabilities is an ordered, duplicate-free capability list.
type Capabilities []Capability

// Contains reports whether the capability is present.
func (c Capabilities) Contains(capability Capability) bool {
	capability = normalizeCapability(capability)
	for _, candidate := range c {
		if normalizeCapability(candidate) == capability {
			return true
		}
	}
	return false
}

// Member is public, non-secret provider-member metadata. SecretRef identifies
// where a Hub-side caller may retrieve a secret; it is never a secret value.
type Member struct {
	ID               string       `json:"id"`
	ChannelID        string       `json:"channel_id,omitempty"`
	Provider         string       `json:"provider,omitempty"`
	ProviderName     string       `json:"provider_name,omitempty"`
	Label            string       `json:"label"`
	SecretRef        string       `json:"secret_ref,omitempty"`
	SecretConfigured bool         `json:"secret_configured,omitempty"`
	Enabled          bool         `json:"enabled"`
	Weight           int          `json:"weight,omitempty"`
	MaxInflight      int          `json:"max_inflight,omitempty"`
	Capabilities     []Capability `json:"capabilities,omitempty"`
}

// Channel is one ordered provider route for one or more capabilities. It
// contains endpoint/model metadata but deliberately has no API-key field.
type Channel struct {
	ID             string       `json:"id"`
	Capability     Capability   `json:"capability,omitempty"`
	Capabilities   []Capability `json:"capabilities"`
	Label          string       `json:"label"`
	Provider       string       `json:"provider,omitempty"`
	ProviderName   string       `json:"provider_name"`
	Protocol       string       `json:"protocol,omitempty"`
	Endpoint       string       `json:"endpoint,omitempty"`
	Path           string       `json:"path,omitempty"`
	Model          string       `json:"model,omitempty"`
	AuthHeader     string       `json:"auth_header,omitempty"`
	AuthScheme     string       `json:"auth_scheme,omitempty"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
	Enabled        bool         `json:"enabled"`
	RouteOrder     int          `json:"route_order"`
	Members        []Member     `json:"members,omitempty"`
}

// Supports reports whether the channel is eligible for a capability.
func (c Channel) Supports(capability Capability) bool {
	capability = normalizeCapability(capability)
	if capability == "" {
		return false
	}
	if normalizeCapability(c.Capability) == capability {
		return true
	}
	return Capabilities(c.Capabilities).Contains(capability)
}

func (c Channel) provider() string {
	if value := strings.TrimSpace(c.Provider); value != "" {
		return value
	}
	return strings.TrimSpace(c.ProviderName)
}

func (c Channel) capabilities() []Capability {
	return normalizeCapabilities(c.Capability, c.Capabilities)
}

func (m Member) provider() string {
	if value := strings.TrimSpace(m.Provider); value != "" {
		return value
	}
	return strings.TrimSpace(m.ProviderName)
}

func normalizeCapability(capability Capability) Capability {
	return Capability(strings.TrimSpace(string(capability)))
}

func normalizeCapabilities(singular Capability, capabilities []Capability) []Capability {
	result := make([]Capability, 0, len(capabilities)+1)
	seen := make(map[Capability]struct{}, len(capabilities)+1)
	appendCapability := func(capability Capability) {
		capability = normalizeCapability(capability)
		if capability == "" {
			return
		}
		if _, exists := seen[capability]; exists {
			return
		}
		seen[capability] = struct{}{}
		result = append(result, capability)
	}
	appendCapability(singular)
	for _, capability := range capabilities {
		appendCapability(capability)
	}
	return result
}
