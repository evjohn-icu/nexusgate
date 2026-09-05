package providerchannels

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/providerpool"
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

// "Every key gets three tries" is the operator-facing promise, so a channel is
// worth three members before the route moves on to the next provider.
func TestExecutorTriesThreeMembersOfOneProviderThenUsesNextProviderRoute(t *testing.T) {
	channels := []Channel{
		{
			ID: "channel-a", Label: "Provider A", ProviderName: "provider-a", Enabled: true, RouteOrder: 0,
			Capabilities: []Capability{CapabilityVideoAnalysis},
			Members: []Member{
				{ID: "a-1", ChannelID: "channel-a", ProviderName: "provider-a", Label: "primary", SecretRef: "provider/a-1", Enabled: true},
				{ID: "a-2", ChannelID: "channel-a", ProviderName: "provider-a", Label: "backup", SecretRef: "provider/a-2", Enabled: true},
				{ID: "a-3", ChannelID: "channel-a", ProviderName: "provider-a", Label: "third", SecretRef: "provider/a-3", Enabled: true},
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
	if got := invocationIDs(calls); !equalStrings(got, []string{"a-1", "a-2", "a-3", "b-1"}) {
		t.Fatalf("video invocation order = %v, want [a-1 a-2 a-3 b-1]", got)
	}
	if calls[0].ProviderName != "provider-a" || calls[1].ProviderName != "provider-a" || calls[2].ProviderName != "provider-a" || calls[3].ProviderName != "provider-b" {
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
	// 400: the request is what the provider rejected, and the fallback would
	// reject it identically. Crossing the boundary would only buy a second bill.
	err = executor.Execute(context.Background(), CapabilityASR, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation)
		return providerpool.HTTPError{Code: 400, Err: errors.New("bad request")}
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

// A spent monthly quota answers 429 on every key at once. The caller has to be
// able to tell that from one unlucky call without reading error text, because
// the two need opposite handling: back off for seconds, or stop asking for
// hours.
func TestExecutorReportsRouteExhaustionWhenEveryKeyFailsRetryably(t *testing.T) {
	channels := []Channel{
		{
			ID: "channel-a", ProviderName: "provider-a", Enabled: true, RouteOrder: 0,
			Capabilities: []Capability{CapabilityVideoAnalysis},
			Members: []Member{
				{ID: "a-1", Enabled: true}, {ID: "a-2", Enabled: true}, {ID: "a-3", Enabled: true},
			},
		},
		{
			ID: "channel-b", ProviderName: "provider-b", Enabled: true, RouteOrder: 1,
			Capabilities: []Capability{CapabilityVideoAnalysis},
			Members:      []Member{{ID: "b-1", Enabled: true}},
		},
	}
	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	quota := providerpool.HTTPError{Code: 429, Err: errors.New("monthly quota exhausted")}
	var calls []string
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return quota
	})
	if !errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("every key failing must surface as ErrRouteExhausted, got %v", err)
	}
	// The underlying failure has to stay reachable: it is what the operator
	// reads on the progress page to find out which wall they hit.
	if !strings.Contains(err.Error(), "monthly quota exhausted") {
		t.Fatalf("exhaustion error dropped the cause: %v", err)
	}
	if !equalStrings(calls, []string{"a-1", "a-2", "a-3", "b-1"}) {
		t.Fatalf("every enabled member must be tried before exhaustion is claimed: %v", calls)
	}

	// The second call is the shape that matters most in practice: providerpool
	// has now cooled every member, so no request is issued at all. That must
	// still read as exhaustion, or the next job spends its whole attempt
	// budget discovering a wall this one already found.
	before := len(calls)
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, invocation Invocation) error {
		calls = append(calls, invocation.MemberID)
		return quota
	})
	if len(calls) != before {
		t.Fatalf("cooled route should not have issued a request: %v", calls)
	}
	if !errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("a fully cooled route must still report exhaustion, got %v", err)
	}
}

// The distinction only means anything if it is narrow: a single failure, or a
// route that was never tried, must not be dressed up as exhaustion.
func TestExecutorDoesNotClaimExhaustionWhileAKeyIsStillUntried(t *testing.T) {
	channels := []Channel{{
		ID: "channel-a", ProviderName: "provider-a", Enabled: true,
		Capabilities: []Capability{CapabilityVideoAnalysis},
		Members: []Member{
			{ID: "a-1", Enabled: true}, {ID: "a-2", Enabled: true}, {ID: "a-3", Enabled: true},
			{ID: "a-4", Enabled: true},
		},
	}}
	executor, err := NewExecutor(channels)
	if err != nil {
		t.Fatal(err)
	}
	err = executor.Execute(context.Background(), CapabilityVideoAnalysis, func(_ context.Context, _ Invocation) error {
		return providerpool.HTTPError{Code: 503, Err: errors.New("temporary provider failure")}
	})
	if err == nil {
		t.Fatal("expected the retryable failure to be returned")
	}
	if errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("three attempts left a fourth key untried; that is not exhaustion: %v", err)
	}

	// An unroutable capability is a configuration mistake, not an outage: it
	// must keep reporting ErrNoRoute so the operator is told to fix it rather
	// than being made to wait hours for nothing to change.
	err = executor.Execute(context.Background(), CapabilityASR, func(_ context.Context, _ Invocation) error { return nil })
	if !errors.Is(err, ErrNoRoute) || errors.Is(err, ErrRouteExhausted) {
		t.Fatalf("missing route must stay ErrNoRoute, got %v", err)
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
