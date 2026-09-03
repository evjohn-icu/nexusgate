package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// CostSummary is the trusted-read view of the cost ledger: the accumulated
// estimates for the current UTC day and the current UTC month, in the
// channels' configured relative unit. It is a guide, never a billing record.
type CostSummary struct {
	TodayEstimate float64 `json:"today_estimate"`
	MonthEstimate float64 `json:"month_estimate"`
}

// costDayLayout is the UTC calendar-day shape the ledger attributes estimates
// to (YYYY-MM-DD, zero-padded), and costMonthLayout its year-month prefix.
const (
	costDayLayout   = "2006-01-02"
	costMonthLayout = "2006-01"
)

// costRates is the cost metadata of the channel that prices a model run,
// kept as components because the estimate formula applies the per-minute
// rates to the asset's duration while the per-request rate is counted once.
type costRates struct {
	perRequest     float64
	perVideoMinute float64
	perAudioMinute float64
}

func (c costRates) priced() bool {
	return c.perRequest > 0 || c.perVideoMinute > 0 || c.perAudioMinute > 0
}

// channelCostRatesFor resolves the channel whose cost metadata should price a
// model run. The run's provider name is preferred so a capability with
// several channels prices against the one that actually served; when no
// channel matches by provider, the first channel of the capability that
// carries any cost metadata is used — an estimate is a guide, and a run that
// went through a channel the provider name no longer matches (e.g. a renamed
// channel) must still land somewhere rather than silently vanish. A
// capability with no cost-configured channel yields zero rates: the caller's
// no-op.
func channelCostRatesFor(channels []domain.ProviderChannel, provider string) costRates {
	provider = strings.TrimSpace(provider)
	var firstPriced costRates
	foundFirst := false
	for _, channel := range channels {
		rates := costRates{
			perRequest:     channel.CostPerRequest,
			perVideoMinute: channel.CostPerVideoMinute,
			perAudioMinute: channel.CostPerAudioMinute,
		}
		if !rates.priced() {
			continue
		}
		if !foundFirst {
			firstPriced, foundFirst = rates, true
		}
		if provider != "" && strings.TrimSpace(channel.ProviderName) == provider {
			return rates
		}
	}
	return firstPriced
}

// RecordAnalysisCostEstimate accumulates one estimate for a committed model
// run. capability is the provider-channel capability (video_analysis, asr);
// provider and model are the identities the commit recorded; durationMS is
// the asset's duration, which stands in for both the video minutes and the
// audio minutes — media metadata does not split audio/video length, so the
// per-video-minute and per-audio-minute components both price the whole
// duration. The estimate is per_request + (per_video_minute +
// per_audio_minute) * duration_minutes. No channel with cost metadata for
// the capability is a no-op: the estimate is a guide, so a channel that
// declares no price contributes nothing.
func (s *Service) RecordAnalysisCostEstimate(ctx context.Context, capability, provider, model, assetID string, durationMS int64) error {
	channels, err := s.repo.ListProviderChannels(ctx, capability)
	if err != nil {
		return err
	}
	rates := channelCostRatesFor(channels, provider)
	if !rates.priced() {
		return nil
	}
	minutes := float64(durationMS) / 60000.0
	estimate := rates.perRequest + (rates.perVideoMinute+rates.perAudioMinute)*minutes
	if estimate <= 0 {
		return nil
	}
	return s.repo.RecordCostEstimate(ctx, domain.CostEntry{
		Day:        time.Now().UTC().Format(costDayLayout),
		Capability: capability,
		Provider:   provider,
		Model:      model,
		AssetID:    assetID,
		Estimate:   estimate,
	})
}

// CostSummary returns the accumulated estimates for the current UTC day and
// month. A repository failure on either sum is returned as-is: the ledger is
// the source of truth for this read, unlike the commit path where a recording
// failure must never fail the job that already succeeded.
func (s *Service) CostSummary(ctx context.Context) (CostSummary, error) {
	now := time.Now().UTC()
	today, err := s.repo.CostEstimateForDay(ctx, now.Format(costDayLayout))
	if err != nil {
		return CostSummary{}, err
	}
	month, err := s.repo.CostEstimateForMonth(ctx, now.Format(costMonthLayout))
	if err != nil {
		return CostSummary{}, err
	}
	return CostSummary{TodayEstimate: today, MonthEstimate: month}, nil
}

// recordAnalysisCostEstimate is the pipeline-side wrapper NewService wires as
// the pipeline's cost hook. It swallows errors: the analysis has already
// committed, the ledger is a guide, and a recording failure must never fail
// the job — the same argument that keeps the post-commit embedding hook from
// failing jobs.
func (s *Service) recordAnalysisCostEstimate(ctx context.Context, capability, provider, model, assetID string, durationMS int64) {
	if err := s.RecordAnalysisCostEstimate(ctx, capability, provider, model, assetID, durationMS); err != nil {
		slog.Warn("cost ledger: failed to record analysis cost estimate", "capability", capability, "asset_id", assetID, "error", err)
	}
}
