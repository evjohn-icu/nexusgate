package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/providerchannels"
	"github.com/evjohn-icu/timingdex/internal/providers"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
)

// providerChannelRepository is intentionally narrower than Repository. The
// runtime only needs the persisted route metadata; keeping this boundary small
// makes it possible to exercise routing without constructing the whole Hub.
type providerChannelRepository interface {
	ListProviderChannels(context.Context, string) ([]domain.ProviderChannel, error)
}

type providerSecretResolver interface {
	Has(context.Context, string) (bool, error)
	Resolve(string) (string, bool, error)
}

// ErrProviderChannelNotConfigured tells Service callers that neither a
// persisted channel nor a legacy provider exists. It is deliberately distinct
// from transport/protocol failures so deterministic fallbacks remain intact.
//
// It is marked permanent rather than given a second name: this condition
// already has one, and domain.ErrPermanentFailure is a property it carries,
// not an identity that replaces it. errors.Is against either resolves, so
// service.go's existing handling is untouched while the pipeline stops
// spending a retry ladder on a capability nobody has configured.
var ErrProviderChannelNotConfigured = domain.Permanent(errors.New("provider channel capability is not configured"))

// errProviderChannelSecretMissing reports a channel whose secret reference
// resolves to nothing in secretstore. The route exists and names a key that
// does not, which no amount of waiting fixes — an operator has to re-enter it
// at /providers. Named once here rather than rebuilt at each of the eight
// call sites so the eight cannot drift into eight slightly different
// sentences, which is how the substring list acquired dead entries.
var errProviderChannelSecretMissing = domain.Permanent(errors.New("provider channel secret is not configured"))

// errProviderChannelMultiframeUnsupported is the fail-fast for a video
// channel configured with the openai_multiframe protocol. Channel routing
// has no multiframe orchestration yet: channelVideo exposes only the plain
// video path, so a multiframe provider built here would be called with zero
// sampled frames and fail downstream as "multiframe summary call requires at
// least one frame" — the operator would be debugging an engine error instead
// of a configuration boundary. Naming the boundary at construction time beats
// running into it: a channel protocol is metadata the Hub already holds, so
// this verdict is permanent, like the other channel configuration failures.
var errProviderChannelMultiframeUnsupported = domain.Permanent(errors.New("openai_multiframe is currently supported through providers.local_vlm config only; provider-channel routing support is not available yet"))

// providerChannelRuntime is the Hub-side bridge from persisted channel
// metadata to the existing provider implementations. It deliberately stores
// no API-key value. A key is resolved only inside an Executor operation and is
// discarded after the provider call returns.
type providerChannelRuntime struct {
	repo    providerChannelRepository
	secrets providerSecretResolver
	cfg     config.Config

	legacyASR         providers.ASR
	legacyASRFallback providers.ASR
	legacyVideo       videoproviders.VideoUnderstandingProvider
	legacyCurator     providers.TagCurator
	legacyEmbedder    providers.Embedder
	legacyPlanner     providers.RepurposePlanner

	cacheMu   sync.Mutex
	executors map[providerchannels.Capability]cachedProviderExecutor

	// identityCache stores (name, model) pairs resolved together so that
	// Name() and Model() always report a consistent identity even when the
	// resolution times out. Each entry is keyed by capability + fallback
	// provider name so that primary and fallback instances of the same
	// capability (e.g. ASR) never share an entry, and carries the executor
	// route fingerprint at the time of resolution so that a channel edit
	// invalidates the cache before the TTL elapses.
	identityCacheMu sync.Mutex
	identityCache   map[string]cachedIdentity
}

// identityCacheTTL bounds how long a cached identity is reused before the
// next Name() / Model() call re-resolves. 30 s is long enough that UI
// refreshes and status endpoints reuse one resolution, and short enough
// that a channel edit takes effect without a Hub restart.
const identityCacheTTL = 30 * time.Second

type cachedIdentity struct {
	name        string
	model       string
	fingerprint string // executor route fingerprint at resolution time; mismatch → re-resolve
	expiresAt   time.Time
}

type cachedProviderExecutor struct {
	fingerprint string
	executor    *providerchannels.Executor
	hasRoute    bool
}

func newProviderChannelRuntime(repo providerChannelRepository, cfg config.Config, secrets providerSecretResolver, legacyASR, legacyASRFallback providers.ASR, legacyVideo videoproviders.VideoUnderstandingProvider, legacyCurator providers.TagCurator, legacyEmbedder providers.Embedder, legacyPlanner providers.RepurposePlanner) *providerChannelRuntime {
	return &providerChannelRuntime{
		repo:              repo,
		secrets:           secrets,
		cfg:               cfg,
		legacyASR:         legacyASR,
		legacyASRFallback: legacyASRFallback,
		legacyVideo:       legacyVideo,
		legacyCurator:     legacyCurator,
		legacyEmbedder:    legacyEmbedder,
		legacyPlanner:     legacyPlanner,
		executors:         make(map[providerchannels.Capability]cachedProviderExecutor),
		identityCache:     make(map[string]cachedIdentity),
	}
}

func (r *providerChannelRuntime) asr() providers.ASR {
	return &channelASR{runtime: r}
}

func (r *providerChannelRuntime) asrFallback() providers.ASR {
	return &channelASR{runtime: r, legacyOverride: r.legacyASRFallback}
}

func (r *providerChannelRuntime) video() videoproviders.VideoUnderstandingProvider {
	return &channelVideo{runtime: r, prepared: make(map[string]providerChannelPreparedVideo)}
}

func (r *providerChannelRuntime) curator() providers.TagCurator {
	return &channelTagCurator{runtime: r}
}

func (r *providerChannelRuntime) embedder() providers.Embedder {
	return &channelEmbedder{runtime: r}
}

func (r *providerChannelRuntime) planner() providers.RepurposePlanner {
	return &channelPlanner{runtime: r}
}

type channelASR struct {
	runtime        *providerChannelRuntime
	legacyOverride providers.ASR
}

func (p *channelASR) Name() string {
	fallback := p.legacyOverride
	if fallback == nil {
		fallback = p.runtime.legacyASR
	}
	name, _ := p.runtime.identity(providerchannels.CapabilityASR, fallback)
	return name
}

func (p *channelASR) Model() string {
	fallback := p.legacyOverride
	if fallback == nil {
		fallback = p.runtime.legacyASR
	}
	_, model := p.runtime.identity(providerchannels.CapabilityASR, fallback)
	return model
}

func (p *channelASR) Transcribe(ctx context.Context, req common.TranscribeRequest) (domain.Transcript, error) {
	if p.legacyOverride != nil {
		return p.legacyOverride.Transcribe(ctx, req)
	}
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityASR)
	if err != nil {
		return domain.Transcript{}, err
	}
	if !hasRoute {
		if p.runtime.legacyASR == nil {
			return domain.Transcript{}, ErrProviderChannelNotConfigured
		}
		return p.runtime.legacyASR.Transcribe(ctx, req)
	}

	var transcript domain.Transcript
	err = executor.Execute(ctx, providerchannels.CapabilityASR, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.asrProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		transcript, err = provider.Transcribe(callCtx, req)
		if err != nil {
			return redactError(err, key)
		}
		transcript = redactTranscript(transcript, key)
		return nil
	})
	if err != nil {
		return domain.Transcript{}, err
	}
	return transcript, nil
}

type channelVideo struct {
	runtime  *providerChannelRuntime
	mu       sync.Mutex
	prepared map[string]providerChannelPreparedVideo
}

// providerChannelPreparedVideo keeps only the selected public invocation. The
// key is resolved again when Analyze runs, so a prepared-file binding never
// retains an API key and cannot outlive a secret-store rotation in memory.
type providerChannelPreparedVideo struct {
	invocation providerchannels.Invocation
}

func (p *channelVideo) Name() string {
	name, _ := p.runtime.identity(providerchannels.CapabilityVideoAnalysis, p.runtime.legacyVideo)
	return name
}

func (p *channelVideo) Model() string {
	_, model := p.runtime.identity(providerchannels.CapabilityVideoAnalysis, p.runtime.legacyVideo)
	return model
}

func (p *channelVideo) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{
		videoproviders.CapabilityVideoAnalysis,
		videoproviders.CapabilitySceneAnalysis,
		videoproviders.CapabilityShotAnalysis,
	}
}

// RequiresVideoPreparation makes Gemini's resumable Files API visible to the
// existing Pipeline. OpenAI-compatible routes use an inline data URL and do
// not need this preparation step.
func (p *channelVideo) RequiresVideoPreparation() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityVideoAnalysis)
	if err == nil && hasRoute {
		routes := executor.Route(providerchannels.CapabilityVideoAnalysis)
		return len(routes) > 0 && routes[0].ProviderName == "gemini"
	}
	if p.runtime != nil && p.runtime.legacyVideo != nil {
		_, ok := p.runtime.legacyVideo.(videoproviders.VideoPreparer)
		return ok
	}
	return false
}

// MaxInlineVideoBytes reports the inline ceiling for the legacy route only.
//
// Building a channel's provider requires resolving its secret, and this is
// asked before every analysis merely to size the request — decrypting a
// provider key to read a constant is the wrong trade. A channel route
// therefore answers 0, which the caller reads as "unknown" and satisfies with
// the conservative default. The effect is more windows than strictly
// necessary, never a request too large to send; channels have no field in
// which to declare a limit yet, so there is nothing more precise to report.
func (p *channelVideo) MaxInlineVideoBytes() int64 {
	if p.runtime != nil && p.runtime.legacyVideo != nil {
		if limiter, ok := p.runtime.legacyVideo.(videoproviders.InlineVideoLimiter); ok {
			return limiter.MaxInlineVideoBytes()
		}
	}
	return 0
}

// PrepareVideo selects and uploads through the same Executor route that owns
// the following analysis. The selected invocation is bound by the returned
// remote URI, not by a secret value; Analyze consumes that binding once.
func (p *channelVideo) PrepareVideo(ctx context.Context, req videoproviders.PrepareVideoRequest) (videoproviders.PreparedVideo, error) {
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityVideoAnalysis)
	if err != nil {
		return videoproviders.PreparedVideo{}, err
	}
	if !hasRoute {
		if preparer, ok := p.runtime.legacyVideo.(videoproviders.VideoPreparer); ok {
			return preparer.PrepareVideo(ctx, req)
		}
		return videoproviders.PreparedVideo{}, ErrProviderChannelNotConfigured
	}

	var prepared videoproviders.PreparedVideo
	var selected providerchannels.Invocation
	err = executor.Execute(ctx, providerchannels.CapabilityVideoAnalysis, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		if invocation.ProviderName != "gemini" {
			return domain.Permanent(fmt.Errorf("video provider %q does not support remote file preparation", invocation.ProviderName))
		}
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.videoProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		preparer, ok := provider.(videoproviders.VideoPreparer)
		if !ok {
			return domain.Permanent(fmt.Errorf("video provider %q does not support remote file preparation", invocation.ProviderName))
		}
		prepared, err = preparer.PrepareVideo(callCtx, req)
		if err != nil {
			return redactError(err, key)
		}
		if strings.TrimSpace(prepared.RemoteURI) == "" {
			return fmt.Errorf("video provider %q returned an empty remote URI", invocation.ProviderName)
		}
		selected = invocation
		return nil
	})
	if err != nil {
		return videoproviders.PreparedVideo{}, err
	}
	p.mu.Lock()
	if p.prepared == nil {
		p.prepared = make(map[string]providerChannelPreparedVideo)
	}
	p.prepared[prepared.RemoteURI] = providerChannelPreparedVideo{invocation: selected}
	p.mu.Unlock()
	return prepared, nil
}

func (p *channelVideo) Analyze(ctx context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	if binding, ok := p.takePrepared(input.RemoteURI); ok {
		key, secretOK, resolveErr := p.runtime.resolve(binding.invocation.SecretRef)
		if resolveErr != nil {
			return videoanalysis.Result{}, "", resolveErr
		}
		if !secretOK {
			return videoanalysis.Result{}, "", errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.videoProvider(binding.invocation, key)
		if buildErr != nil {
			return videoanalysis.Result{}, "", buildErr
		}
		result, raw, analyzeErr := provider.Analyze(ctx, input)
		raw = redactString(raw, key)
		if analyzeErr != nil {
			return videoanalysis.Result{}, raw, redactError(analyzeErr, key)
		}
		return redactVideoResult(result, key), raw, nil
	}

	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityVideoAnalysis)
	if err != nil {
		return videoanalysis.Result{}, "", err
	}
	if !hasRoute {
		if p.runtime.legacyVideo == nil {
			return videoanalysis.Result{}, "", ErrProviderChannelNotConfigured
		}
		return p.runtime.legacyVideo.Analyze(ctx, input)
	}

	var result videoanalysis.Result
	var raw string
	err = executor.Execute(ctx, providerchannels.CapabilityVideoAnalysis, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.videoProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		providerInput := input
		// A Gemini Files URI is scoped to the Gemini preparation path. Do not
		// accidentally send it to an OpenAI-compatible fallback provider.
		if invocation.ProviderName != "gemini" {
			providerInput.RemoteURI = ""
		}
		result, raw, err = provider.Analyze(callCtx, providerInput)
		raw = redactString(raw, key)
		if err != nil {
			return redactError(err, key)
		}
		result = redactVideoResult(result, key)
		return nil
	})
	if err != nil {
		return videoanalysis.Result{}, redactString(raw, ""), err
	}
	return result, raw, nil
}

func (p *channelVideo) takePrepared(remoteURI string) (providerChannelPreparedVideo, bool) {
	if strings.TrimSpace(remoteURI) == "" {
		return providerChannelPreparedVideo{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	binding, ok := p.prepared[remoteURI]
	if ok {
		delete(p.prepared, remoteURI)
	}
	return binding, ok
}

type channelTagCurator struct{ runtime *providerChannelRuntime }

func (p *channelTagCurator) Name() string {
	name, _ := p.runtime.identity(providerchannels.CapabilityTagCurator, p.runtime.legacyCurator)
	return name
}

func (p *channelTagCurator) Model() string {
	_, model := p.runtime.identity(providerchannels.CapabilityTagCurator, p.runtime.legacyCurator)
	return model
}

func (p *channelTagCurator) Curate(ctx context.Context, unresolved []domain.UnresolvedTag, existing []domain.CanonicalTag) ([]domain.TagProposal, error) {
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityTagCurator)
	if err != nil {
		return nil, err
	}
	if !hasRoute {
		if p.runtime.legacyCurator == nil {
			return nil, ErrProviderChannelNotConfigured
		}
		return p.runtime.legacyCurator.Curate(ctx, unresolved, existing)
	}
	var proposals []domain.TagProposal
	err = executor.Execute(ctx, providerchannels.CapabilityTagCurator, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.tagCuratorProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		proposals, err = provider.Curate(callCtx, unresolved, existing)
		if err != nil {
			return redactError(err, key)
		}
		proposals = redactTagProposals(proposals, key)
		return nil
	})
	return proposals, err
}

func (p *channelTagCurator) SummarizeLibrary(ctx context.Context, input domain.LibrarySummaryInput) (domain.LibrarySummaryDraft, error) {
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityTagCurator)
	if err != nil {
		return domain.LibrarySummaryDraft{}, err
	}
	if !hasRoute {
		if p.runtime.legacyCurator == nil {
			return domain.LibrarySummaryDraft{}, ErrProviderChannelNotConfigured
		}
		return p.runtime.legacyCurator.SummarizeLibrary(ctx, input)
	}
	var draft domain.LibrarySummaryDraft
	err = executor.Execute(ctx, providerchannels.CapabilityTagCurator, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.tagCuratorProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		draft, err = provider.SummarizeLibrary(callCtx, input)
		if err != nil {
			return redactError(err, key)
		}
		draft = redactLibrarySummary(draft, key)
		return nil
	})
	return draft, err
}

type channelEmbedder struct{ runtime *providerChannelRuntime }

func (p *channelEmbedder) Name() string {
	name, _ := p.runtime.identity(providerchannels.CapabilityEmbedding, p.runtime.legacyEmbedder)
	return name
}

func (p *channelEmbedder) Model() string {
	_, model := p.runtime.identity(providerchannels.CapabilityEmbedding, p.runtime.legacyEmbedder)
	return model
}

func (p *channelEmbedder) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityEmbedding)
	if err != nil {
		return nil, err
	}
	if !hasRoute {
		if p.runtime.legacyEmbedder == nil {
			return nil, ErrProviderChannelNotConfigured
		}
		return p.runtime.legacyEmbedder.Embed(ctx, inputs)
	}
	var vectors [][]float64
	err = executor.Execute(ctx, providerchannels.CapabilityEmbedding, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.embedderProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		vectors, err = provider.Embed(callCtx, inputs)
		if err != nil {
			return redactError(err, key)
		}
		return nil
	})
	return vectors, err
}

type channelPlanner struct{ runtime *providerChannelRuntime }

func (p *channelPlanner) Name() string {
	name, _ := p.runtime.identity(providerchannels.CapabilityRepurpose, p.runtime.legacyPlanner)
	return name
}

func (p *channelPlanner) Model() string {
	_, model := p.runtime.identity(providerchannels.CapabilityRepurpose, p.runtime.legacyPlanner)
	return model
}

func (p *channelPlanner) Plan(ctx context.Context, brief domain.RepurposeBrief) (domain.RepurposePlanDraft, error) {
	executor, hasRoute, err := p.runtime.executor(ctx, providerchannels.CapabilityRepurpose)
	if err != nil {
		return domain.RepurposePlanDraft{}, err
	}
	if !hasRoute {
		if p.runtime.legacyPlanner == nil {
			return domain.RepurposePlanDraft{}, ErrProviderChannelNotConfigured
		}
		return p.runtime.legacyPlanner.Plan(ctx, brief)
	}
	var draft domain.RepurposePlanDraft
	err = executor.Execute(ctx, providerchannels.CapabilityRepurpose, func(callCtx context.Context, invocation providerchannels.Invocation) error {
		key, ok, resolveErr := p.runtime.resolve(invocation.SecretRef)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			return errProviderChannelSecretMissing
		}
		provider, buildErr := p.runtime.plannerProvider(invocation, key)
		if buildErr != nil {
			return buildErr
		}
		draft, err = provider.Plan(callCtx, brief)
		if err != nil {
			return redactError(err, key)
		}
		draft = redactRepurposeDraft(draft, key)
		return nil
	})
	return draft, err
}

// executor loads the latest channel state for each provider operation. This
// makes UI changes take effect without restarting the Hub and ensures a key
// removed from the secret store cannot be selected by a stale pool.
func (r *providerChannelRuntime) executor(ctx context.Context, capability providerchannels.Capability) (*providerchannels.Executor, bool, error) {
	if r == nil || r.repo == nil || r.secrets == nil {
		return nil, false, nil
	}
	rows, err := r.repo.ListProviderChannels(ctx, string(capability))
	if err != nil {
		return nil, false, err
	}
	channels := make([]providerchannels.Channel, 0, len(rows))
	fingerprintRows := make([]providerChannelFingerprintChannel, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled || strings.TrimSpace(row.Capability) != string(capability) || !supportedChannelProvider(capability, row.ProviderName) {
			continue
		}
		channel := providerchannels.Channel{
			ID: row.ID, Capability: capability, Capabilities: []providerchannels.Capability{capability},
			Label: row.Label, Provider: row.ProviderName, ProviderName: row.ProviderName,
			Protocol: row.Protocol, Endpoint: row.Endpoint, Model: row.Model,
			Enabled: row.Enabled, RouteOrder: row.RouteOrder,
		}
		fingerprintChannel := providerChannelFingerprintChannel{
			ID: row.ID, Capability: row.Capability, Label: row.Label, ProviderName: row.ProviderName,
			Protocol: row.Protocol, Endpoint: row.Endpoint, Model: row.Model, Enabled: row.Enabled,
			RouteOrder: row.RouteOrder,
		}
		for _, member := range row.Members {
			if strings.TrimSpace(member.SecretRef) == "" {
				continue
			}
			ready, hasErr := r.secrets.Has(ctx, member.SecretRef)
			if hasErr != nil {
				return nil, false, hasErr
			}
			weight, maxInflight := member.Weight, member.MaxInflight
			if weight <= 0 {
				weight = 1
			}
			if maxInflight <= 0 {
				maxInflight = 1
			}
			fingerprintChannel.Members = append(fingerprintChannel.Members, providerChannelFingerprintMember{
				ID: member.ID, ChannelID: member.ChannelID, Label: member.Label, SecretRef: member.SecretRef,
				Enabled: member.Enabled, Weight: weight, MaxInflight: maxInflight, SecretReady: ready,
			})
			if !member.Enabled || !ready {
				continue
			}
			channel.Members = append(channel.Members, providerchannels.Member{
				ID: member.ID, ChannelID: row.ID, Provider: row.ProviderName, ProviderName: row.ProviderName,
				Label: member.Label, SecretRef: member.SecretRef, SecretConfigured: true, Enabled: true,
				Weight: weight, MaxInflight: maxInflight, Capabilities: []providerchannels.Capability{capability},
			})
		}
		fingerprintRows = append(fingerprintRows, fingerprintChannel)
		if len(channel.Members) > 0 {
			channels = append(channels, channel)
		}
	}
	fingerprint := providerChannelRouteFingerprint(capability, fingerprintRows)
	r.cacheMu.Lock()
	if cached, ok := r.executors[capability]; ok && cached.fingerprint == fingerprint {
		r.cacheMu.Unlock()
		return cached.executor, cached.hasRoute, nil
	}
	if len(channels) == 0 {
		r.executors[capability] = cachedProviderExecutor{fingerprint: fingerprint, hasRoute: false}
		r.cacheMu.Unlock()
		return nil, false, nil
	}
	executor, err := providerchannels.NewExecutor(channels)
	if err != nil {
		r.cacheMu.Unlock()
		return nil, false, err
	}
	r.executors[capability] = cachedProviderExecutor{fingerprint: fingerprint, executor: executor, hasRoute: true}
	r.cacheMu.Unlock()
	return executor, true, nil
}

// runtimeCapabilities enumerates every capability providerChannelRuntime can
// actually route through an Executor -- the same set supportedChannelProvider
// recognizes. CapabilityAlignment is deliberately absent: Service wires
// alignment straight to the legacy providers.Alignment provider (there is no
// channelRuntime.alignment() method, and supportedChannelProvider has no case
// for it), so a channel row saved with capability "alignment" could never be
// built into an Executor here, and capabilityStatuses must not claim
// otherwise by listing it.
var runtimeCapabilities = []providerchannels.Capability{
	providerchannels.CapabilityASR,
	providerchannels.CapabilityVideoAnalysis,
	providerchannels.CapabilityTagCurator,
	providerchannels.CapabilityEmbedding,
	providerchannels.CapabilityRepurpose,
}

// ProviderChannelCapabilityStatus is the secret-free runtime view of one
// capability's provider route. HasRuntimeData is false when no Executor has
// ever been built for this capability -- nothing has routed through it since
// the Hub started or since its channels were last edited (executor's
// fingerprint cache, above, is what "built" means here) -- and Snapshot is
// nil in that case. A caller must check HasRuntimeData before reading
// Snapshot: there is a real difference between "every key is healthy" and
// "nothing has been asked of this route yet", and collapsing the second into
// the first is exactly the gap that let a fully retired channel keep
// reporting green while every job on it quietly parked.
type ProviderChannelCapabilityStatus struct {
	Capability     providerchannels.Capability      `json:"capability"`
	HasRuntimeData bool                             `json:"has_runtime_data"`
	HasRoute       bool                             `json:"has_route"`
	Snapshot       *providerchannels.StatusSnapshot `json:"snapshot,omitempty"`
}

// capabilityStatuses reports the runtime state of every capability
// providerChannelRuntime can route, reading the executor cache exactly as it
// stands. It deliberately never calls executor(): that call builds and caches
// an Executor on demand, and building one here -- inside what is supposed to
// be a read -- would let a status request fabricate the very "has this route
// been used" signal HasRuntimeData exists to report honestly. A fresh
// Executor's Snapshot has zero attempts and zero retirements on every member,
// which Snapshot's own Available logic reads as healthy; querying status
// immediately after a Hub restart must come back saying "no data yet" for
// every capability, not "all green" because nothing has failed only because
// nothing has been tried.
func (r *providerChannelRuntime) capabilityStatuses() []ProviderChannelCapabilityStatus {
	statuses := make([]ProviderChannelCapabilityStatus, 0, len(runtimeCapabilities))
	if r == nil {
		for _, capability := range runtimeCapabilities {
			statuses = append(statuses, ProviderChannelCapabilityStatus{Capability: capability})
		}
		return statuses
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	for _, capability := range runtimeCapabilities {
		cached, ok := r.executors[capability]
		if !ok {
			statuses = append(statuses, ProviderChannelCapabilityStatus{Capability: capability})
			continue
		}
		status := ProviderChannelCapabilityStatus{Capability: capability, HasRuntimeData: true, HasRoute: cached.hasRoute}
		if cached.hasRoute && cached.executor != nil {
			snapshot := cached.executor.Snapshot(capability)
			status.Snapshot = &snapshot
		}
		statuses = append(statuses, status)
	}
	return statuses
}

type providerChannelFingerprintChannel struct {
	ID           string                             `json:"id"`
	Capability   string                             `json:"capability"`
	Label        string                             `json:"label"`
	ProviderName string                             `json:"provider_name"`
	Protocol     string                             `json:"protocol,omitempty"`
	Endpoint     string                             `json:"endpoint,omitempty"`
	Model        string                             `json:"model,omitempty"`
	Enabled      bool                               `json:"enabled"`
	RouteOrder   int                                `json:"route_order"`
	Members      []providerChannelFingerprintMember `json:"members,omitempty"`
}

type providerChannelFingerprintMember struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channel_id"`
	Label       string `json:"label"`
	SecretRef   string `json:"secret_ref"`
	Enabled     bool   `json:"enabled"`
	Weight      int    `json:"weight"`
	MaxInflight int    `json:"max_inflight"`
	SecretReady bool   `json:"secret_ready"`
}

func providerChannelRouteFingerprint(capability providerchannels.Capability, rows []providerChannelFingerprintChannel) string {
	payload, _ := json.Marshal(struct {
		Capability providerchannels.Capability         `json:"capability"`
		Channels   []providerChannelFingerprintChannel `json:"channels"`
	}{Capability: capability, Channels: rows})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func (r *providerChannelRuntime) resolve(ref string) (string, bool, error) {
	if strings.TrimSpace(ref) == "" {
		return "", false, fmt.Errorf("provider channel secret reference is empty")
	}
	value, ok, err := r.secrets.Resolve(ref)
	if err != nil {
		return "", false, fmt.Errorf("resolve provider channel secret: %w", err)
	}
	return value, ok, nil
}

// identity resolves the (name, model) pair for a capability, caching the
// result so that Name() and Model() always report a consistent identity even
// when the underlying executor resolution times out.
//
// The entire resolution (SQLite query, secret-store checks, executor build)
// is wrapped inside a goroutine + 2 s select so that a stalled DB or blocked
// mutex never holds a UI render or status endpoint indefinitely.  When the
// deadline elapses before the goroutine completes, identity returns the
// fallback provider's Name/Model pair, or a stable non-empty sentinel
// ("provider_channel" / "provider_channel") when no fallback exists — never
// an empty string that would destabilise hash chains.
//
// The cache key includes the fallback provider name so that primary and
// fallback instances of the same capability (e.g. ASR) are never served
// from the same entry.  Each cached entry also carries the executor route
// fingerprint at resolution time; a mismatch on the next lookup invalidates
// the entry before the TTL elapses, so provider-channel edits take effect
// without a Hub restart.
//
// Fast-path fingerprint re-validation: the fast path calls executor() with a
// 1 s timeout to re-read the DB and compute a fresh fingerprint, so that an
// identity cached before a channel edit is invalidated even when no other
// executor operation has rebuilt the executor cache.  When the re-validation
// fails or times out the cached identity is still returned — the slow path
// on the next lookup will correct it.
//
// Resource governance: resolveIdentity receives a context with a 10 s
// deadline so that its goroutine cannot accumulate indefinitely behind a
// stalled database or blocked mutex.  The 2 s UI deadline is enforced by
// the outer select; when it fires the goroutine is NOT cancelled — it
// continues to completion under its own 10 s deadline, caches its result,
// and exits normally.  The resolveCancel is owned by the goroutine, not by
// the caller, so the caller returning early never kills background work.
func (r *providerChannelRuntime) identity(capability providerchannels.Capability, fallback interface {
	Name() string
	Model() string
}) (string, string) {
	fallbackName := ""
	if fallback != nil {
		fallbackName = fallback.Name()
	}
	cacheKey := string(capability) + "\x00" + fallbackName

	// Fast path: consistent cached pair whose executor fingerprint still
	// matches.  The fingerprint is re-validated against a fresh DB read
	// (short timeout) so that channel edits are visible even when no
	// executor rebuild has been triggered by another operation.
	r.identityCacheMu.Lock()
	cached, cacheHit := r.identityCache[cacheKey]
	r.identityCacheMu.Unlock()

	if cacheHit && time.Now().Before(cached.expiresAt) {
		// Re-read DB to compute a fresh fingerprint for the cached
		// entry.  A 1 s timeout keeps this fast; on failure or timeout
		// the cached identity is returned (the next call will retry).
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 1*time.Second)
		_, freshHasRoute, freshErr := r.executor(checkCtx, capability)
		checkCancel()
		if freshErr == nil && freshHasRoute {
			r.cacheMu.Lock()
			execCached, execOk := r.executors[capability]
			r.cacheMu.Unlock()
			if execOk && execCached.fingerprint == cached.fingerprint {
				return cached.name, cached.model
			}
		} else {
			// Re-validation failed or timed out; compare against the
			// in-memory executor cache as a fallback (the slow path
			// will correct on the next call if the fingerprint actually
			// changed).
			r.cacheMu.Lock()
			execCached, execOk := r.executors[capability]
			r.cacheMu.Unlock()
			if !execOk || execCached.fingerprint == cached.fingerprint {
				return cached.name, cached.model
			}
		}
		// Fingerprint mismatch: channel config changed since last
		// resolution.  Fall through to re-resolve.
	}

	// Slow path: resolve with overall timeout.
	type pair struct {
		name  string
		model string
	}
	resultCh := make(chan pair, 1)

	// The goroutine owns its context and its cancellation.  The 10 s
	// deadline bounds the goroutine lifetime even when the caller has
	// already returned (the 2 s select below fired first).  executor()
	// passes ctx to ListProviderChannels so the SQLite query respects
	// cancellation; the secret-store check is also context-aware.
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 10*time.Second)

	go func() {
		defer resolveCancel()
		n, m := r.resolveIdentity(resolveCtx, capability)
		// NOTE: resultCh is buffered (cap 1), so this send never blocks and the
		// default branch below is unreachable once the caller has moved on — the
		// result is simply discarded and the next caller re-runs the slow path.
		// Keep the buffer so the common (non-timeout) case never blocks the
		// goroutine; do not "optimize" this into a cache write without a real
		// lock-free single-flight design.
		select {
		case resultCh <- pair{n, m}:
		default:
			if n != "" {
				r.cacheIdentity(capability, fallbackName, n, m)
			}
		}
	}()

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	select {
	case <-timeoutCtx.Done():
		return r.cacheFallbackIdentity(capability, fallbackName, fallback)
	case res := <-resultCh:
		if res.name != "" {
			r.cacheIdentity(capability, fallbackName, res.name, res.model)
			return res.name, res.model
		}
		return r.cacheFallbackIdentity(capability, fallbackName, fallback)
	}
}

// resolveIdentity performs the actual channel-executor resolution.  It
// accepts a context so that the caller (identity) can bound its lifetime and
// prevent goroutine accumulation behind a stalled database or blocked mutex.
func (r *providerChannelRuntime) resolveIdentity(ctx context.Context, capability providerchannels.Capability) (string, string) {
	executor, hasRoute, err := r.executor(ctx, capability)
	if err == nil && hasRoute {
		routes := executor.Route(capability)
		// Require both name and model non-empty so a partially configured
		// channel (e.g. provider name set but model field blank) does not
		// bypass the sentinel pair that cacheFallbackIdentity enforces.
		if len(routes) > 0 && routes[0].ProviderName != "" && routes[0].Model != "" {
			return routes[0].ProviderName, routes[0].Model
		}
	}
	return "", ""
}

// cacheIdentity stores a resolved (name, model) pair in the identity cache
// together with the executor route fingerprint at resolution time.
//
// Lock order: identityCacheMu → cacheMu, consistent with the fast path in
// identity().  The reverse order (cacheMu → identityCacheMu) would deadlock
// when identity()'s fast path and cacheIdentity() run concurrently.
func (r *providerChannelRuntime) cacheIdentity(capability providerchannels.Capability, fallbackName, name, model string) {
	cacheKey := string(capability) + "\x00" + fallbackName

	// Lock identityCacheMu first (consistent with identity's fast path), then
	// cacheMu to read the executor fingerprint.
	r.identityCacheMu.Lock()
	r.cacheMu.Lock()
	fingerprint := ""
	if cached, ok := r.executors[capability]; ok {
		fingerprint = cached.fingerprint
	}
	r.identityCache[cacheKey] = cachedIdentity{
		name: name, model: model, fingerprint: fingerprint,
		expiresAt: time.Now().Add(identityCacheTTL),
	}
	r.cacheMu.Unlock()
	r.identityCacheMu.Unlock()
}

// cacheFallbackIdentity returns the fallback provider's identity or, when no
// fallback exists, a stable non-empty sentinel so that hash chains and model
// caches never see an empty string.  The result is cached with the standard
// TTL so that subsequent Name() / Model() calls within the window are
// consistent.
//
// When the fallback provider exists, both Name() and Model() are read and
// validated as a pair: if either is empty the pair is replaced with the
// "provider_channel" sentinel so that no caller ever receives a mixed
// identity (e.g. one empty, one from legacy config) that would destabilise
// downstream caches.
func (r *providerChannelRuntime) cacheFallbackIdentity(capability providerchannels.Capability, fallbackName string, fallback interface {
	Name() string
	Model() string
}) (string, string) {
	var name, model string
	if fallback != nil {
		name = fallback.Name()
		model = fallback.Model()
	}
	if name == "" || model == "" {
		name, model = "provider_channel", "provider_channel"
	}
	r.cacheIdentity(capability, fallbackName, name, model)
	return name, model
}

// The five builders below turn a channel invocation into a provider
// implementation, and they are the point at which a route stops being
// configuration and becomes something callable. Nothing in them touches the
// network: every failure they can produce is an unknown provider name, an
// unknown protocol, or a provider an operator switched off, all of which will
// be exactly as wrong on the next attempt. Each is therefore a thin marking
// wrapper over the real body, so that the edit these functions actually
// attract — adding a `case` for a new provider — inherits the verdict rather
// than needing someone to remember it somewhere else. See
// domain.ErrPermanentFailure.
func (r *providerChannelRuntime) asrProvider(invocation providerchannels.Invocation, key string) (providers.ASR, error) {
	provider, err := r.buildASRProvider(invocation, key)
	if err != nil {
		return nil, domain.Permanent(err)
	}
	return provider, nil
}

func (r *providerChannelRuntime) buildASRProvider(invocation providerchannels.Invocation, key string) (providers.ASR, error) {
	providersConfig := r.cfg.Providers
	clearProviderSecrets(&providersConfig)
	name := strings.TrimSpace(invocation.ProviderName)
	switch name {
	case "stepfun":
		applyProviderChannelConfig(&providersConfig.StepFun, invocation, key)
	case "qwen":
		applyProviderChannelConfig(&providersConfig.Qwen, invocation, key)
	case "volcengine_asr":
		providersConfig.VolcASR.Enabled = true
		providersConfig.VolcASR.APIKey = key
		providersConfig.VolcASR.APIKeyEnv = ""
		if invocation.Endpoint != "" {
			providersConfig.VolcASR.URL = invocation.Endpoint
		}
		if invocation.Model != "" {
			providersConfig.VolcASR.Model = invocation.Model
		}
	default:
		return nil, fmt.Errorf("unsupported ASR provider channel %q", name)
	}
	provider, err := providers.NewASR(name, providersConfig)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("ASR provider channel %q is disabled", name)
	}
	return provider, nil
}

func (r *providerChannelRuntime) videoProvider(invocation providerchannels.Invocation, key string) (videoproviders.VideoUnderstandingProvider, error) {
	provider, err := r.buildVideoProvider(invocation, key)
	if err != nil {
		return nil, domain.Permanent(err)
	}
	return provider, nil
}

func (r *providerChannelRuntime) buildVideoProvider(invocation providerchannels.Invocation, key string) (videoproviders.VideoUnderstandingProvider, error) {
	// Fail fast on the protocol that channel routing cannot orchestrate yet
	// (see errProviderChannelMultiframeUnsupported): building the provider
	// here would succeed and then die on the first frame-less call.
	if invocation.Protocol == providers.ProtocolOpenAIMultiframe {
		return nil, errProviderChannelMultiframeUnsupported
	}
	providersConfig := r.cfg.Providers
	clearProviderSecrets(&providersConfig)
	name := strings.TrimSpace(invocation.ProviderName)
	switch name {
	case "gemini":
		applyProviderChannelConfig(&providersConfig.Gemini, invocation, key)
	case "qwen_video":
		applyProviderChannelConfig(&providersConfig.QwenVideo, invocation, key)
	case "volcengine_video":
		applyProviderChannelConfig(&providersConfig.VolcVideo, invocation, key)
	case "local_vlm":
		applyProviderChannelConfig(&providersConfig.LocalVLM, invocation, key)
	default:
		return nil, fmt.Errorf("unsupported video provider channel %q", name)
	}
	provider, err := providers.NewVideoUnderstandingProvider(name, nil, providersConfig)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("video provider channel %q is disabled", name)
	}
	return provider, nil
}

func (r *providerChannelRuntime) tagCuratorProvider(invocation providerchannels.Invocation, key string) (providers.TagCurator, error) {
	provider, err := r.buildTagCuratorProvider(invocation, key)
	if err != nil {
		return nil, domain.Permanent(err)
	}
	return provider, nil
}

func (r *providerChannelRuntime) buildTagCuratorProvider(invocation providerchannels.Invocation, key string) (providers.TagCurator, error) {
	providersConfig := r.cfg.Providers
	clearProviderSecrets(&providersConfig)
	name := strings.TrimSpace(invocation.ProviderName)
	switch name {
	case "openai_chat":
		applyProviderChannelConfig(&providersConfig.TagCurator, invocation, key)
		if invocation.Protocol == "" {
			providersConfig.TagCurator.Protocol = "openai_chat"
		}
	case "volc_agent_plan":
		applyProviderChannelConfig(&providersConfig.VolcAgentPlan, invocation, key)
		providersConfig.VolcAgentPlan.Protocol = "openai_chat"
	case "volc_coding_plan":
		applyProviderChannelConfig(&providersConfig.VolcCodingPlan, invocation, key)
		providersConfig.VolcCodingPlan.Protocol = "openai_chat"
	default:
		return nil, fmt.Errorf("unsupported tag curator provider channel %q", name)
	}
	provider, err := providers.NewTagCurator(name, providersConfig)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("tag curator provider channel %q is disabled", name)
	}
	return provider, nil
}

func (r *providerChannelRuntime) embedderProvider(invocation providerchannels.Invocation, key string) (providers.Embedder, error) {
	provider, err := r.buildEmbedderProvider(invocation, key)
	if err != nil {
		return nil, domain.Permanent(err)
	}
	return provider, nil
}

func (r *providerChannelRuntime) buildEmbedderProvider(invocation providerchannels.Invocation, key string) (providers.Embedder, error) {
	providersConfig := r.cfg.Providers
	clearProviderSecrets(&providersConfig)
	name := strings.TrimSpace(invocation.ProviderName)
	switch name {
	case "openai_embeddings", "gemini_embed_content":
		applyProviderChannelConfig(&providersConfig.Embedding, invocation, key)
		if invocation.Protocol == "" {
			providersConfig.Embedding.Protocol = name
		}
	case "volc_agent_plan_embedding":
		applyProviderChannelConfig(&providersConfig.VolcAgentPlanEmbedding, invocation, key)
		providersConfig.VolcAgentPlanEmbedding.Protocol = "openai_embeddings"
	case "volc_coding_plan_embedding":
		applyProviderChannelConfig(&providersConfig.VolcCodingPlanEmbedding, invocation, key)
		providersConfig.VolcCodingPlanEmbedding.Protocol = "openai_embeddings"
	default:
		return nil, fmt.Errorf("unsupported embedding provider channel %q", name)
	}
	provider, err := providers.NewEmbedder(name, providersConfig)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("embedding provider channel %q is disabled", name)
	}
	return provider, nil
}

func (r *providerChannelRuntime) plannerProvider(invocation providerchannels.Invocation, key string) (providers.RepurposePlanner, error) {
	provider, err := r.buildPlannerProvider(invocation, key)
	if err != nil {
		return nil, domain.Permanent(err)
	}
	return provider, nil
}

func (r *providerChannelRuntime) buildPlannerProvider(invocation providerchannels.Invocation, key string) (providers.RepurposePlanner, error) {
	providersConfig := r.cfg.Providers
	clearProviderSecrets(&providersConfig)
	name := strings.TrimSpace(invocation.ProviderName)
	if name != "openai_chat" {
		return nil, fmt.Errorf("unsupported repurpose provider channel %q", name)
	}
	applyProviderChannelConfig(&providersConfig.Repurpose, invocation, key)
	if invocation.Protocol == "" {
		providersConfig.Repurpose.Protocol = "openai_chat"
	}
	provider, err := providers.NewRepurposePlanner(name, providersConfig)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("repurpose provider channel %q is disabled", name)
	}
	return provider, nil
}

func applyProviderChannelConfig(target *config.ProviderConfig, invocation providerchannels.Invocation, key string) {
	target.Enabled = true
	target.APIKey = key
	target.APIKeyEnv = ""
	if invocation.Protocol != "" {
		target.Protocol = invocation.Protocol
	}
	if invocation.Endpoint != "" {
		target.BaseURL = invocation.Endpoint
	}
	if invocation.Model != "" {
		target.Model = invocation.Model
	}
	if invocation.Path != "" {
		target.Path = invocation.Path
	}
	if invocation.AuthHeader != "" {
		target.AuthHeader = invocation.AuthHeader
	}
	if invocation.AuthScheme != "" {
		target.AuthScheme = invocation.AuthScheme
	}
	if invocation.TimeoutSeconds > 0 {
		target.TimeoutSeconds = invocation.TimeoutSeconds
	}
}

func clearProviderSecrets(c *config.ProvidersConfig) {
	for _, target := range []*config.ProviderConfig{
		&c.StepFun, &c.Qwen, &c.Gemini, &c.QwenVideo, &c.VolcVideo, &c.LocalVLM,
		&c.TagCurator, &c.Embedding, &c.Repurpose, &c.VolcAgentPlan, &c.VolcCodingPlan,
		&c.VolcAgentPlanEmbedding, &c.VolcCodingPlanEmbedding,
	} {
		target.APIKey = ""
		target.APIKeyEnv = ""
	}
	c.VolcASR.APIKey = ""
	c.VolcASR.APIKeyEnv = ""
}

func supportedChannelProvider(capability providerchannels.Capability, name string) bool {
	switch capability {
	case providerchannels.CapabilityASR:
		return name == "stepfun" || name == "qwen" || name == "volcengine_asr"
	case providerchannels.CapabilityVideoAnalysis:
		return name == "gemini" || name == "qwen_video" || name == "volcengine_video" || name == "local_vlm"
	case providerchannels.CapabilityTagCurator:
		return name == "openai_chat" || name == "volc_agent_plan" || name == "volc_coding_plan"
	case providerchannels.CapabilityEmbedding:
		return name == "openai_embeddings" || name == "gemini_embed_content" || name == "volc_agent_plan_embedding" || name == "volc_coding_plan_embedding"
	case providerchannels.CapabilityRepurpose:
		return name == "openai_chat"
	default:
		return false
	}
}

// redactedError carries a redacted message plus (optionally) a redacted copy
// of a *common.StatusError found in the original chain. The provider key must
// not survive anywhere in the returned chain, including inside a wrapped
// *common.StatusError whose Body is the upstream response -- a relay may echo
func redactError(err error, secret string) error { return common.RedactError(err, secret) }

func redactString(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}

func redactTranscript(transcript domain.Transcript, secret string) domain.Transcript {
	transcript.Text = redactString(transcript.Text, secret)
	transcript.RawResponse = redactString(transcript.RawResponse, secret)
	for i := range transcript.Segments {
		transcript.Segments[i].Text = redactString(transcript.Segments[i].Text, secret)
	}
	return transcript
}

func redactVideoResult(result videoanalysis.Result, secret string) videoanalysis.Result {
	result.Summary = redactString(result.Summary, secret)
	result.RawTags = redactStrings(result.RawTags, secret)
	result.Mood = redactStrings(result.Mood, secret)
	result.Analysis.Summary = redactString(result.Analysis.Summary, secret)
	result.Analysis.SceneTags = redactStrings(result.Analysis.SceneTags, secret)
	result.Analysis.Subjects = redactStrings(result.Analysis.Subjects, secret)
	result.Analysis.UsableAs = redactStrings(result.Analysis.UsableAs, secret)
	result.Analysis.MoodTags = redactStrings(result.Analysis.MoodTags, secret)
	result.Analysis.QualityFlags = redactStrings(result.Analysis.QualityFlags, secret)
	result.Analysis.ExtraTags = redactStrings(result.Analysis.ExtraTags, secret)
	result.Analysis.EditorialReason = redactString(result.Analysis.EditorialReason, secret)
	for i := range result.Scenes {
		result.Scenes[i].Description = redactString(result.Scenes[i].Description, secret)
		result.Scenes[i].Tags = redactStrings(result.Scenes[i].Tags, secret)
	}
	for i := range result.Objects {
		result.Objects[i].Name = redactString(result.Objects[i].Name, secret)
	}
	for i := range result.Actions {
		result.Actions[i].Name = redactString(result.Actions[i].Name, secret)
	}
	for i := range result.Shots {
		result.Shots[i].Description = redactString(result.Shots[i].Description, secret)
		result.Shots[i].Tags = redactStrings(result.Shots[i].Tags, secret)
		result.Shots[i].Objects = redactStrings(result.Shots[i].Objects, secret)
		result.Shots[i].Actions = redactStrings(result.Shots[i].Actions, secret)
		result.Shots[i].Mood = redactStrings(result.Shots[i].Mood, secret)
	}
	return result
}

func redactStrings(values []string, secret string) []string {
	if len(values) == 0 || secret == "" {
		return values
	}
	result := append([]string(nil), values...)
	for i := range result {
		result[i] = redactString(result[i], secret)
	}
	return result
}

func redactTagProposals(proposals []domain.TagProposal, secret string) []domain.TagProposal {
	if secret == "" || len(proposals) == 0 {
		return proposals
	}
	result := append([]domain.TagProposal(nil), proposals...)
	for i := range result {
		result[i].CanonicalName = redactString(result[i].CanonicalName, secret)
		result[i].Reason = redactString(result[i].Reason, secret)
		result[i].Payload = redactMap(result[i].Payload, secret)
	}
	return result
}

func redactLibrarySummary(draft domain.LibrarySummaryDraft, secret string) domain.LibrarySummaryDraft {
	draft.Summary = redactString(draft.Summary, secret)
	draft.Themes = redactStrings(draft.Themes, secret)
	draft.SuitableFor = redactStrings(draft.SuitableFor, secret)
	return draft
}

func redactRepurposeDraft(draft domain.RepurposePlanDraft, secret string) domain.RepurposePlanDraft {
	draft.Title = redactString(draft.Title, secret)
	for i := range draft.Sections {
		draft.Sections[i].Role = redactString(draft.Sections[i].Role, secret)
		draft.Sections[i].Query = redactString(draft.Sections[i].Query, secret)
		draft.Sections[i].Rationale = redactString(draft.Sections[i].Rationale, secret)
	}
	return draft
}

func redactMap(values map[string]any, secret string) map[string]any {
	if values == nil || secret == "" {
		return values
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = redactAny(value, secret)
	}
	return result
}

func redactAny(value any, secret string) any {
	switch item := value.(type) {
	case string:
		return redactString(item, secret)
	case map[string]any:
		return redactMap(item, secret)
	case []any:
		result := make([]any, len(item))
		for i := range item {
			result[i] = redactAny(item[i], secret)
		}
		return result
	default:
		return value
	}
}
