package providerchannels

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/providerpool"
)

func TestLegacyConfigConversionPreservesOrderedRoutesWithoutSecretValues(t *testing.T) {
	providers := config.ProvidersConfig{
		ASRPrimary:  "stepfun",
		ASRFallback: "qwen",
		StepFun: config.ProviderConfig{
			Enabled: true, Protocol: "step_sse", BaseURL: "https://step.example", Path: "audio/asr/sse",
			APIKey: "step-secret", Model: "step-model",
		},
		Qwen: config.ProviderConfig{
			Enabled: true, Protocol: "openai_chat", BaseURL: "https://qwen.example", Path: "chat/completions",
			APIKey: "qwen-secret", Model: "qwen-model",
		},
	}

	channels, err := FromLegacyConfig(providers)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 {
		t.Fatalf("converted channels = %d, want 2: %+v", len(channels), channels)
	}
	if got := channels[0].ProviderName; got != "stepfun" {
		t.Fatalf("primary provider = %q, want stepfun", got)
	}
	if got := channels[1].ProviderName; got != "qwen" {
		t.Fatalf("fallback provider = %q, want qwen", got)
	}
	for i, channel := range channels {
		if len(channel.Capabilities) != 1 || channel.Capabilities[0] != CapabilityASR {
			t.Fatalf("channel[%d] capabilities = %+v, want [%q]", i, channel.Capabilities, CapabilityASR)
		}
		if len(channel.Members) != 1 {
			t.Fatalf("channel[%d] members = %+v, want one member", i, channel.Members)
		}
		if channel.Members[0].SecretRef == "" {
			t.Fatalf("channel[%d] has no secret reference", i)
		}
	}

	raw, err := json.Marshal(channels)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); strings.Contains(got, "step-secret") || strings.Contains(got, "qwen-secret") {
		t.Fatalf("legacy conversion leaked a secret value: %s", got)
	}
}

func TestExecutorRetriesOneSameProviderMemberThenUsesNextProviderRoute(t *testing.T) {
	channels := []Channel{
		{
			ID: "channel-a", Label: "Provider A", ProviderName: "provider-a", Enabled: true, RouteOrder: 0,
			Capabilities: []Capability{CapabilityVideoAnalysis},
			Members: []Member{
				{ID: "a-1", ChannelID: "channel-a", ProviderName: "provider-a", Label: "primary", SecretRef: "provider/a-1", Enabled: true},
				{ID: "a-2", ChannelID: "channel-a", ProviderName: "provider-a", Label: "backup", SecretRef: "provider/a-2", Enabled: true},
				{ID: "a-3", ChannelID: "channel-a", ProviderName: "provider-a", Label: "unused", SecretRef: "provider/a-3", Enabled: true},
			},
		},
		{
			ID: "channel-b", Label: "Provider B", ProviderName: "provider-b", Enabled: true, RouteOrder: 1,
			Capabilities: []Capability{CapabilityVideoAnalysis},
			Members:      []Member{{ID: "b-1", ChannelID: "channel-b", ProviderName: "provider-b", Label: "primary", SecretRef: "provider/b-1", Enabled: true}},
		},
		{
			ID: "channel-text", Label: "Text only", ProviderName: "text-provider", Enabled: true, RouteOrder: 0,
			Capabilities: []Capability{"text"},
			Members:      []Member{{ID: "text-1", ChannelID: "channel-text", ProviderName: "text-provider", Label: "primary", SecretRef: "provider/text-1", Enabled: true}},
		},
	}

	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	var calls []Invocation
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation)
		if invocation.MemberID != "b-1" {
			return providerpool.HTTPError{Code: 503, Err: errors.New("temporary provider failure")}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := invocationIDs(calls); !equalStrings(got, []string{"a-1", "a-2", "b-1"}) {
		t.Fatalf("video invocation order = %v, want [a-1 a-2 b-1]", got)
	}
	if calls[0].ProviderName != "provider-a" || calls[1].ProviderName != "provider-a" || calls[2].ProviderName != "provider-b" {
		t.Fatalf("provider route crossed unexpectedly: %+v", calls)
	}
	for _, call := range calls {
		if call.Capability != CapabilityVideoAnalysis || call.SecretRef == "" {
			t.Fatalf("invocation metadata = %+v", call)
		}
	}
	invocationJSON, err := json.Marshal(calls[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(invocationJSON), "api_key") || strings.Contains(string(invocationJSON), "temporary provider failure") {
		t.Fatalf("invocation exposed worker-secret/error material: %s", invocationJSON)
	}

	var textCalls []Invocation
	if err := executor.Execute(context.Background(), Capability("text"), func(_ context.Context, invocation Invocation) error {
		textCalls = append(textCalls, invocation)
		return nil
	}); err != nil {
		t.Fatalf("text Execute returned error: %v", err)
	}
	if got := invocationIDs(textCalls); !equalStrings(got, []string{"text-1"}) {
		t.Fatalf("text invocation order = %v, want [text-1]", got)
	}
}

func TestExecutorStatusSnapshotContainsMetadataAndNoSecretValues(t *testing.T) {
	channels := []Channel{{
		ID: "channel-a", Label: "Provider A", ProviderName: "provider-a", Enabled: true, RouteOrder: 2,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members:      []Member{{ID: "a-1", ChannelID: "channel-a", ProviderName: "provider-a", Label: "primary", SecretRef: "provider/a-1", SecretConfigured: true, Enabled: true}},
	}}
	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error { return nil }); err != nil {
		t.Fatal(err)
	}

	snapshot := executor.Snapshot()
	if len(snapshot.Channels) != 1 || snapshot.Channels[0].ID != "channel-a" {
		t.Fatalf("snapshot channels = %+v", snapshot.Channels)
	}
	if got := snapshot.Channels[0].Members[0].Successes; got != 1 {
		t.Fatalf("member successes = %d, want 1", got)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "provider/a-1") || strings.Contains(string(raw), "api-key") {
		t.Fatalf("status snapshot contains secret material: %s", raw)
	}
}

func TestCapabilitySnapshotUsesOrderedProviderRoutes(t *testing.T) {
	channels := []Channel{
		{
			ID: "late", ProviderName: "late-provider", Enabled: true, RouteOrder: 20,
			Capabilities: []Capability{CapabilityVideoAnalysis}, Members: []Member{{ID: "late-member", Enabled: true}},
		},
		{
			ID: "early", ProviderName: "early-provider", Enabled: true, RouteOrder: 10,
			Capabilities: []Capability{CapabilityVideoAnalysis}, Members: []Member{{ID: "early-member", Enabled: true}},
		},
	}
	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := executor.Snapshot(CapabilityVideoAnalysis)
	if len(snapshot.Channels) != 2 || snapshot.Channels[0].ID != "early" || snapshot.Channels[1].ID != "late" {
		t.Fatalf("ordered capability snapshot = %+v, want early then late", snapshot.Channels)
	}
}

func TestExecutorDoesNotCrossProviderOnNonRetryableFailure(t *testing.T) {
	channels := []Channel{
		{ID: "primary", ProviderName: "primary", Enabled: true, RouteOrder: 0, Capabilities: []Capability{CapabilityASR}, Members: []Member{{ID: "primary-member", Enabled: true}}},
		{ID: "fallback", ProviderName: "fallback", Enabled: true, RouteOrder: 1, Capabilities: []Capability{CapabilityASR}, Members: []Member{{ID: "fallback-member", Enabled: true}}},
	}
	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	var calls []Invocation
	err = executor.Execute(context.Background(), CapabilityASR, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation)
		return providerpool.HTTPError{Code: 401, Err: errors.New("unauthorized")}
	})
	if err == nil || len(calls) != 1 || calls[0].ProviderName != "primary" {
		t.Fatalf("non-retryable execution err=%v calls=%+v", err, calls)
	}
}

func TestExecutorSupportsIDAndSecretReferenceOperation(t *testing.T) {
	channels := []Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true, Capabilities: []Capability{CapabilityASR},
		Members: []Member{{ID: "member-a", SecretRef: "provider/member-a", Enabled: true}},
	}}
	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	var memberID, secretRef string
	if err := executor.Execute(context.Background(), CapabilityASR, func(_ context.Context, gotID, gotSecretRef string) error {
		memberID, secretRef = gotID, gotSecretRef
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if memberID != "member-a" || secretRef != "provider/member-a" {
		t.Fatalf("operation received memberID=%q secretRef=%q", memberID, secretRef)
	}
}

func invocationIDs(calls []Invocation) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.MemberID)
	}
	return ids
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
