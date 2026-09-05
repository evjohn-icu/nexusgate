package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// costLedgerRepo supports the two methods RecordAnalysisCostEstimate
// exercises — ListProviderChannels (to resolve the serving channel's cost
// metadata) and the three ledger methods (to capture what gets recorded and
// answer the summary queries). Anything else would panic via the embedded nil
// interface, which keeps the fake honest about how narrow the exercised
// surface is.
type costLedgerRepo struct {
	Repository
	channels []domain.ProviderChannel
	recorded []domain.CostEntry
	daySum   float64
	moSum    float64
	listErr  error
}

func (r *costLedgerRepo) ListProviderChannels(_ context.Context, capability string) ([]domain.ProviderChannel, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if capability == "" {
		return r.channels, nil
	}
	var matched []domain.ProviderChannel
	for _, channel := range r.channels {
		if channel.Capability == capability {
			matched = append(matched, channel)
		}
	}
	return matched, nil
}

func (r *costLedgerRepo) RecordCostEstimate(_ context.Context, entry domain.CostEntry) error {
	r.recorded = append(r.recorded, entry)
	return nil
}

func (r *costLedgerRepo) CostEstimateForDay(_ context.Context, _ string) (float64, error) {
	return r.daySum, nil
}
func (r *costLedgerRepo) CostEstimateForMonth(_ context.Context, _ string) (float64, error) {
	return r.moSum, nil
}

func TestRecordAnalysisCostEstimatePricesChannelRates(t *testing.T) {
	ctx := context.Background()
	repo := &costLedgerRepo{channels: []domain.ProviderChannel{{
		ID: "channel-1", Capability: "video_analysis", ProviderName: "qwen", Model: "qwen-vl",
		CostPerRequest: 2.0, CostPerVideoMinute: 0.5, CostPerAudioMinute: 0.25,
	}}}
	service := &Service{repo: repo}

	// 90 seconds = 1.5 minutes: per_request 2.0 + (0.5 + 0.25) * 1.5 = 3.125.
	if err := service.RecordAnalysisCostEstimate(ctx, "video_analysis", "qwen", "qwen-vl", "asset-1", 90_000); err != nil {
		t.Fatal(err)
	}
	if len(repo.recorded) != 1 {
		t.Fatalf("expected one ledger entry, got %d", len(repo.recorded))
	}
	entry := repo.recorded[0]
	if entry.Estimate != 3.125 {
		t.Fatalf("estimate = %v, want 3.125", entry.Estimate)
	}
	if entry.Day != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("day = %q, want today's UTC calendar day", entry.Day)
	}
	if entry.Capability != "video_analysis" || entry.Provider != "qwen" || entry.Model != "qwen-vl" || entry.AssetID != "asset-1" {
		t.Fatalf("entry identity = %+v", entry)
	}
}

func TestRecordAnalysisCostEstimatePrefersMatchingProviderChannel(t *testing.T) {
	ctx := context.Background()
	// Two channels for the capability; the run's provider must be priced with
	// its own channel's rates, not the first priced channel's.
	repo := &costLedgerRepo{channels: []domain.ProviderChannel{
		{ID: "channel-a", Capability: "video_analysis", ProviderName: "gemini", Model: "gemini-flash", CostPerRequest: 5.0},
		{ID: "channel-b", Capability: "video_analysis", ProviderName: "qwen", Model: "qwen-vl", CostPerRequest: 1.0, CostPerVideoMinute: 0.1},
	}}
	service := &Service{repo: repo}
	if err := service.RecordAnalysisCostEstimate(ctx, "video_analysis", "qwen", "qwen-vl", "asset-1", 60_000); err != nil {
		t.Fatal(err)
	}
	if got := repo.recorded[0].Estimate; got != 1.1 {
		t.Fatalf("estimate = %v, want 1.1 (qwen channel: 1.0 + 0.1*1)", got)
	}
}

func TestRecordAnalysisCostEstimateNoOpsWithoutCostMetadata(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		channels []domain.ProviderChannel
	}{
		{"no channel at all", nil},
		{"channel without cost metadata", []domain.ProviderChannel{{ID: "channel-1", Capability: "video_analysis", ProviderName: "qwen"}}},
		{"channel with all-zero rates", []domain.ProviderChannel{{ID: "channel-1", Capability: "video_analysis", ProviderName: "qwen", CostPerRequest: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &costLedgerRepo{channels: tc.channels}
			service := &Service{repo: repo}
			if err := service.RecordAnalysisCostEstimate(ctx, "video_analysis", "qwen", "qwen-vl", "asset-1", 120_000); err != nil {
				t.Fatal(err)
			}
			if len(repo.recorded) != 0 {
				t.Fatalf("expected no ledger entry, got %d", len(repo.recorded))
			}
		})
	}
}

func TestRecordAnalysisCostEstimatePropagatesRepoErrors(t *testing.T) {
	ctx := context.Background()
	repo := &costLedgerRepo{listErr: errFakeUpsertRejected}
	service := &Service{repo: repo}
	if err := service.RecordAnalysisCostEstimate(ctx, "video_analysis", "qwen", "qwen-vl", "asset-1", 60_000); err != errFakeUpsertRejected {
		t.Fatalf("err = %v, want the repository error", err)
	}
}

// TestCostSummaryAggregatesTodayAndMonth pins the endpoint's shape: two sums
// from the two ledger queries, attributed to the current UTC day and month.
func TestCostSummaryAggregatesTodayAndMonth(t *testing.T) {
	ctx := context.Background()
	repo := &costLedgerRepo{daySum: 4.75, moSum: 6.75}
	service := &Service{repo: repo}
	summary, err := service.CostSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TodayEstimate != 4.75 || summary.MonthEstimate != 6.75 {
		t.Fatalf("summary = %+v, want today 4.75 month 6.75", summary)
	}
}

// TestProviderChannelCostValidation guards the create/update funnel: negative
// cost metadata is a sign mistake and must be rejected with
// ErrProviderChannelValidation (so the API shows the operator the message),
// while non-negative values pass.
func TestProviderChannelCostValidation(t *testing.T) {
	ctx := context.Background()
	service := &Service{repo: &fakeUpsertOnlyRepo{}}
	base := domain.ProviderChannel{
		Capability:   "video_analysis",
		Label:        "Cost channel",
		ProviderName: "qwen",
		Endpoint:     "https://example.invalid",
		Model:        "qwen-vl",
		Enabled:      true,
	}
	for _, tc := range []struct {
		name string
		mut  func(*domain.ProviderChannel)
	}{
		{"negative per request", func(c *domain.ProviderChannel) { c.CostPerRequest = -1 }},
		{"negative per video minute", func(c *domain.ProviderChannel) { c.CostPerVideoMinute = -0.1 }},
		{"negative per audio minute", func(c *domain.ProviderChannel) { c.CostPerAudioMinute = -2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := base
			tc.mut(&channel)
			_, err := service.SaveProviderChannel(ctx, channel, nil)
			if err == nil {
				t.Fatal("expected negative cost to be rejected")
			}
			if !strings.Contains(err.Error(), "must not be negative") {
				t.Fatalf("error should explain the sign mistake, got %q", err.Error())
			}
		})
	}
	channel := base
	channel.CostPerRequest, channel.CostPerVideoMinute, channel.CostPerAudioMinute = 1.5, 0.25, 0.1
	if _, err := service.SaveProviderChannel(ctx, channel, nil); err != nil {
		t.Fatalf("non-negative costs must be accepted: %v", err)
	}
}
