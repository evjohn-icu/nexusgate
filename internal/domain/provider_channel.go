package domain

import "time"

// ProviderChannel is a non-secret route for one capability. API keys live
// exclusively in the Hub secret store and are represented here by member
// SecretRef values only.
type ProviderChannel struct {
	ID           string                  `json:"id"`
	Capability   string                  `json:"capability"`
	Label        string                  `json:"label"`
	ProviderName string                  `json:"provider_name"`
	Protocol     string                  `json:"protocol,omitempty"`
	Endpoint     string                  `json:"endpoint,omitempty"`
	Model        string                  `json:"model,omitempty"`
	Enabled      bool                    `json:"enabled"`
	RouteOrder   int                     `json:"route_order"`
	Members      []ProviderChannelMember `json:"members,omitempty"`
	CreatedAt    time.Time               `json:"created_at"`
	UpdatedAt    time.Time               `json:"updated_at"`
}

type ProviderChannelMember struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channel_id"`
	Label       string `json:"label"`
	SecretRef   string `json:"secret_ref,omitempty"`
	SecretReady bool   `json:"secret_ready"`
	Enabled     bool   `json:"enabled"`
	Weight      int    `json:"weight"`
	MaxInflight int    `json:"max_inflight"`
}
