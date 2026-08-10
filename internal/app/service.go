package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/credentials"
	"github.com/evjohn-icu/timingdex/internal/curator"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/hubauth"
	"github.com/evjohn-icu/timingdex/internal/idgen"
	"github.com/evjohn-icu/timingdex/internal/ingest"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/mount"
	"github.com/evjohn-icu/timingdex/internal/providers"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	"github.com/evjohn-icu/timingdex/internal/remote"
	sqlite "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/repurpose"
	"github.com/evjohn-icu/timingdex/internal/search"
	"github.com/evjohn-icu/timingdex/internal/secretstore"
	"github.com/evjohn-icu/timingdex/internal/staging"
	"github.com/evjohn-icu/timingdex/internal/webdavspace"
)

var ErrInvalidRepurposeRevision = errors.New("invalid repurpose revision")

// The human-approval boundary's sentinels are app-facing names for the domain
// sentinels the repository wraps at the point it enforces them; see
// internal/domain/errors.go for what each condition means and why the
// identities live there rather than here. These are aliases, not separate
// errors.New values, so errors.Is(err, app.ErrPlanImmutable) and
// errors.Is(err, domain.ErrPlanImmutable) are the same question asked twice
// and cannot come back with different answers -- the trap 27ac022 closed the
// last time one condition carried two names in two layers.
var (
	ErrPlanImmutable         = domain.ErrPlanImmutable
	ErrPlanRevisionNotFound  = domain.ErrPlanRevisionNotFound
	ErrPlanRevisionNotDraft  = domain.ErrPlanRevisionNotDraft
	ErrPlanRevisionNotLatest = domain.ErrPlanRevisionNotLatest
)

var ErrInvalidWorkerArtifact = errors.New("invalid worker artifact")

// ErrWorkerArtifactLease is UploadWorkerArtifact's API-facing rename of
// domain.ErrJobLeaseLost: writeWorkerArtifactResult (internal/api/server.go)
// matches this with errors.Is to answer 409 specifically for an artifact
// upload, rather than exposing the repository's generic lease sentinel at
// the handler. It used to be produced by matching the repository error's
// text for "worker does not own active job" -- the same prose the repository
// happened to use for the condition, so the two stayed in sync by
// coincidence, not by anything the compiler checked. The repository now
// wraps domain.ErrJobLeaseLost instead, so this wraps that with errors.Is;
// see its doc comment for why one sentinel covers both the Hub-local and
// Worker-facing halves of the same condition.
var ErrWorkerArtifactLease = errors.New("worker does not own active job")
var ErrWorkerProviderCredentialDeliveryDisabled = errors.New("worker provider credential delivery is disabled")

// ErrWorkerProviderNotConfigured is returned when a Worker asks for an
// operation that has no provider available to it by any means this Hub
// knows: no providers.* block, and (per
// ErrWorkerProviderConfiguredAsChannelOnly's doc comment) no provider
// channel either. This is the "there is genuinely nothing here" case; an
// operator response is to configure the provider, by either method.
var ErrWorkerProviderNotConfigured = errors.New("worker provider is not configured")

// ErrWorkerProviderConfiguredAsChannelOnly is returned when
// credentials.Broker fails to resolve a provider for a Worker operation but a
// provider channel exists for that capability. Worker credential and proxy
// issuance (IssueWorkerCredential, issueProviderCredentialForProxy) build
// their broker from s.cfg.Providers only, deliberately: CLAUDE.md's Worker
// trust boundary keeps a Worker's provider access to the opt-in
// direct-credential path or the Hub-side JSON proxy, never to
// internal/providerchannels's channel-scoped keys, member pools and health
// state -- pushing those to a remote node is exactly what the direct-credential
// path being default-deny is protecting against. Before this sentinel
// existed, that refusal looked identical to "you have not configured this
// provider at all" even though the operator had configured it correctly
// through the channels UI; classifyWorkerProviderBrokerErr distinguishes the
// two by checking whether a channel row exists for the operation, not by
// re-deriving anything from the channel other than its presence.
var ErrWorkerProviderConfiguredAsChannelOnly = errors.New("worker provider access reads only providers.* config; this capability is configured as a provider channel instead")

type Repository interface {
	PipelineRepository
	IntegrityCheck(ctx context.Context) error
	// HealStaleRunningJobs releases 'running' jobs whose lease expired (a
	// crash artifact, not work in flight) back to 'pending', and terminally
	// fails the ones that exhausted their attempts. HealOnStartup is its only
	// caller; it lives on the full interface rather than PipelineRepository
	// because it is a startup concern, not a pipeline-run one, and the narrow
	// interface's fakes should not have to grow it.
	HealStaleRunningJobs(context.Context, time.Time) (int, int, error)
	CreateLibraryRoot(ctx context.Context, path string) (domain.LibraryRoot, error)
	ListLibraryRoots(ctx context.Context) ([]domain.LibraryRoot, error)
	GetLibraryRoot(ctx context.Context, id string) (domain.LibraryRoot, error)
	IsLibraryRoot(ctx context.Context, path string) (bool, error)
	// MarkRootScanStarted, MarkRootHealthy and MarkRootUnavailable persist the
	// scan-time health ledger. ScanLibraryRoot writes them; nothing else does,
	// and the reconciliation gate in ScanLibraryRoot is the only consumer of
	// what they record.
	MarkRootScanStarted(ctx context.Context, rootID string, at time.Time) error
	MarkRootHealthy(ctx context.Context, rootID string, at time.Time) error
	MarkRootUnavailable(ctx context.Context, rootID string, at time.Time) error
	// MarkUnseenLocationsMissing lives here rather than on ingest.ScanRepository
	// because the scanner no longer reconciles: it reports the seen list and
	// the service calls this only after the root-health gate passes.
	MarkUnseenLocationsMissing(ctx context.Context, rootID string, seenRelativePaths []string) (int, error)
	ingest.ScanRepository
	AssetsWithoutProbeJob(ctx context.Context, rootID string, limit int) ([]string, error)
	ListAssets(ctx context.Context, limit, offset int) ([]domain.Asset, error)
	TotalSourceBytes(ctx context.Context) (int64, error)
	ListAssetCards(ctx context.Context, limit, offset int) ([]domain.AssetCard, error)
	ListAssetCardsFiltered(ctx context.Context, filter domain.AssetCardFilter) ([]domain.AssetCard, error)
	SaveAssetCollection(ctx context.Context, collection domain.AssetCollection) (domain.AssetCollection, error)
	ListAssetCollections(ctx context.Context) ([]domain.CollectionSummary, error)
	GetAssetCollection(ctx context.Context, id string) (*domain.CollectionSummary, error)
	DeleteAssetCollection(ctx context.Context, id string) error
	// The shot-basket methods are deliberately on the full Repository, not on
	// PipelineRepository: the pipeline has no reason to read or mutate a
	// collection's pins, and the narrow interface's fakes should not have to
	// grow them.
	AddShotToCollection(ctx context.Context, collectionID, shotID string) error
	RemoveShotFromCollection(ctx context.Context, collectionID, shotID string) error
	ListCollectionShots(ctx context.Context, collectionID string) ([]domain.CollectionShotDetail, error)
	ReorderCollectionShots(ctx context.Context, collectionID string, shotIDs []string) error
	ListAssetCardsInCollection(ctx context.Context, collectionID string, limit, offset int) ([]domain.AssetCard, error)
	GetAssetProcessingSummary(ctx context.Context, filter domain.AssetCollectionFilter) (domain.AssetProcessingSummary, error)
	ListShootSessions(ctx context.Context, filter domain.ShootSessionFilter) ([]domain.ShootSession, error)
	GetShootSession(ctx context.Context, id string) (*domain.ShootSession, error)
	GetAssetDetail(ctx context.Context, assetID string) (*domain.AssetDetail, error)
	GetArtifact(ctx context.Context, assetID, typ string) (*domain.DerivedArtifact, error)
	ListCanonicalTags(context.Context) ([]domain.CanonicalTag, error)
	ListUnresolvedTags(context.Context, int) ([]domain.UnresolvedTag, error)
	CreateTagCurationRun(context.Context, []domain.TagProposal, int, string) (domain.TagCurationResult, error)
	ListTagProposals(context.Context, string, int) ([]domain.TagProposal, error)
	ReviewTagProposal(context.Context, string, string, string) error
	UpsertTagEmbeddings(context.Context, string, []domain.UnresolvedTag, [][]float64) error
	CreateTagClusterRun(context.Context, string, string, float64, []domain.TagCluster, int) (string, error)
	BuildLibrarySummaryInput(context.Context) (domain.LibrarySummaryInput, error)
	SaveLibrarySummary(context.Context, domain.LibrarySummary) (domain.LibrarySummary, error)
	LatestLibrarySummary(context.Context) (*domain.LibrarySummary, error)
	JobIssues(context.Context) ([]domain.JobIssue, error)
	ListAssetShots(context.Context, string) ([]domain.AssetShot, error)
	ShotExists(context.Context, string) (bool, error)
	SearchShots(context.Context, string, int) ([]domain.ShotSearchResult, error)
	SearchShotsFiltered(context.Context, string, int, domain.FacetFilter) ([]domain.ShotSearchResult, error)
	// SearchFiltered is deliberately not on PipelineRepository: that narrow
	// interface (Search lives there, for RebuildSearch's neighbourhood) has
	// several fakes that would all have to grow a method none of them need.
	SearchFiltered(context.Context, string, int, domain.FacetFilter) ([]string, error)
	HybridSearchShots(context.Context, string, int) ([]domain.ShotSearchResult, error)
	HybridSearchShotsFiltered(context.Context, string, int, domain.FacetFilter) ([]domain.ShotSearchResult, error)
	SimilarShots(context.Context, string, int) ([]domain.ShotSearchResult, error)
	SimilarShotsFiltered(context.Context, string, int, domain.FacetFilter) ([]domain.ShotSearchResult, error)
	DiscoverRareShots(context.Context, int) ([]domain.RareShot, error)
	SaveRepurposePlan(context.Context, domain.RepurposePlan) (domain.RepurposePlan, error)
	GetRepurposePlan(context.Context, string) (*domain.RepurposePlan, error)
	SaveRepurposePlanRevision(context.Context, domain.RepurposePlan, string) (domain.RepurposePlanRevision, error)
	ListRepurposePlanRevisions(context.Context, string) ([]domain.RepurposePlanRevision, error)
	ApproveRepurposePlanRevision(context.Context, string, int) (domain.RepurposePlanRevision, error)
	UpsertProviderChannel(context.Context, domain.ProviderChannel) (domain.ProviderChannel, error)
	ListProviderChannels(context.Context, string) ([]domain.ProviderChannel, error)
	SoftDeleteProviderChannel(context.Context, string) error
	RecordCostEstimate(context.Context, domain.CostEntry) error
	CostEstimateForDay(context.Context, string) (float64, error)
	CostEstimateForMonth(context.Context, string) (float64, error)
	RebuildAutomaticShootSessions(context.Context, string) error
	CreateWorkerPairing(context.Context, time.Duration) (remote.PairingToken, error)
	EnrollWorker(context.Context, string, remote.WorkerRegistration) (remote.Worker, string, error)
	AuthenticateWorker(context.Context, string) (remote.Worker, error)
	HeartbeatWorker(context.Context, string, string, remote.WorkerCapabilities) error
	ListWorkers(context.Context) ([]remote.Worker, error)
	LeaseNextWorkerDerive(context.Context, remote.Worker, time.Duration, domain.LeaseFilter) (*remote.WorkerJob, error)
	CompleteWorkerJob(context.Context, string, string, domain.JobState, string) error
	RecordWorkerJobProgress(context.Context, string, string, string, float64, string, string) error
	GetWorkerJobStatus(context.Context, string) (remote.WorkerJobStatus, error)
	SetDeriveWorkerAssignment(context.Context, string, string, remote.WorkerAssignmentMode) error
	PrepareWorkerArtifact(context.Context, string, string, string, string) (string, *domain.DerivedArtifact, error)
	CommitWorkerArtifact(context.Context, string, string, domain.DerivedArtifact, bool) (domain.DerivedArtifact, bool, error)
	RecordProviderCredentialLease(context.Context, string, string, string, string, time.Time) error
	SavePipelineThrottle(context.Context, domain.PipelineThrottle) error
	// SearchIndexHealth and MigrationStatus are the read-only views
	// DoctorReport renders; they live on the full Repository (not the narrow
	// pipeline interface) because nothing in the pipeline needs them.
	SearchIndexHealth(ctx context.Context) (domain.SearchIndexHealth, error)
	MigrationStatus(ctx context.Context) (domain.MigrationStatus, error)
}

type Service struct {
	repo       Repository
	cfg        config.Config
	scanner    *ingest.Scanner
	pipeline   *Pipeline
	curator    providers.TagCurator
	embedder   providers.Embedder
	planner    providers.RepurposePlanner
	hardware   media.HardwareReport
	adminToken string
	agentToken string
	secrets    *secretstore.Store

	// webdav is the on-demand WebDAV space manager for footage delivery to
	// editing agents; nil when the operator has not enabled the feature.
	webdav *webdavspace.Manager
	// webdavAccounts persists WebDAV Basic-Auth accounts (bcrypt hashes).
	webdavAccounts webdavspace.AccountStore

	// channelRuntime is the same bridge NewService hands the pipeline and
	// curator/embedder/planner wrappers, kept here too so
	// ProviderChannelRuntimeStatus can read its executor cache. It is not
	// duplicated -- both are the one instance constructed below.
	channelRuntime *providerChannelRuntime

	// searchV2 is the Search Architecture v2 engine. Wired by NewService only
	// when repo implements search.ShotStore; nil in tests and minimal setups,
	// where the legacy repository search methods remain the path. Nil-safe:
	// every method guards it.
	searchV2 *search.Service

	pipelineMu      sync.Mutex
	pipelineRunning bool

	supervisor *LibrarySupervisor

	// scanFailuresMu guards scanFailures, which tracks consecutive enqueue
	// failures per asset per root across ScanLibraryRoot passes so a single perpetually
	// failing asset does not fill the log with identical warnings on every
	// 15-minute scan.  The outer map is keyed by root ID so that concurrent
	// scans of different roots cannot delete each other's failure counters.
	scanFailuresMu sync.Mutex
	scanFailures   map[string]map[string]int

	// scanRootLocks serialize the walk and reconciliation for each root. The
	// job layer prevents cross-process duplicate scans; this in-process lock
	// prevents one scan's reconciliation from interleaving with another scan's
	// upserts. Cross-process walk/reconcile overlap remains a residual.
	scanRootLocksMu sync.Mutex
	scanRootLocks   map[string]*sync.Mutex

	// hostOverride replaces mount.LocalHost() in InspectRootPath when set. It
	// exists only so tests can exercise the mount.Host.Container branch (the
	// compose-volume suggestion below) without this test binary actually
	// running inside a container — mount.LocalHost() detects that from
	// /.dockerenv, which a test cannot fake by construction. Left unexported
	// and nil in every real Service: nothing outside this package's own tests
	// can reach it, so production InspectRootPath always reports the real
	// host.
	hostOverride *mount.Host
}

// mountHost is the mount.Host InspectRootPath generates advice for.
func (s *Service) mountHost() mount.Host {
	if s.hostOverride != nil {
		return *s.hostOverride
	}
	return mount.LocalHost()
}

func NewService(repo Repository, cfg config.Config) (*Service, error) {
	asr, err := providers.NewASR(cfg.Providers.ASRPrimary, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize primary ASR provider: %w", err)
	}
	fallback, err := providers.NewASR(cfg.Providers.ASRFallback, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize fallback ASR provider: %w", err)
	}
	videoProvider, err := providers.NewVideoUnderstandingProvider(cfg.Providers.VisionPrimary, cfg.Providers.VisionFallback, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize video understanding provider: %w", err)
	}
	alignment, err := providers.NewAlignment(cfg.Providers.AlignmentPrimary, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize alignment provider: %w", err)
	}
	shotDetector, err := providers.NewShotDetector(cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize shot detector: %w", err)
	}
	tagCurator, err := providers.NewTagCurator(cfg.Providers.TagCuratorPrimary, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize tag curator: %w", err)
	}
	embedder, err := providers.NewEmbedder(cfg.Providers.EmbeddingPrimary, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize embedding provider: %w", err)
	}
	planner, err := providers.NewRepurposePlanner(cfg.Providers.RepurposePrimary, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("initialize repurpose planner: %w", err)
	}
	sourceStager, err := staging.New(cfg.SourceStaging.Mode, cfg.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("initialize source staging: %w", err)
	}
	hardware, plan := media.DetectHardware(context.Background(), cfg.Hardware)
	adminToken, err := hubauth.EnsureAdminToken(cfg.DataDir, cfg.HubSecurity.AdminToken)
	if err != nil {
		return nil, fmt.Errorf("initialize Hub administrator token: %w", err)
	}
	// The agent token is a separate credential from the admin token: it is
	// handed to a Skill/agent so it can create and revise draft repurpose
	// plans (see requireAgentOrAdmin in internal/api) without ever holding a
	// token that can approve a plan or run the pipeline. There is no config
	// override for it yet, so explicit is always empty here.
	agentToken, err := hubauth.EnsureAgentToken(cfg.DataDir, "")
	if err != nil {
		return nil, fmt.Errorf("initialize Hub agent token: %w", err)
	}
	secrets, err := secretstore.Open(cfg.DataDir, adminToken)
	if err != nil {
		return nil, fmt.Errorf("initialize Hub provider secret store: %w", err)
	}
	channelRuntime := newProviderChannelRuntime(repo, cfg, secrets, asr, fallback, videoProvider, tagCurator, embedder, planner)
	// The pipeline sees the channel wrapper with the legacy router's
	// multiframe surface lifted on top (see pipelineVideo): channel-managed
	// video keeps routing, and the multiframe orchestration — which is
	// legacy-config-only by design — actually runs in production instead of
	// falling through to the plain path.
	videoRouter, _ := videoProvider.(*videoproviders.Router)
	pipelineVideoProvider := &pipelineVideo{channel: channelRuntime.video().(*channelVideo), router: videoRouter}
	service := &Service{
		repo: repo, cfg: cfg, scanner: ingest.NewScanner(repo),
		pipeline: NewPipeline(repo, cfg.CacheDir, channelRuntime.asr(), channelRuntime.asrFallback(), pipelineVideoProvider, alignment, shotDetector, plan, sourceStager, time.Duration(cfg.Pipeline.ProviderRouteDeferralMinutes)*time.Minute, cfg.Pipeline.MinimumFreeSpaceBytes),
		curator:  channelRuntime.curator(), embedder: channelRuntime.embedder(), planner: channelRuntime.planner(),
		hardware: hardware, adminToken: adminToken, agentToken: agentToken, secrets: secrets,
		channelRuntime: channelRuntime,
		scanFailures:   make(map[string]map[string]int),
		scanRootLocks:  make(map[string]*sync.Mutex),
	}
	// Constructed for every command, started by none of them: only `serve`
	// calls RunLibrarySupervisor, and a disabled supervisor's Run is a no-op.
	// Constructing it unconditionally keeps the status endpoint answerable
	// ("configured but not running") instead of nil.
	service.supervisor = newLibrarySupervisor(service, cfg.LibrarySupervisor)
	// The Search v2 engine is wired when the repository implements the
	// ShotStore contract (real sqlite does; test fakes do not). When it is
	// absent the legacy repository search methods keep serving — the
	// compatibility fallback.
	if store, ok := repo.(search.ShotStore); ok {
		opts := search.DefaultOptions()
		if service.embedder != nil {
			// The adapter makes "embedding not configured" a no-op channel
			// instead of a failed search (see searchEmbedderAdapter).
			opts.Embedder = searchEmbedderAdapter{embedder: service.embedder}
		}
		service.searchV2 = search.NewService(store, opts)
	}
	// The text-embedding layer (if configured) keeps its derived vectors
	// fresh after every analysis commit. The pipeline fires the hook
	// synchronously inside the job; ensureShotTextEmbeddings swallows its own
	// errors so an embedding hiccup can never fail an analysis job.
	service.pipeline.SetAfterShotsCommitted(service.ensureShotTextEmbeddings)
	// The cost ledger records an estimate after every successful analysis
	// commit; the wrapper swallows errors so a ledger hiccup can never fail
	// an analysis job (see recordAnalysisCostEstimate).
	service.pipeline.SetCostEstimator(service.recordAnalysisCostEstimate)
	return service, nil
}

// ListProviderChannels returns operational channel metadata but never exposes
// a secret reference or key to the API layer.
func (s *Service) ListProviderChannels(ctx context.Context, capability string) ([]domain.ProviderChannel, error) {
	channels, err := s.repo.ListProviderChannels(ctx, capability)
	if err != nil {
		return nil, err
	}
	for i := range channels {
		for j := range channels[i].Members {
			member := &channels[i].Members[j]
			_, member.SecretReady, _ = s.secrets.Get(member.SecretRef)
			member.SecretRef = ""
		}
	}
	return channels, nil
}

// ProviderChannelRuntimeStatus is the admin-only, secret-free view of live
// provider-channel routing state: which member the pool has actually retired
// since the Hub started (a key answering 401/402/403 -- see
// providerpool.MemberSpent), as opposed to what the provider_channels table
// configures. ListProviderChannels above answers "what is configured";
// this answers "what is the Hub actually doing about it right now", which
// used to be invisible -- a retired key silently narrowed the route, and once
// the last one retired, jobs just stopped landing with nothing to explain
// why. See providerChannelRuntime.capabilityStatuses for what
// HasRuntimeData=false means and why it is reported rather than papered over.
func (s *Service) ProviderChannelRuntimeStatus(_ context.Context) []ProviderChannelCapabilityStatus {
	return s.channelRuntime.capabilityStatuses()
}

// SaveProviderChannel stores API keys only in the Hub secret store.
// memberKeys is positional, not keyed by label: memberKeys[i] corresponds to
// channel.Members[i]. Labels are unique per channel (UNIQUE(channel_id,
// label), migration 0013; validateDistinctProviderChannelMemberLabels
// rejects a duplicate below before anything is written), so memberKeys is
// positional because Members itself is positional — not because two members
// could share a label. An empty or missing entry preserves the member's
// existing secret; the slice may be shorter than Members, in which case the
// missing tail is treated as empty.
func (s *Service) SaveProviderChannel(ctx context.Context, channel domain.ProviderChannel, memberKeys []string) (domain.ProviderChannel, error) {
	if err := validateDistinctProviderChannelMemberLabels(channel.Members); err != nil {
		return domain.ProviderChannel{}, err
	}
	if err := validateProviderChannelCosts(channel); err != nil {
		return domain.ProviderChannel{}, err
	}
	if channel.ID == "" {
		channel.ID = idgen.New()
	}
	// Track which SecretRefs this call created (member.SecretRef was empty on
	// entry) so they can be rolled back if UpsertProviderChannel rejects the
	// channel below. Refs that already existed on a member are a key
	// overwrite, not a creation, and are deliberately left alone: undoing
	// those would require reading the previous plaintext key into memory
	// first just to restore it, which is a second in-memory copy of a secret
	// for a case that isn't the leak this guards against.
	var newlyCreatedRefs []string
	for i := range channel.Members {
		member := &channel.Members[i]
		if member.ID == "" {
			member.ID = idgen.New()
		}
		if member.SecretRef == "" {
			member.SecretRef = "provider-channel/" + channel.ID + "/" + member.ID
			newlyCreatedRefs = append(newlyCreatedRefs, member.SecretRef)
		}
		var key string
		if i < len(memberKeys) {
			key = strings.TrimSpace(memberKeys[i])
		}
		if key != "" {
			if err := s.secrets.Put(member.SecretRef, key); err != nil {
				return domain.ProviderChannel{}, err
			}
		}
	}
	saved, err := s.repo.UpsertProviderChannel(ctx, channel)
	if err != nil {
		// The channel row was rejected (e.g. UNIQUE constraint, missing
		// required field), so any secret this call just created has no
		// channel referencing it. Delete it now rather than leaving an
		// orphan in the encrypted store indefinitely. If the cleanup Delete
		// itself fails, its error is discarded rather than returned: the
		// Upsert error is the one the caller needs to see, and replacing or
		// wrapping it with a secondary cleanup failure would hide the actual
		// cause of the save failing.
		for _, ref := range newlyCreatedRefs {
			_ = s.secrets.Delete(ref)
		}
		return domain.ProviderChannel{}, err
	}
	return saved, nil
}

// AdminToken is used only by the in-process API authorization boundary. It is
// intentionally not included in API responses, database records, or logs.
func (s *Service) AdminToken() string { return s.adminToken }

// AgentToken is used only by the in-process API authorization boundary
// (requireAgentOrAdmin). Like AdminToken, it is intentionally never included
// in API responses, database records, or logs. It authorizes a strictly
// narrower surface than the admin token: creating and revising draft
// repurpose plans, never approval or pipeline runs.
func (s *Service) AgentToken() string { return s.agentToken }

// DataDir is the Hub's state directory. The API layer needs it to serve
// operator-supplied Worker binaries and derived artifacts from a known
// subdirectory; it is not a general-purpose filesystem escape hatch, and
// nothing derives a path from request input relative to it.
func (s *Service) DataDir() string { return s.cfg.DataDir }

// CacheDir is the directory for derived artifacts (thumbnails, proxies).
// It defaults to DataDir/cache but may be configured independently; the
// API layer must check both roots when serving artifacts.
func (s *Service) CacheDir() string { return s.cfg.CacheDir }

// PipelineThrottle reads the current disk-load limits.
func (s *Service) PipelineThrottle(ctx context.Context) (domain.PipelineThrottle, error) {
	return s.repo.GetPipelineThrottle(ctx)
}

// SavePipelineThrottle validates and persists the limits. It takes effect on the
// next lease rather than on restart: RunUntilIdle re-reads the throttle every
// iteration, so tightening it stops a scan that is already grinding.
func (s *Service) SavePipelineThrottle(ctx context.Context, throttle domain.PipelineThrottle) error {
	return s.repo.SavePipelineThrottle(ctx, throttle)
}

// TrustedReadNetworks are the CIDR ranges the API layer admits to the read
// routes that carry no token. Load() has already validated them, so a parse
// failure here cannot happen; the API layer still falls back to its restrictive
// defaults rather than assuming otherwise.
func (s *Service) TrustedReadNetworks() ([]netip.Prefix, error) {
	return s.cfg.HubSecurity.TrustedReadPrefixes()
}

// ErrShareNotMounted is returned when the operator gave a network share where a
// path was expected. It is a distinct error because the caller can do something
// useful with it — print the mount commands — that it cannot do with a generic
// stat failure.
type ErrShareNotMounted struct {
	Share mount.Share
}

func (e ErrShareNotMounted) Error() string {
	return fmt.Sprintf("%s is a network share and is not mounted here", e.Share)
}

func (s *Service) AddLibraryRoot(ctx context.Context, path string) (domain.LibraryRoot, error) {
	// The share check comes first because filepath.Abs turns //nas/Video into a
	// path relative to the working directory, at which point the input the
	// operator actually typed is no longer recoverable.
	if share, ok := mount.ParseShare(path); ok {
		if _, err := os.Stat(path); err != nil {
			return domain.LibraryRoot{}, ErrShareNotMounted{Share: share}
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return domain.LibraryRoot{}, fmt.Errorf("resolve root path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return domain.LibraryRoot{}, fmt.Errorf("stat root path: %w", err)
	}
	if !info.IsDir() {
		return domain.LibraryRoot{}, fmt.Errorf("path is not a directory: %s", absolute)
	}
	return s.repo.CreateLibraryRoot(ctx, absolute)
}

// RootWarnings reports what is true about a root's storage that the operator
// cannot see from the path alone and that will otherwise show up as unexplained
// slowness or as a library that scans to nothing.
func (s *Service) RootWarnings(path string, registered bool) []string {
	table := mount.ReadMountTable()
	if table == "" {
		return nil
	}
	var warnings []string
	if registered && mount.LooksUnmounted(path, table) {
		warnings = append(warnings, fmt.Sprintf("%s is empty and is not itself a mount point. If the share should be mounted there, mount it before scanning: a scan of an unmounted directory marks every asset in it as missing.", path))
	}
	filesystem, known := mount.FilesystemFor(path, table)
	if !known || !filesystem.Network {
		return warnings
	}
	warnings = append(warnings, fmt.Sprintf("%s is on a %s mount (%s).", path, filesystem.Label, filesystem.Type))
	if s.cfg.SourceStaging.Mode != "copy" {
		warnings = append(warnings, `Set "source_staging": {"mode": "copy"} in config.json. Every derive stage re-reads the source, and doing that over the network is what makes a NAS library take days rather than hours.`)
	}
	if !filesystem.ReadOnly {
		warnings = append(warnings, "The mount is writable. Nothing here writes to source footage, but mounting the share read-only makes that true of every other process too.")
	}
	return warnings
}

func (s *Service) ListLibraryRoots(ctx context.Context) ([]domain.LibraryRoot, error) {
	return s.repo.ListLibraryRoots(ctx)
}

// RootInspection reports what the Hub can tell about a candidate library root
// path before it becomes one — including the full mount commands when the
// operator gave a network share instead of a path — without ever reading
// anything beyond that single path: no directory listing, no globbing, and
// nothing said about any path other than the one asked about.
type RootInspection struct {
	Path string `json:"path"`

	// ContainerPath is set only when this Hub runs in a container and Path
	// lies under the directory bound in as footage: it is where that same
	// directory appears in here, and therefore the only one of the two paths
	// this process can stat or record as a library root. Everything below —
	// Exists, IsDir, FilesystemType, LooksUnmounted — describes this path
	// when it is set, because a verdict about the host-side path would be a
	// verdict about a namespace this process cannot see. Empty on a
	// bare-metal Hub, and empty when the translation cannot be stated as
	// fact rather than guessed.
	ContainerPath string `json:"container_path,omitempty"`

	// IsShare and Share are set when Path parses as a network share rather
	// than a local path. Share.User is the only credential fragment
	// mount.Share ever carries — see ParseShare's userinfo handling — so a
	// password is never accepted, parsed or echoed here.
	IsShare bool          `json:"is_share"`
	Share   *ShareSummary `json:"share,omitempty"`

	// DefaultMountpoint and Guidance are populated only for a share: the
	// commands the operator pastes to mount it, generated for this Hub's own
	// operating system.
	DefaultMountpoint string         `json:"default_mountpoint,omitempty"`
	Guidance          *MountGuidance `json:"guidance,omitempty"`

	// ComposeVolume is populated only for a share whose Hub is itself running
	// containerised (mount.Host.Container) — see InspectRootPath. On a
	// bare-metal host the mount commands above are simply the right answer
	// and a compose stanza would be noise; a containerised Hub genuinely
	// cannot mount the share for itself (mount.Guidance's
	// container-cannot-mount note), which is the case this suggestion exists
	// for.
	ComposeVolume *ComposeVolumeSuggestion `json:"compose_volume,omitempty"`

	// Exists and IsDir describe Path itself, from a single os.Stat.
	Exists bool `json:"exists"`
	IsDir  bool `json:"is_dir"`

	// FilesystemType, Network and NetworkLabel describe the mount Path falls
	// under, when the Hub's mount table is readable.
	FilesystemType string `json:"filesystem_type,omitempty"`
	Network        bool   `json:"network"`
	NetworkLabel   string `json:"network_label,omitempty"`
	LooksUnmounted bool   `json:"looks_unmounted"`

	// Warnings is the same advice Doctor prints for an existing root, offered
	// here before the root is even created.
	Warnings []string `json:"warnings,omitempty"`
}

// ShareSummary is the share half of a RootInspection.
type ShareSummary struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	User     string `json:"user,omitempty"`
}

// MountGuidance mirrors mount.Guide in a shape that survives a JSON round
// trip without exposing the mount package's types on the wire.
type MountGuidance struct {
	Summary string           `json:"summary"`
	Steps   []MountGuideStep `json:"steps"`
	Notes   []MountGuideNote `json:"notes"`
}

// MountGuideStep carries mount.Step's Key alongside the English Title so the
// browser wizard (internal/api/library_roots_page.go) can look up a Chinese
// translation by Key and fall back to Title — which stays the English
// text — when the key is unrecognised. The CLI (timingdex doctor) never sees
// this type; it renders mount.Guide directly and is unaffected by Key.
type MountGuideStep struct {
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Commands []string `json:"commands"`
}

// MountGuideNote is the same Key/English-fallback pairing as MountGuideStep,
// for mount.Note.
type MountGuideNote struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// ComposeVolumeSuggestion mirrors mount.VolumeDefinition in a shape that
// survives a JSON round trip without exposing the mount package's types on
// the wire — the same rationale as MountGuidance above.
type ComposeVolumeSuggestion struct {
	Name string `json:"name"`
	// YAML is the docker-compose named-volume entry, ready to splice under a
	// volumes: block. See mount.VolumeDefinition.YAML.
	YAML string `json:"yaml"`
	// ServiceYAML mounts that volume into the hub and worker services, and
	// MountPath is where it lands inside them — which makes MountPath the
	// path to record as a library root, not the NAS address the operator
	// typed. Without both, the volume entry above is inert and the operator
	// has to derive the root path themselves.
	ServiceYAML string `json:"service_yaml,omitempty"`
	MountPath   string `json:"mount_path,omitempty"`
	// Warning and WarningKey are empty together (the NFS form) or set
	// together (the SMB form, which cannot avoid an inline cleartext
	// password — see mount.ComposeVolume's doc). WarningKey carries the same
	// Key/English-fallback translation contract as MountGuideStep.Key.
	Warning    string `json:"warning,omitempty"`
	WarningKey string `json:"warning_key,omitempty"`
}

// InspectRootPath answers "what would adding this as a library root involve"
// without touching the repository or creating anything. mountpoint overrides
// mount.DefaultMountpoint for the share case — the wizard calls this again
// with an edited mountpoint to regenerate the commands, since the exact
// command text (credentials path, uid/gid, the WSL nsenter prefix) is
// generated here rather than duplicated in JavaScript. Passing "" for
// mountpoint uses the default.
func (s *Service) InspectRootPath(ctx context.Context, path, mountpoint string) RootInspection {
	trimmed := strings.TrimSpace(path)
	result := RootInspection{Path: trimmed}
	registered := false
	if s.repo != nil {
		registered, _ = s.repo.IsLibraryRoot(ctx, trimmed)
	}
	if trimmed == "" {
		return result
	}
	host := s.mountHost()
	if share, ok := mount.ParseShare(trimmed); ok {
		result.IsShare = true
		// Echo the parsed share rather than what was typed. Share.String()
		// reconstructs the location from host and share name only, so a
		// password pasted as smb://user:password@host/share cannot survive
		// into this response — and this response is rendered straight into
		// the browser page that asked for it. ParseShare already drops the
		// password from Share.User; leaving the raw input in Path would have
		// put it back in the reply anyway.
		result.Path = share.String()
		result.Share = &ShareSummary{
			Protocol: string(share.Protocol),
			Host:     share.Host,
			Name:     share.Name,
			User:     share.User,
		}
		result.DefaultMountpoint = mount.DefaultMountpoint(share, host)
		target := strings.TrimSpace(mountpoint)
		if target == "" {
			target = result.DefaultMountpoint
		}
		guide := mount.Guidance(share, target, host)
		steps := make([]MountGuideStep, 0, len(guide.Steps))
		for _, step := range guide.Steps {
			steps = append(steps, MountGuideStep{Key: step.Key, Title: step.Title, Commands: step.Commands})
		}
		notes := make([]MountGuideNote, 0, len(guide.Notes))
		for _, note := range guide.Notes {
			notes = append(notes, MountGuideNote{Key: note.Key, Text: note.Text})
		}
		result.Guidance = &MountGuidance{Summary: guide.Summary, Steps: steps, Notes: notes}
		if host.Container {
			// See ComposeVolume's field doc: this is the one case where a
			// docker-compose volume stanza is the actual next step rather
			// than noise alongside the host mount commands above.
			if volume, ok := mount.ComposeVolume(share, mount.VolumeName(share)); ok {
				result.ComposeVolume = &ComposeVolumeSuggestion{
					Name:        volume.Name,
					YAML:        volume.YAML,
					ServiceYAML: volume.ServiceYAML,
					MountPath:   volume.MountPath,
					Warning:     volume.Warning,
					WarningKey:  volume.WarningKey,
				}
			}
		}
	}
	// Everything below stats the filesystem. On a containerised Hub the path
	// the operator is verifying is one they were told to mount on the *host*,
	// which this process cannot stat — so translate it to where the same
	// directory appears in here first. Without this, correctly following the
	// guidance ends in "does not exist" at the verification step, because the
	// answer would be about a path that only ever existed in another
	// namespace. The untranslated path is kept in Path so the operator still
	// sees the one they typed; ContainerPath is reported alongside so the page
	// can name both rather than silently swapping one for the other.
	inspected := trimmed
	if translated, ok := mount.ContainerPath(trimmed, host); ok {
		inspected = translated
		result.ContainerPath = translated
	}
	if info, err := os.Stat(inspected); err == nil {
		result.Exists = true
		result.IsDir = info.IsDir()
	}
	if table := mount.ReadMountTable(); table != "" {
		if filesystem, known := mount.FilesystemFor(inspected, table); known {
			result.FilesystemType = filesystem.Type
			result.Network = filesystem.Network
			result.NetworkLabel = filesystem.Label
		}
		result.LooksUnmounted = mount.LooksUnmounted(inspected, table)
	}
	result.Warnings = s.RootWarnings(trimmed, registered)
	return result
}

func (s *Service) CreateWorkerPairing(ctx context.Context, ttl time.Duration) (remote.PairingToken, error) {
	return s.repo.CreateWorkerPairing(ctx, ttl)
}

func (s *Service) EnrollWorker(ctx context.Context, pairingToken string, registration remote.WorkerRegistration) (remote.Worker, string, error) {
	worker, token, err := s.repo.EnrollWorker(ctx, pairingToken, registration)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "pairing token") {
			return remote.Worker{}, "", fmt.Errorf("%w: %v", ErrPairingTokenInvalid, err)
		}
		return remote.Worker{}, "", fmt.Errorf("worker enrollment: %w", err)
	}
	return worker, token, nil
}

func (s *Service) AuthenticateWorker(ctx context.Context, token string) (remote.Worker, error) {
	return s.repo.AuthenticateWorker(ctx, token)
}

func (s *Service) HeartbeatWorker(ctx context.Context, workerID, version string, capabilities remote.WorkerCapabilities) error {
	return s.repo.HeartbeatWorker(ctx, workerID, version, capabilities)
}

func (s *Service) ListWorkers(ctx context.Context) ([]remote.Worker, error) {
	return s.repo.ListWorkers(ctx)
}

// LeaseNextWorkerDerive applies the same throttle the local pipeline obeys. The
// Hub decides the rate and the size ceiling rather than the Worker, because the
// setting protects a disk both of them read and a Worker must not be able to
// opt itself out of it.
func (s *Service) LeaseNextWorkerDerive(ctx context.Context, worker remote.Worker) (*remote.WorkerJob, error) {
	throttle, err := s.repo.GetPipelineThrottle(ctx)
	if err != nil {
		slog.Warn("pipeline throttle unreadable; leasing worker derive unthrottled", "error", err)
		throttle = domain.DefaultPipelineThrottle()
	}
	// The same disk preflight the local pipeline runs before a heavy stage
	// gates worker derive leases too: the uploaded proxy lands on the Hub's
	// own volume (DataDir/derived/worker), and a worker that keeps deriving
	// while that volume is full fills it further — the upload then fails with
	// ENOSPC and the job burns attempts. The floor resolution mirrors the
	// pipeline's (settings row wins when it exists, including an explicit 0 =
	// disabled; otherwise the static config floor), and the probe fails open:
	// an unreadable statfs or a Windows host never blocks work.
	configured, cfgErr := s.repo.PipelineThrottleConfigured(ctx)
	floor := s.cfg.Pipeline.MinimumFreeSpaceBytes
	if cfgErr == nil && configured {
		floor = throttle.MinimumFreeSpaceBytes
	}
	if floor > 0 {
		if free, freeErr := freeBytes(s.cfg.DataDir); freeErr == nil && free < floor {
			slog.Warn("cache volume below the configured free-space floor; not leasing worker derive", "free_bytes", free, "min_free_bytes", floor)
			return nil, nil
		}
	}
	now := time.Now()
	job, err := s.repo.LeaseNextWorkerDerive(ctx, worker, 30*time.Minute, domain.LeaseFilter{MaxAssetBytes: throttle.MaxAssetBytesAt(now)})
	if err != nil || job == nil {
		return job, err
	}
	job.ReadRate = throttle.ReadRateFor(job.SourceBytes)
	return job, nil
}

func (s *Service) CompleteWorkerJob(ctx context.Context, jobID, workerID string, state domain.JobState, message string) error {
	return s.repo.CompleteWorkerJob(ctx, jobID, workerID, state, message)
}

// RecordWorkerJobProgress accepts only bounded, lease-owned progress reports.
// It is deliberately a separate transition from completion so the Hub can
// present a useful, recoverable workflow state while a Worker is still busy.
func (s *Service) RecordWorkerJobProgress(ctx context.Context, jobID, workerID, stage string, progress float64, event, message string) error {
	return s.repo.RecordWorkerJobProgress(ctx, jobID, workerID, stage, progress, event, message)
}

func (s *Service) GetWorkerJobStatus(ctx context.Context, jobID string) (remote.WorkerJobStatus, error) {
	return s.repo.GetWorkerJobStatus(ctx, jobID)
}

func (s *Service) SetDeriveWorkerAssignment(ctx context.Context, jobID, workerID string, mode remote.WorkerAssignmentMode) error {
	return s.repo.SetDeriveWorkerAssignment(ctx, jobID, workerID, mode)
}

// IssueWorkerCredential permits an explicitly opted-in, trusted Worker to call
// a Provider directly for the job it currently owns. The API key is returned
// only in this response and must stay in Worker memory; the lease expiry is an
// audit/delivery window, not revocation of a third-party long-lived key.
func (s *Service) IssueWorkerCredential(ctx context.Context, worker remote.Worker, jobID string, operation credentials.Operation) (credentials.Lease, error) {
	if !s.cfg.HubSecurity.AllowWorkerProviderCredentials {
		return credentials.Lease{}, ErrWorkerProviderCredentialDeliveryDisabled
	}
	if !workerSupportsOperation(worker.Capabilities, operation) {
		return credentials.Lease{}, fmt.Errorf("worker does not declare provider operation %q", operation)
	}
	lease, err := credentials.NewBroker(s.cfg.Providers, nil).Issue(jobID, worker.ID, operation, 5*time.Minute)
	if err != nil {
		return credentials.Lease{}, s.classifyWorkerProviderBrokerErr(ctx, operation, err)
	}
	expiresAt, err := time.Parse(time.RFC3339, lease.ExpiresAt)
	if err != nil {
		return credentials.Lease{}, fmt.Errorf("invalid generated credential lease: %w", err)
	}
	if err := s.repo.RecordProviderCredentialLease(ctx, jobID, worker.ID, lease.Provider, string(operation), expiresAt); err != nil {
		return credentials.Lease{}, err
	}
	return lease, nil
}

func workerSupportsOperation(capabilities remote.WorkerCapabilities, operation credentials.Operation) bool {
	for _, candidate := range capabilities.ProviderOperations {
		if strings.EqualFold(strings.TrimSpace(candidate), string(operation)) {
			return true
		}
	}
	return false
}

const maxWorkerArtifactBytes int64 = 2 << 30

// UploadWorkerArtifact stores a worker result under Hub-owned derived storage
// and commits its database record only while the worker still owns the lease.
// The temporary file is local to the Hub and never becomes part of the worker
// configuration or job event payload.
func (s *Service) UploadWorkerArtifact(ctx context.Context, jobID, workerID, artifactType, profileHash, contentType string, source io.Reader) (domain.DerivedArtifact, bool, error) {
	if source == nil || strings.TrimSpace(jobID) == "" || strings.TrimSpace(workerID) == "" {
		return domain.DerivedArtifact{}, false, fmt.Errorf("%w: missing upload input", ErrInvalidWorkerArtifact)
	}
	if artifactType != "thumbnail" && artifactType != "proxy" && artifactType != "audio" {
		return domain.DerivedArtifact{}, false, fmt.Errorf("%w: unsupported type %q", ErrInvalidWorkerArtifact, artifactType)
	}
	if !safeWorkerArtifactSegment(profileHash) {
		return domain.DerivedArtifact{}, false, fmt.Errorf("%w: invalid profile hash", ErrInvalidWorkerArtifact)
	}
	assetID, existing, err := s.repo.PrepareWorkerArtifact(ctx, jobID, workerID, artifactType, profileHash)
	if err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			return domain.DerivedArtifact{}, false, fmt.Errorf("%w: %w", ErrWorkerArtifactLease, err)
		}
		return domain.DerivedArtifact{}, false, err
	}
	preserveExisting := false
	if existing != nil {
		if info, statErr := os.Stat(existing.LocalPath); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			preserveExisting = true
			stored, reused, commitErr := s.repo.CommitWorkerArtifact(ctx, jobID, workerID, *existing, true)
			if commitErr != nil {
				if errors.Is(commitErr, domain.ErrJobLeaseLost) {
					return domain.DerivedArtifact{}, false, fmt.Errorf("%w: %w", ErrWorkerArtifactLease, commitErr)
				}
				return domain.DerivedArtifact{}, false, commitErr
			}
			return stored, reused, nil
		}
	}
	if strings.TrimSpace(s.cfg.DataDir) == "" {
		return domain.DerivedArtifact{}, false, fmt.Errorf("%w: Hub data directory is not configured", ErrInvalidWorkerArtifact)
	}
	targetDir := filepath.Join(s.cfg.DataDir, "derived", "worker", assetID)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return domain.DerivedArtifact{}, false, fmt.Errorf("create derived storage: %w", err)
	}
	extension := workerArtifactExtension(artifactType, contentType)
	targetPath := filepath.Join(targetDir, artifactType+"-"+profileHash+extension)
	if _, statErr := os.Stat(targetPath); statErr == nil {
		return domain.DerivedArtifact{}, false, fmt.Errorf("%w: target artifact path already exists", ErrInvalidWorkerArtifact)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return domain.DerivedArtifact{}, false, fmt.Errorf("inspect derived storage: %w", statErr)
	}
	tmp, err := os.CreateTemp(targetDir, ".worker-upload-*")
	if err != nil {
		return domain.DerivedArtifact{}, false, fmt.Errorf("create upload temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	size, copyErr := io.Copy(tmp, io.LimitReader(source, maxWorkerArtifactBytes+1))
	if copyErr != nil {
		cleanup()
		return domain.DerivedArtifact{}, false, fmt.Errorf("write worker artifact: %w", copyErr)
	}
	if size <= 0 || size > maxWorkerArtifactBytes {
		cleanup()
		return domain.DerivedArtifact{}, false, fmt.Errorf("%w: artifact size must be between 1 byte and %d bytes", ErrInvalidWorkerArtifact, maxWorkerArtifactBytes)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return domain.DerivedArtifact{}, false, fmt.Errorf("sync worker artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return domain.DerivedArtifact{}, false, fmt.Errorf("close worker artifact: %w", err)
	}
	if err := os.Link(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return domain.DerivedArtifact{}, false, fmt.Errorf("commit worker artifact file: %w", err)
	}
	_ = os.Remove(tmpPath)
	artifact := domain.DerivedArtifact{ID: idgen.New(), AssetID: assetID, Type: artifactType, ProfileHash: profileHash, LocalPath: targetPath, SizeBytes: size}
	stored, reused, err := s.repo.CommitWorkerArtifact(ctx, jobID, workerID, artifact, preserveExisting)
	if err != nil {
		if !preserveExisting && !reused {
			_ = os.Remove(targetPath)
		}
		if errors.Is(err, domain.ErrJobLeaseLost) {
			return domain.DerivedArtifact{}, false, fmt.Errorf("%w: %w", ErrWorkerArtifactLease, err)
		}
		return domain.DerivedArtifact{}, false, err
	}
	if reused && stored.LocalPath != targetPath {
		_ = os.Remove(targetPath)
	}
	return stored, reused, nil
}

func safeWorkerArtifactSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func workerArtifactExtension(artifactType, contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/png":
		return ".png"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/mpeg":
		return ".mp3"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	switch artifactType {
	case "thumbnail":
		return ".jpg"
	case "audio":
		return ".wav"
	default:
		return ".mp4"
	}
}

func (s *Service) ScanLibraryRoot(ctx context.Context, rootID string) (domain.ScanResult, error) {
	s.scanRootLocksMu.Lock()
	if s.scanRootLocks == nil {
		s.scanRootLocks = make(map[string]*sync.Mutex)
	}
	rootLock := s.scanRootLocks[rootID]
	if rootLock == nil {
		rootLock = &sync.Mutex{}
		s.scanRootLocks[rootID] = rootLock
	}
	s.scanRootLocksMu.Unlock()
	rootLock.Lock()
	defer rootLock.Unlock()

	root, err := s.repo.GetLibraryRoot(ctx, rootID)
	if err != nil {
		return domain.ScanResult{}, err
	}
	// MarkRootScanStarted is bookkeeping, not a precondition: the gate below
	// writes the authoritative verdict from this scan's walk, so a failed
	// start-mark only leaves last_scan_at stale for one interval and heals on
	// the next pass. Blocking the scan itself on it would give a bookkeeping
	// write the power to stop discovery.
	now := time.Now().UTC()
	if err := s.repo.MarkRootScanStarted(ctx, rootID, now); err != nil {
		slog.Warn("scan: failed to record scan start", "root_id", rootID, "error", err)
	}
	result, err := s.scanner.Scan(ctx, root)
	if err != nil {
		return result, err
	}
	// Reconciliation gate. The scanner never marks files missing itself — it
	// returns the seen list and this is the only place a scan may reconcile,
	// decided from the root's state AFTER the walk. An unmounted NAS root
	// walks as an empty directory; without this gate that walk would mark
	// every asset in the root missing. The verdict marks are best-effort for
	// the same reason the start-mark is: every scan recomputes them from
	// scratch, so a lost write self-heals on the next pass and can never
	// weaken the gate (the gate never reads the persisted verdict). A failure
	// to write the reconciliation itself stays fatal, as it was when the
	// scanner owned it.
	if s.rootHealthyAfterScan(root, result) {
		if err := s.repo.MarkRootHealthy(ctx, rootID, now); err != nil {
			slog.Warn("scan: failed to record root healthy", "root_id", rootID, "error", err)
		}
		missing, err := s.repo.MarkUnseenLocationsMissing(ctx, root.ID, result.SeenRelativePaths)
		if err != nil {
			return result, err
		}
		result.Missing = missing
	} else {
		if err := s.repo.MarkRootUnavailable(ctx, rootID, now); err != nil {
			slog.Warn("scan: failed to record root unavailable", "root_id", rootID, "error", err)
		}
	}
	// Enqueue only the assets this scan actually changed. Re-enqueuing the
	// whole library on every 15-minute pass is 2N queries that all land on an
	// INSERT OR IGNORE no-op; the changed set is the only work a scan creates.
	//
	// counted tracks every asset this scan pass already processed so that an
	// asset appearing in both the changed set and the catch-up query cannot
	// double-increment its scan-failure counter.  After the pass,
	// scanFailures entries for this root that are absent from counted are
	// pruned so the map cannot grow without bound.
	//
	// scanFailures is per-root (map[rootID]map[assetID]count).  Each scan
	// copies the existing counts into a scan-local map and only merges back
	// at the end.  Within a single root, concurrent scans are rare (normal
	// operation is a periodic single-threaded timer), and the merge keeps
	// the maximum count per asset while pruning assets absent from this
	// scan's counted set.  If two scans of the same root do overlap, the
	// later merge may prune an asset the earlier scan counted but did not
	// itself see — acceptable for a warning-counter, not a functional path.
	rootFailures := func() map[string]int {
		s.scanFailuresMu.Lock()
		defer s.scanFailuresMu.Unlock()
		if s.scanFailures[rootID] == nil {
			s.scanFailures[rootID] = make(map[string]int)
		}
		// Deep-copy the existing map so this scan never mutates another
		// concurrent scan's view of the same root's failure counts.
		local := make(map[string]int, len(s.scanFailures[rootID]))
		for k, v := range s.scanFailures[rootID] {
			local[k] = v
		}
		return local
	}()

	counted := make(map[string]bool, len(result.ChangedAssetIDs)+1000)
	for _, assetID := range result.ChangedAssetIDs {
		counted[assetID] = true
		if err := s.pipeline.EnqueueAsset(ctx, assetID); err != nil {
			// A single enqueue failure must not abort the pass: the catch-up
			// query below only rescues a missing probe job if the pass
			// completes, so the same asset gets another chance next scan.
			rootFailures[assetID]++
			count := rootFailures[assetID]
			if count > 0 && count%5 == 0 {
				slog.Error("scan: enqueue changed asset failed 5 consecutive passes", "asset_id", assetID, "consecutive_failures", count, "error", err)
			} else {
				slog.Warn("scan: enqueue changed asset failed", "asset_id", assetID, "error", err)
			}
		} else {
			delete(rootFailures, assetID)
		}
	}
	// Catch up on assets that never got a probe job at all — nothing in the
	// changed set will ever re-derive them, so without this they would starve
	// forever. The limit keeps one pathological root from making a scan
	// unbounded; assets past the cap are picked up by a later pass.
	// Duplicates against the changed set are skipped so an asset appearing
	// in both lists cannot double-increment its failure counter.
	catchUp, err := s.repo.AssetsWithoutProbeJob(ctx, rootID, 1000)
	if err != nil {
		return result, err
	}
	for _, assetID := range catchUp {
		if counted[assetID] {
			continue
		}
		counted[assetID] = true
		if err := s.pipeline.EnqueueAsset(ctx, assetID); err != nil {
			rootFailures[assetID]++
			count := rootFailures[assetID]
			if count > 0 && count%5 == 0 {
				slog.Error("scan: enqueue missing-probe asset failed 5 consecutive passes", "asset_id", assetID, "consecutive_failures", count, "error", err)
			} else {
				slog.Warn("scan: enqueue missing-probe asset failed", "asset_id", assetID, "error", err)
			}
		} else {
			delete(rootFailures, assetID)
		}
	}
	// Merge scan-local failure counts back into the shared map.  Only assets
	// in this scan's counted set are updated; assets counted by a concurrent
	// scan of the same root are left untouched.  The higher count wins so
	// that two concurrent scans both incrementing the same asset do not
	// undercount.
	s.scanFailuresMu.Lock()
	if s.scanFailures[rootID] == nil {
		s.scanFailures[rootID] = make(map[string]int)
	}
	// Prune assets that this scan did not encounter.
	for id := range s.scanFailures[rootID] {
		if !counted[id] {
			delete(s.scanFailures[rootID], id)
		}
	}
	// Merge local counts: keep the higher of the existing and local count.
	for id, count := range rootFailures {
		if !counted[id] {
			continue
		}
		if existing, ok := s.scanFailures[rootID][id]; !ok || count > existing {
			s.scanFailures[rootID][id] = count
		}
	}
	s.scanFailuresMu.Unlock()
	return result, nil
}

// rootHealthyAfterScan is the reconciliation gate: whether this scan's walk
// result may be used to mark previously-seen files missing.
//
// The boundary it protects: an unmounted NAS root walks exactly like an empty
// directory, and marking every asset missing from that walk is the data loss
// this gate exists to prevent. The rules therefore err on the side of NOT
// reconciling —
//
//   - the walk itself reported the root path unreachable → not healthy
//   - the root directory no longer exists (os.Stat fails) → not healthy
//   - mount.LooksUnmounted: the directory is empty and the mount table says
//     it resolves to a different filesystem (the state of a mount whose share
//     did not come back after a reboot) → not healthy
//   - the directory is empty: a genuinely empty root cannot be distinguished
//     from an unmounted one, and the alternative — scanning it and recording
//     every asset as missing — is worse than saying so, so an empty directory
//     is never proof of health and the walk does not reconcile
//
// Errors about files or subdirectories inside the root do not fail the gate:
// one unreadable clip must not stop the library from reconciling files that
// were genuinely deleted.
func (s *Service) rootHealthyAfterScan(root domain.LibraryRoot, result domain.ScanResult) bool {
	// The scanner records the walker's error verbatim, which for the root
	// itself carries the path directly after the kernel verb ("lstat
	// /mnt/nas: no such file or directory", "open /mnt/nas: permission
	// denied"). The check is positional — the root path must directly precede
	// the colon — so an error about a file inside the root never matches.
	if len(result.Errors) > 0 && rootPathUnreachableInErrors(result.Errors, root) {
		return false
	}
	// The walk can also complete without errors while the root is gone if the
	// directory vanished after WalkDir's initial lstat; stat it afresh.
	if _, err := os.Stat(root.Path); err != nil {
		return false
	}
	if mount.LooksUnmounted(root.Path, mount.ReadMountTable()) {
		return false
	}
	entries, err := os.ReadDir(root.Path)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// rootPathUnreachableInErrors reports whether result.Errors contains the
// walk's failure to reach the root directory itself. The scanner appends the
// raw walker error for the root path, so the verbs are the ones the kernel's
// os.Lstat/os.Open produce on any platform this runs on.
func rootPathUnreachableInErrors(errs []string, root domain.LibraryRoot) bool {
	for _, e := range errs {
		for _, verb := range []string{"lstat ", "stat ", "open ", "readdir ", "readdirent "} {
			if strings.HasPrefix(e, verb+root.Path+":") {
				return true
			}
		}
	}
	return false
}

func (s *Service) ListAssets(ctx context.Context, limit, offset int) ([]domain.Asset, error) {
	return s.repo.ListAssets(ctx, limit, offset)
}

// ReanalysisSelector picks the assets a reanalysis run should cover.
type ReanalysisSelector struct {
	AssetID string
	RootID  string
	All     bool
	Reason  string
}

// ResolveReanalysisAssets turns a selector into the concrete asset list.
// Exactly one of AssetID / RootID / All must be set; combining them would be
// ambiguous about intent and is rejected rather than silently preferring one.
func (s *Service) ResolveReanalysisAssets(ctx context.Context, sel ReanalysisSelector) ([]string, error) {
	selectors := 0
	if sel.AssetID != "" {
		selectors++
	}
	if sel.RootID != "" {
		selectors++
	}
	if sel.All {
		selectors++
	}
	if selectors != 1 {
		return nil, fmt.Errorf("select exactly one of --asset, --root or --all")
	}
	switch {
	case sel.AssetID != "":
		asset, err := s.repo.GetAssetDetail(ctx, sel.AssetID)
		if err != nil {
			return nil, err
		}
		if asset == nil {
			return nil, fmt.Errorf("asset %s not found", sel.AssetID)
		}
		return []string{sel.AssetID}, nil
	case sel.RootID != "":
		if _, err := s.repo.GetLibraryRoot(ctx, sel.RootID); err != nil {
			return nil, err
		}
		assets, err := s.repo.ListAssets(ctx, 1<<31-1, 0)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(assets))
		for _, asset := range assets {
			loc, err := s.repo.GetPrimaryLocation(ctx, asset.ID)
			if err != nil {
				// A catalogued asset can briefly have no on-disk location (a
				// scan removed its file); that must not fail every other asset
				// in the root with a raw sql error.
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return nil, err
			}
			if loc.RootID == sel.RootID {
				ids = append(ids, asset.ID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("root %s has no assets", sel.RootID)
		}
		return ids, nil
	case sel.All:
		assets, err := s.repo.ListAssets(ctx, 1<<31-1, 0)
		if err != nil {
			return nil, err
		}
		if len(assets) == 0 {
			return nil, fmt.Errorf("library has no assets to reanalyze")
		}
		ids := make([]string, 0, len(assets))
		for _, asset := range assets {
			ids = append(ids, asset.ID)
		}
		return ids, nil
	default:
		return nil, fmt.Errorf("select one of --asset, --root or --all")
	}
}

// ReanalyzeAssets enqueues a fresh analyze job for each asset. Old model runs
// stay immutable; the new run switches the canonical analysis when it lands.
func (s *Service) ReanalyzeAssets(ctx context.Context, assetIDs []string, reason string) (int, error) {
	if reason == "" {
		reason = "reanalysis-v1"
	}
	enqueued := 0
	for _, id := range assetIDs {
		if err := s.repo.EnqueueReanalysis(ctx, id, reason); err != nil {
			return enqueued, fmt.Errorf("enqueue reanalysis for %s: %w", id, err)
		}
		enqueued++
	}
	return enqueued, nil
}

// Doctor renders the DoctorReport as the sectioned operator console and
// returns an error only when a HARD check failed: a broken SQLite integrity
// check, or a report that could not be collected (a repository error). Every
// advisory fact — a missing helper binary, a root in an unhealthy state, low
// disk — is a printed ⚠/✗ line, never a non-zero exit, so the exit code
// keeps meaning "the Hub's own state is in question".
func (s *Service) Doctor(ctx context.Context, writer io.Writer) error {
	report, err := s.DoctorReport(ctx)
	if err != nil {
		return err
	}
	printDoctorReport(writer, report)
	if !report.DB.IntegrityOK {
		return fmt.Errorf("SQLite integrity check failed; restore from a backup before continuing")
	}
	return nil
}

func (s *Service) HardwareReport() media.HardwareReport { return s.hardware }

// HealOnStartup runs the crash-recovery sweep for stale 'running' jobs once,
// right after NewService, on the CLI paths that actually run the queue
// (`serve`, `pipeline run`). It is deliberately not called from NewService
// itself: tests construct a Service for nearly every test in this package,
// and an implicit database sweep on every construction would be a surprise
// side effect nobody asked for. Failures are logged, never fatal — a Hub can
// serve reads while the next lease attempt reclaims a stale job anyway.
func (s *Service) HealOnStartup(ctx context.Context) error {
	_, _, err := s.repo.HealStaleRunningJobs(ctx, time.Now())
	return err
}

func (s *Service) RunPipeline(ctx context.Context) error {
	executed, err := s.pipeline.RunUntilIdle(ctx)
	if err != nil {
		return err
	}
	// Sessions are derived from assets, and an idle pass changed no asset, so
	// there is nothing for a rebuild to pick up. Rebuilding anyway would
	// re-derive every session per root on every supervisor tick — four times an
	// hour on an idle library — for zero effect. Zero executed jobs is a safe
	// signal here: RunUntilIdle counts a job the moment it leaves the queue,
	// so a pass that deferred work on a dead route still reports non-zero.
	if executed == 0 {
		return nil
	}
	roots, err := s.repo.ListLibraryRoots(ctx)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if err := s.repo.RebuildAutomaticShootSessions(ctx, root.ID); err != nil {
			return err
		}
	}
	return nil
}

// StartPipeline launches a background pipeline run if none is currently active.
// It returns true when a new goroutine was started, or false if one was already
// running. The caller never blocks — the lifecycle is detached from any HTTP
// request context. CLI callers that need synchronous behaviour should use
// RunPipeline instead.
func (s *Service) StartPipeline() bool {
	if !s.beginPipelinePass() {
		return false
	}
	go func() {
		defer s.endPipelinePass()
		if err := s.RunPipeline(context.Background()); err != nil {
			slog.Error("pipeline run failed", "error", err)
		}
	}()
	return true
}

// TryRunPipeline runs a pass on the caller's own goroutine, under the same
// single-run guard as StartPipeline, and reports whether it ran at all.
//
// It exists for a caller that must be able to stop the pass and to know when it
// has stopped. The unattended supervisor's context is the server's, and a pass
// detached onto context.Background() the way StartPipeline detaches it would go
// on spending Provider calls after shutdown had begun, with nothing left to
// join it.
func (s *Service) TryRunPipeline(ctx context.Context) (bool, error) {
	if !s.beginPipelinePass() {
		return false, nil
	}
	defer s.endPipelinePass()
	return true, s.RunPipeline(ctx)
}

// beginPipelinePass claims the right to be the one pipeline pass in flight.
// Every way of starting a pass goes through it, so an operator pressing "run"
// and the supervisor waking up cannot end up driving the same disk at once —
// which would defeat the throttle rather than obey it.
func (s *Service) beginPipelinePass() bool {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()
	if s.pipelineRunning {
		return false
	}
	s.pipelineRunning = true
	return true
}

func (s *Service) endPipelinePass() {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()
	s.pipelineRunning = false
}

// RunLibrarySupervisor blocks until ctx is cancelled, rescanning the library and
// draining the queue on a timer. It returns immediately when the supervisor is
// disabled, which is the default. When it returns, no work it started is still
// in flight — see LibrarySupervisor.Run.
func (s *Service) RunLibrarySupervisor(ctx context.Context) error {
	return s.supervisor.Run(ctx)
}

// LibrarySupervisorStatus is the observability surface for the unattended loop.
func (s *Service) LibrarySupervisorStatus() LibrarySupervisorStatus {
	return s.supervisor.Status()
}

// PipelineRunning reports whether a background pipeline goroutine is currently
// executing.
func (s *Service) PipelineRunning() bool {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()
	return s.pipelineRunning
}

func (s *Service) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	return s.pipeline.repo.ListJobs(ctx, limit)
}

func (s *Service) JobSummary(ctx context.Context) (domain.JobSummary, error) {
	return s.pipeline.repo.JobSummary(ctx)
}

// RequeueFailedJobs gives every failed job a fresh attempt budget. Both ways a
// job stops being retried are one-way, and re-scanning the library does not
// undo either, so configuring a provider that was previously missing otherwise
// leaves the work that failed for want of it stranded forever.
func (s *Service) RequeueFailedJobs(ctx context.Context) (int, error) {
	return s.pipeline.repo.RequeueFailedJobs(ctx)
}

// ResumeDeferredJobs releases work parked because every provider key on its
// route was failing. It is the operator's answer to a wait that has already
// ended — a topped-up account, or an outage that cleared — which the Hub
// cannot detect on its own without spending a call to find out.
func (s *Service) ResumeDeferredJobs(ctx context.Context) (int, error) {
	return s.pipeline.repo.ResumeDeferredJobs(ctx, domain.JobDeferProviderRouteExhausted)
}
func (s *Service) Search(ctx context.Context, q string, limit int) ([]string, error) {
	return s.pipeline.repo.Search(ctx, q, limit)
}

// SearchFiltered narrows Search by domain.FacetFilter. Callers must validate
// facet values against normalize's exported *Values lists before calling
// this — see the FacetFilter doc comment (internal/domain/asset_browse.go).
func (s *Service) SearchFiltered(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]string, error) {
	return s.repo.SearchFiltered(ctx, q, limit, facets)
}

func (s *Service) ListAssetShots(ctx context.Context, assetID string) ([]domain.AssetShot, error) {
	return s.repo.ListAssetShots(ctx, assetID)
}

func (s *Service) SearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	return s.repo.SearchShots(ctx, q, limit)
}

// SearchShotsFiltered narrows SearchShots by domain.FacetFilter. Callers must
// validate facet values against normalize's exported *Values lists before
// calling this — see the FacetFilter doc comment (internal/domain/asset_browse.go).
func (s *Service) SearchShotsFiltered(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return s.repo.SearchShotsFiltered(ctx, q, limit, facets)
}

func (s *Service) HybridSearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	return s.HybridSearchShotsFiltered(ctx, q, limit, domain.FacetFilter{})
}

// HybridSearchShotsFiltered narrows HybridSearchShots by domain.FacetFilter.
// When the Search v2 engine is wired, this delegates to its compatibility
// path (LegacySearch), which reproduces the legacy hybrid ranking exactly —
// pinned by the golden set's equality test — so the old GET endpoints, the
// MCP tool and the repurpose planner keep their behaviour under the new
// engine. Without the engine (test fakes), it falls back to the repository.
func (s *Service) HybridSearchShotsFiltered(ctx context.Context, q string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	if s.searchV2 != nil {
		return s.searchV2.LegacySearch(ctx, q, limit, facets)
	}
	return s.repo.HybridSearchShotsFiltered(ctx, q, limit, facets)
}

// SearchV2 runs the structured Search v2 pipeline (compile, route, channels,
// fusion, evidence gate, selection) and returns the evidence-bearing
// response. It is the backend of POST /api/v1/search/shots.
func (s *Service) SearchV2(ctx context.Context, req search.SearchRequest) (*search.SearchResponse, error) {
	if s.searchV2 == nil {
		return nil, fmt.Errorf("search engine not available")
	}
	return s.searchV2.Search(ctx, req)
}

func (s *Service) SimilarShots(ctx context.Context, shotID string, limit int) ([]domain.ShotSearchResult, error) {
	return s.repo.SimilarShots(ctx, shotID, limit)
}

// SimilarShotsFiltered narrows SimilarShots by domain.FacetFilter.
func (s *Service) SimilarShotsFiltered(ctx context.Context, shotID string, limit int, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return s.repo.SimilarShotsFiltered(ctx, shotID, limit, facets)
}

func (s *Service) DiscoverRareShots(ctx context.Context, limit int) ([]domain.RareShot, error) {
	return s.repo.DiscoverRareShots(ctx, limit)
}

func (s *Service) CreateRepurposePlan(ctx context.Context, brief domain.RepurposeBrief) (domain.RepurposePlan, error) {
	brief.Brief = strings.TrimSpace(brief.Brief)
	if brief.Brief == "" {
		return domain.RepurposePlan{}, fmt.Errorf("brief is required")
	}

	provider, model := "deterministic", "heuristic-v1"
	var draft domain.RepurposePlanDraft
	var err error
	providerUnavailable := false
	if s.planner != nil {
		provider, model = s.planner.Name(), s.planner.Model()
		draft, err = s.planner.Plan(ctx, brief)
		if errors.Is(err, ErrProviderChannelNotConfigured) {
			providerUnavailable = true
			err = nil
		} else if err != nil && !s.cfg.Providers.RepurposeFallbackHeuristic {
			return domain.RepurposePlan{}, fmt.Errorf("run repurpose planner: %w", err)
		}
	}
	if s.planner == nil || providerUnavailable || err != nil {
		draft = repurpose.HeuristicDraft(brief)
		provider, model = "deterministic", "heuristic-v1"
	}

	hits := make(map[string][]domain.ShotSearchResult, len(draft.Sections))
	for _, need := range draft.Sections {
		if strings.TrimSpace(need.Query) == "" {
			continue
		}
		matches, searchErr := s.repo.HybridSearchShots(ctx, need.Query, 10)
		if searchErr != nil {
			return domain.RepurposePlan{}, fmt.Errorf("search shots for %q: %w", need.Query, searchErr)
		}
		hits[need.Query] = matches
	}
	plan := repurpose.ComposePlan(brief, draft, hits, provider, model)
	saved, err := s.repo.SaveRepurposePlan(ctx, plan)
	if err != nil {
		return domain.RepurposePlan{}, err
	}
	if _, err := s.repo.SaveRepurposePlanRevision(ctx, saved, "initial plan"); err != nil {
		return domain.RepurposePlan{}, err
	}
	return saved, nil
}

func (s *Service) GetRepurposePlan(ctx context.Context, id string) (*domain.RepurposePlan, error) {
	return s.repo.GetRepurposePlan(ctx, id)
}

// ReviseRepurposePlan validates a proposed set of sections against the plan's
// current one and records the result as a new draft revision.
//
// The two guards below look like copies of SaveRepurposePlanRevision's own
// checks (internal/repository/sqlite/repository.go) and deliberately are not
// the authority: that one runs at write time and decides, and its error
// already carries the sentinel, so nothing here re-derives it afterwards.
// What these earn is the order the refusals come out in.
//
//   - plan == nil has to be here regardless: every line below dereferences
//     plan.Sections, so this call cannot proceed without a plan at all.
//   - plan.Status == "approved" puts the boundary refusal ahead of the
//     request-shape validation that follows. Without it, revising an approved
//     plan reports whatever the payload trips first -- an empty section list,
//     say -- as ErrInvalidRepurposeRevision's 400 instead of the ErrPlanImmutable
//     409 the operator actually needs: "your request was malformed" instead of
//     "this plan cannot be revised at all, reshaping the payload will not help".
//     It also skips a ShotExists query per candidate on a plan that is going
//     to be refused anyway.
func (s *Service) ReviseRepurposePlan(ctx context.Context, planID string, sections []domain.PlanSection, editorNote string) (domain.RepurposePlanRevision, error) {
	plan, err := s.repo.GetRepurposePlan(ctx, planID)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if plan == nil {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: no plan matches id %s", ErrPlanNotFound, planID)
	}
	if plan.Status == "approved" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s is approved and accepts no further revisions", ErrPlanImmutable, planID)
	}
	if len(sections) == 0 {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: a revision needs at least one section", ErrInvalidRepurposeRevision)
	}
	previousSections := make(map[string]domain.PlanSection, len(plan.Sections))
	for _, section := range plan.Sections {
		previousSections[section.Role] = section
	}
	missing := make([]string, 0)
	for index := range sections {
		section := &sections[index]
		if strings.TrimSpace(section.Role) == "" || strings.TrimSpace(section.Query) == "" || section.DurationMS <= 0 {
			return domain.RepurposePlanRevision{}, fmt.Errorf("%w: invalid plan section", ErrInvalidRepurposeRevision)
		}
		candidateIDs := make(map[string]struct{}, len(section.Candidates))
		for _, candidate := range section.Candidates {
			if candidate.ShotID == "" {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan candidate requires shot_id", ErrInvalidRepurposeRevision)
			}
			candidateIDs[candidate.ShotID] = struct{}{}
			exists, err := s.repo.ShotExists(ctx, candidate.ShotID)
			if err != nil {
				return domain.RepurposePlanRevision{}, err
			}
			if !exists {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan candidate shot not found: %s", ErrInvalidRepurposeRevision, candidate.ShotID)
			}
		}
		if section.SelectedShotID != "" {
			if _, ok := candidateIDs[section.SelectedShotID]; !ok {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: selected shot must belong to section candidates: %s", ErrInvalidRepurposeRevision, section.SelectedShotID)
			}
		}
		excluded := make(map[string]struct{}, len(section.ExcludedShotIDs))
		for _, shotID := range section.ExcludedShotIDs {
			if shotID == "" {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: excluded shot requires shot_id", ErrInvalidRepurposeRevision)
			}
			if _, ok := candidateIDs[shotID]; !ok {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: excluded shot must belong to section candidates: %s", ErrInvalidRepurposeRevision, shotID)
			}
			if _, duplicate := excluded[shotID]; duplicate {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: excluded shot is duplicated: %s", ErrInvalidRepurposeRevision, shotID)
			}
			excluded[shotID] = struct{}{}
		}
		if _, selectedIsExcluded := excluded[section.SelectedShotID]; section.SelectedShotID != "" && selectedIsExcluded {
			return domain.RepurposePlanRevision{}, fmt.Errorf("%w: selected shot cannot be excluded: %s", ErrInvalidRepurposeRevision, section.SelectedShotID)
		}
		previous := previousSections[section.Role]
		if previous.Locked {
			if section.Unlock {
				section.Locked = false
			} else if !section.Locked {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: locked section requires an explicit unlock: %s", ErrInvalidRepurposeRevision, section.Role)
			} else if section.SelectedShotID != previous.SelectedShotID {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: locked section selection cannot change before unlock: %s", ErrInvalidRepurposeRevision, section.Role)
			}
		}
		if section.Locked && section.SelectedShotID == "" {
			return domain.RepurposePlanRevision{}, fmt.Errorf("%w: locked section requires a selected shot: %s", ErrInvalidRepurposeRevision, section.Role)
		}
		section.Unlock = false
		if section.Required && len(section.Candidates) == 0 {
			missing = append(missing, section.Role+": "+section.Query)
		}
	}
	plan.Sections = sections
	plan.MissingNeeds = missing
	// SaveRepurposePlanRevision re-checks both preconditions above inside its
	// own write, and wraps the same sentinels this function does, so a plan
	// that was deleted or approved in the gap since GetRepurposePlan arrives
	// already classified. Nothing here re-derives it from a second read.
	saved, err := s.repo.SaveRepurposePlanRevision(ctx, *plan, editorNote)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	return saved, nil
}

func (s *Service) ListRepurposePlanRevisions(ctx context.Context, planID string) ([]domain.RepurposePlanRevision, error) {
	return s.repo.ListRepurposePlanRevisions(ctx, planID)
}

// ApproveRepurposePlanRevision applies the one precondition the repository
// cannot: that every required section with candidates has had a shot picked.
// "Draft", "latest" and "exists" are enforced by
// ApproveRepurposePlanRevision (internal/repository/sqlite/repository.go)
// inside the transaction that writes the approval, and its errors arrive
// already carrying the matching sentinel, so they are not re-derived here --
// a check made outside that transaction can be raced and would only be a
// second, weaker copy of a rule that is meant to have one.
//
// The revision list this fetches is for the section check, which is why the
// matched == nil guard stays: matched.Plan has to exist before its sections
// can be read. Reading the snapshot outside the transaction is safe in a way
// the state checks are not -- approval only moves a revision's state, never
// its sections, so no concurrent writer can change the answer this check
// computes.
//
// One ordering follows from letting the repository decide and is intended:
// asking to approve a superseded revision that also has an unselected
// required section now reports the selection defect rather than "not the
// latest". Both are true of that revision and both are refusals the caller
// can act on; the alternative was keeping a second, race-prone copy of
// "which revision is latest" purely to choose between two 4xx messages.
func (s *Service) ApproveRepurposePlanRevision(ctx context.Context, planID string, revision int) (domain.RepurposePlanRevision, error) {
	revisions, err := s.repo.ListRepurposePlanRevisions(ctx, planID)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	var matched *domain.RepurposePlanRevision
	for i := range revisions {
		if revisions[i].Revision == revision {
			matched = &revisions[i]
		}
	}
	if matched == nil {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s has no revision %d", ErrPlanRevisionNotFound, planID, revision)
	}
	for _, section := range matched.Plan.Sections {
		if section.Required && len(section.Candidates) > 0 && section.SelectedShotID == "" {
			return domain.RepurposePlanRevision{}, fmt.Errorf("%w: required section needs an explicit selection before approval: %s", ErrInvalidRepurposeRevision, section.Role)
		}
	}
	approved, err := s.repo.ApproveRepurposePlanRevision(ctx, planID, revision)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	return approved, nil
}

func (s *Service) ListAssetCards(ctx context.Context, limit, offset int) ([]domain.AssetCard, error) {
	return s.repo.ListAssetCards(ctx, limit, offset)
}
func (s *Service) ListAssetCardsFiltered(ctx context.Context, filter domain.AssetCardFilter) ([]domain.AssetCard, error) {
	return s.repo.ListAssetCardsFiltered(ctx, filter)
}
func (s *Service) SaveAssetCollection(ctx context.Context, collection domain.AssetCollection) (domain.AssetCollection, error) {
	return s.repo.SaveAssetCollection(ctx, collection)
}
func (s *Service) ListAssetCollections(ctx context.Context) ([]domain.CollectionSummary, error) {
	return s.repo.ListAssetCollections(ctx)
}
func (s *Service) GetAssetCollection(ctx context.Context, id string) (*domain.CollectionSummary, error) {
	return s.repo.GetAssetCollection(ctx, id)
}
func (s *Service) DeleteAssetCollection(ctx context.Context, id string) error {
	return s.repo.DeleteAssetCollection(ctx, id)
}
func (s *Service) ListAssetCardsInCollection(ctx context.Context, collectionID string, limit, offset int) ([]domain.AssetCard, error) {
	return s.repo.ListAssetCardsInCollection(ctx, collectionID, limit, offset)
}
func (s *Service) GetAssetProcessingSummary(ctx context.Context, filter domain.AssetCollectionFilter) (domain.AssetProcessingSummary, error) {
	return s.repo.GetAssetProcessingSummary(ctx, filter)
}
func (s *Service) ListShootSessions(ctx context.Context, filter domain.ShootSessionFilter) ([]domain.ShootSession, error) {
	return s.repo.ListShootSessions(ctx, filter)
}
func (s *Service) GetShootSession(ctx context.Context, id string) (*domain.ShootSession, error) {
	return s.repo.GetShootSession(ctx, id)
}
func (s *Service) GetAssetDetail(ctx context.Context, id string) (*domain.AssetDetail, error) {
	return s.repo.GetAssetDetail(ctx, id)
}
func (s *Service) GetArtifact(ctx context.Context, id, typ string) (*domain.DerivedArtifact, error) {
	return s.repo.GetArtifact(ctx, id, typ)
}

// OriginalMediaPath returns the absolute on-disk path of an asset's primary
// source file, or "" when the asset has no accessible primary location. Used
// by the WebDAV space linker to stream original footage without ever exposing
// the path to the client.
func (s *Service) OriginalMediaPath(ctx context.Context, assetID string) string {
	loc, err := s.repo.GetPrimaryLocation(ctx, assetID)
	if err != nil {
		return ""
	}
	if loc.AbsolutePath == "" {
		return ""
	}
	return loc.AbsolutePath
}

// SetWebDAVSpaceManager wires the on-demand WebDAV delivery manager and its
// persisted account store into the service. Called during startup when the
// feature is enabled; nil manager disables the admin endpoints below.
func (s *Service) SetWebDAVSpaceManager(m *webdavspace.Manager, accounts webdavspace.AccountStore) {
	s.webdav = m
	s.webdavAccounts = accounts
}

// WebDAVLinker adapts the service's asset resolution to the space linker.
type WebDAVLinker struct{ Service *Service }

func (l WebDAVLinker) OriginalPath(ctx context.Context, assetID string) string {
	return l.Service.OriginalMediaPath(ctx, assetID)
}

func (l WebDAVLinker) ProxyPath(ctx context.Context, assetID string) string {
	a, err := l.Service.GetArtifact(ctx, assetID, "proxy")
	if err != nil || a == nil {
		return ""
	}
	return a.LocalPath
}

// CreateWebDAVAccount hashes the plaintext password with bcrypt (via the
// manager's account store) and persists it. Plaintext never enters the
// repository.
// ErrWebDAVAccountInvalid reports an empty username or password on account
// creation. Mapped to 400 by the API layer.
var ErrWebDAVAccountInvalid = errors.New("WebDAV username and password are required")

// ErrWebDAVAccountExists reports a duplicate WebDAV account username. Mapped
// to 409 by the API layer; password rotation deletes first, then recreates.
var ErrWebDAVAccountExists = errors.New("WebDAV account already exists")

// ErrWebDAVSpaceNotFound reports an unknown WebDAV space id on a link
// request. Mapped to 404 by the API layer.
var ErrWebDAVSpaceNotFound = errors.New("unknown WebDAV space")

// ErrWebDAVLinkKindInvalid reports a link kind other than original|proxy.
// Mapped to 400 by the API layer.
var ErrWebDAVLinkKindInvalid = errors.New("unknown link kind (want original or proxy)")

// ErrPairingTokenInvalid reports an unrecognised or already-redeemed pairing
// token on worker enrollment. Mapped to 401 by the API layer.
var ErrPairingTokenInvalid = errors.New("pairing token is invalid or already redeemed")

func (s *Service) CreateWebDAVAccount(ctx context.Context, username, password string) error {
	if s.webdavAccounts == nil {
		return errors.New("WebDAV delivery is not enabled")
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return ErrWebDAVAccountInvalid
	}
	// Reject duplicates up front so the operator gets a clear 409 instead of
	// an opaque insert failure (or a silent overwrite). Password rotation is
	// delete-then-recreate.
	if _, exists, err := s.webdavAccounts.GetAccount(ctx, username); err != nil {
		return err
	} else if exists {
		return ErrWebDAVAccountExists
	}
	// The repository-backed store only persists; hashing is done here so the
	// plaintext never crosses into the repository layer.
	hash, err := webdavspace.HashPassword(password)
	if err != nil {
		return err
	}
	repoStore, ok := s.webdavAccounts.(sqlite.WebDAVAccountStore)
	if !ok {
		return errors.New("WebDAV account store is not repository-backed")
	}
	return repoStore.Repo.SaveWebDAVAccount(ctx, username, hash)
}

// CreateWebDAVSpace registers a new on-demand delivery space and returns its
// id. The space starts empty; assets appear only when linked.
func (s *Service) CreateWebDAVSpace(ctx context.Context) (string, error) {
	if s.webdav == nil {
		return "", errors.New("WebDAV delivery is not enabled")
	}
	space := s.webdav.CreateSpace(idgen.New())
	return space.ID, nil
}

// LinkWebDAVAsset adds the asset's original media or proxy artifact to the
// space as a virtual entry, returning the WebDAV path it will be served at.
func (s *Service) LinkWebDAVAsset(ctx context.Context, spaceID, assetID, kind string) (string, error) {
	if s.webdav == nil {
		return "", errors.New("WebDAV delivery is not enabled")
	}
	if strings.TrimSpace(assetID) == "" {
		return "", errors.New("asset_id is required")
	}
	space := s.webdav.Space(spaceID)
	if space == nil {
		return "", ErrWebDAVSpaceNotFound
	}
	switch kind {
	case "original":
		return space.LinkOriginal(ctx, assetID)
	case "proxy":
		return space.LinkProxy(ctx, assetID)
	default:
		return "", ErrWebDAVLinkKindInvalid
	}
}

// ListWebDAVSpaces returns the ids of all live delivery spaces.
func (s *Service) ListWebDAVSpaces() []string {
	if s.webdav == nil {
		return nil
	}
	return s.webdav.SpaceIDs()
}

// ListWebDAVAccounts returns every delivery account username.
func (s *Service) ListWebDAVAccounts(ctx context.Context) ([]string, error) {
	repoStore, ok := s.webdavAccounts.(sqlite.WebDAVAccountStore)
	if !ok {
		return nil, errors.New("WebDAV account store is not repository-backed")
	}
	return repoStore.Repo.ListWebDAVAccounts(ctx)
}

// RevokeWebDAVSpace removes a delivery space and all its linked assets.
// Subsequent reads of the space 404.
func (s *Service) RevokeWebDAVSpace(_ context.Context, id string) error {
	if s.webdav == nil {
		return errors.New("WebDAV delivery is not enabled")
	}
	s.webdav.Revoke(id)
	return nil
}

// DeleteWebDAVAccount removes a delivery account.
func (s *Service) DeleteWebDAVAccount(ctx context.Context, username string) error {
	repoStore, ok := s.webdavAccounts.(sqlite.WebDAVAccountStore)
	if !ok {
		return errors.New("WebDAV account store is not repository-backed")
	}
	return repoStore.Repo.DeleteWebDAVAccount(ctx, username)
}

func (s *Service) ListCanonicalTags(ctx context.Context) ([]domain.CanonicalTag, error) {
	return s.repo.ListCanonicalTags(ctx)
}
func (s *Service) ListUnresolvedTags(ctx context.Context, limit int) ([]domain.UnresolvedTag, error) {
	return s.repo.ListUnresolvedTags(ctx, limit)
}
func (s *Service) RunTagCurator(ctx context.Context, limit int) (domain.TagCurationResult, error) {
	unresolved, err := s.repo.ListUnresolvedTags(ctx, limit)
	if err != nil {
		return domain.TagCurationResult{}, err
	}
	existing, err := s.repo.ListCanonicalTags(ctx)
	if err != nil {
		return domain.TagCurationResult{}, err
	}
	strategy := "heuristic-v1"
	var proposals []domain.TagProposal
	providerUnavailable := false
	if s.curator != nil {
		proposals, err = s.curator.Curate(ctx, unresolved, existing)
		if errors.Is(err, ErrProviderChannelNotConfigured) {
			providerUnavailable = true
			err = nil
		} else if err == nil {
			strategy = s.curator.Name() + ":" + s.curator.Model()
		} else if !s.cfg.Providers.TagCuratorFallbackHeuristic {
			return domain.TagCurationResult{}, fmt.Errorf("run tag curator: %w", err)
		}
	}
	if s.curator == nil || providerUnavailable || err != nil {
		proposals = curator.BuildProposals(unresolved, existing)
	}
	return s.repo.CreateTagCurationRun(ctx, proposals, len(unresolved), strategy)
}
func (s *Service) ListTagProposals(ctx context.Context, state string, limit int) ([]domain.TagProposal, error) {
	return s.repo.ListTagProposals(ctx, state, limit)
}
func (s *Service) ReviewTagProposal(ctx context.Context, id, action, note string) error {
	if action != "approve" && action != "reject" {
		return fmt.Errorf("unsupported review action: %s", action)
	}
	return s.repo.ReviewTagProposal(ctx, id, action, note)
}

func (s *Service) LatestLibrarySummary(ctx context.Context) (*domain.LibrarySummary, error) {
	return s.repo.LatestLibrarySummary(ctx)
}
