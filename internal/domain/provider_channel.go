package domain

import "time"

// ProviderChannel is a non-secret route for one capability. API keys live
// exclusively in the Hub secret store and are represented here by member
// SecretRef values only.
type ProviderChannel struct {
	ID           string `json:"id"`
	Capability   string `json:"capability"`
	Label        string `json:"label"`
	ProviderName string `json:"provider_name"`
	Protocol     string `json:"protocol,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Model        string `json:"model,omitempty"`
	Enabled      bool   `json:"enabled"`
	RouteOrder   int    `json:"route_order"`
	// CostPerRequest is the optional per-call cost component of this
	// channel's estimate. The unit is deliberately unspecified — it is a
	// relative unit chosen by the operator, and the ledger's sums are a
	// guide for cost tracking, never a billing record.
	CostPerRequest float64 `json:"cost_per_request,omitempty"`
	// CostPerVideoMinute is the optional per-video-minute cost component,
	// applied to an asset's duration in minutes when the estimate is
	// recorded. Same relative unit as CostPerRequest.
	CostPerVideoMinute float64 `json:"cost_per_video_minute,omitempty"`
	// CostPerAudioMinute is the optional per-audio-minute cost component.
	// The pipeline approximates audio duration with the asset's duration
	// (the metadata does not split audio/video lengths), so this is applied
	// against the same asset duration as CostPerVideoMinute.
	CostPerAudioMinute float64                 `json:"cost_per_audio_minute,omitempty"`
	Members            []ProviderChannelMember `json:"members,omitempty"`
	CreatedAt          time.Time               `json:"created_at"`
	UpdatedAt          time.Time               `json:"updated_at"`
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
