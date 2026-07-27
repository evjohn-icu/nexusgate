package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/credentials"
	"github.com/ev/timingdex/internal/curator"
	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/hubauth"
	"github.com/ev/timingdex/internal/idgen"
	"github.com/ev/timingdex/internal/ingest"
	"github.com/ev/timingdex/internal/media"
	"github.com/ev/timingdex/internal/providers"
	"github.com/ev/timingdex/internal/remote"
	"github.com/ev/timingdex/internal/repurpose"
	"github.com/ev/timingdex/internal/secretstore"
	"github.com/ev/timingdex/internal/staging"
)

var ErrInvalidRepurposeRevision = errors.New("invalid repurpose revision")
var ErrInvalidWorkerArtifact = errors.New("invalid worker artifact")
var ErrWorkerArtifactLease = errors.New("worker does not own active job")
var ErrWorkerProviderCredentialDeliveryDisabled = errors.New("worker provider credential delivery is disabled")

type Repository interface {
	PipelineRepository
	CreateLibraryRoot(ctx context.Context, path string) (domain.LibraryRoot, error)
	ListLibraryRoots(ctx context.Context) ([]domain.LibraryRoot, error)
	GetLibraryRoot(ctx context.Context, id string) (domain.LibraryRoot, error)
	ingest.ScanRepository
	ListAssets(ctx context.Context, limit, offset int) ([]domain.Asset, error)
	ListAssetCards(ctx context.Context, limit, offset int) ([]domain.AssetCard, error)
	ListAssetCardsFiltered(ctx context.Context, filter domain.AssetCardFilter) ([]domain.AssetCard, error)
	SaveAssetCollection(ctx context.Context, collection domain.AssetCollection) (domain.AssetCollection, error)
	ListAssetCollections(ctx context.Context) ([]domain.AssetCollection, error)
	GetAssetCollection(ctx context.Context, id string) (*domain.AssetCollection, error)
	DeleteAssetCollection(ctx context.Context, id string) error
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
	ListAssetShots(context.Context, string) ([]domain.AssetShot, error)
	ShotExists(context.Context, string) (bool, error)
	SearchShots(context.Context, string, int) ([]domain.ShotSearchResult, error)
	HybridSearchShots(context.Context, string, int) ([]domain.ShotSearchResult, error)
	SimilarShots(context.Context, string, int) ([]domain.ShotSearchResult, error)
	DiscoverRareShots(context.Context, int) ([]domain.RareShot, error)
	SaveRepurposePlan(context.Context, domain.RepurposePlan) (domain.RepurposePlan, error)
	GetRepurposePlan(context.Context, string) (*domain.RepurposePlan, error)
	SaveRepurposePlanRevision(context.Context, domain.RepurposePlan, string) (domain.RepurposePlanRevision, error)
	ListRepurposePlanRevisions(context.Context, string) ([]domain.RepurposePlanRevision, error)
	ApproveRepurposePlanRevision(context.Context, string, int) (domain.RepurposePlanRevision, error)
	UpsertProviderChannel(context.Context, domain.ProviderChannel) (domain.ProviderChannel, error)
	ListProviderChannels(context.Context, string) ([]domain.ProviderChannel, error)
	SoftDeleteProviderChannel(context.Context, string) error
	RebuildAutomaticShootSessions(context.Context, string) error
	CreateWorkerPairing(context.Context, time.Duration) (remote.PairingToken, error)
	EnrollWorker(context.Context, string, remote.WorkerRegistration) (remote.Worker, string, error)
	AuthenticateWorker(context.Context, string) (remote.Worker, error)
	HeartbeatWorker(context.Context, string, remote.WorkerCapabilities) error
	ListWorkers(context.Context) ([]remote.Worker, error)
	LeaseNextWorkerDerive(context.Context, remote.Worker, time.Duration) (*remote.WorkerJob, error)
	CompleteWorkerJob(context.Context, string, string, domain.JobState, string) error
	RecordWorkerJobProgress(context.Context, string, string, string, float64, string, string) error
	GetWorkerJobStatus(context.Context, string) (remote.WorkerJobStatus, error)
	SetDeriveWorkerAssignment(context.Context, string, string, remote.WorkerAssignmentMode) error
	PrepareWorkerArtifact(context.Context, string, string, string, string) (string, *domain.DerivedArtifact, error)
	CommitWorkerArtifact(context.Context, string, string, domain.DerivedArtifact, bool) (domain.DerivedArtifact, bool, error)
	RecordProviderCredentialLease(context.Context, string, string, string, string, time.Time) error
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
	secrets    *secretstore.Store

	pipelineMu      sync.Mutex
	pipelineRunning bool
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
	secrets, err := secretstore.Open(cfg.DataDir, adminToken)
	if err != nil {
		return nil, fmt.Errorf("initialize Hub provider secret store: %w", err)
	}
	channelRuntime := newProviderChannelRuntime(repo, cfg, secrets, asr, fallback, videoProvider, tagCurator, embedder, planner)
	return &Service{
		repo: repo, cfg: cfg, scanner: ingest.NewScanner(repo),
		pipeline: NewPipeline(repo, cfg.CacheDir, channelRuntime.asr(), channelRuntime.asrFallback(), channelRuntime.video(), alignment, plan, sourceStager),
		curator:  channelRuntime.curator(), embedder: channelRuntime.embedder(), planner: channelRuntime.planner(),
		hardware: hardware, adminToken: adminToken, secrets: secrets,
	}, nil
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

// SaveProviderChannel stores API keys only in the Hub secret store.
// memberKeys is positional, not keyed by label: memberKeys[i] corresponds to
// channel.Members[i]. A channel intentionally allows multiple members with
// the same label (Weight/MaxInflight exist precisely to let one provider be
// configured with several keys), so indexing by label would let one key
// silently overwrite or misassign to another member's secret. An empty or
// missing entry preserves the member's existing secret; the slice may be
// shorter than Members, in which case the missing tail is treated as empty.
func (s *Service) SaveProviderChannel(ctx context.Context, channel domain.ProviderChannel, memberKeys []string) (domain.ProviderChannel, error) {
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

func (s *Service) AddLibraryRoot(ctx context.Context, path string) (domain.LibraryRoot, error) {
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

func (s *Service) ListLibraryRoots(ctx context.Context) ([]domain.LibraryRoot, error) {
	return s.repo.ListLibraryRoots(ctx)
}

func (s *Service) CreateWorkerPairing(ctx context.Context, ttl time.Duration) (remote.PairingToken, error) {
	return s.repo.CreateWorkerPairing(ctx, ttl)
}

func (s *Service) EnrollWorker(ctx context.Context, pairingToken string, registration remote.WorkerRegistration) (remote.Worker, string, error) {
	return s.repo.EnrollWorker(ctx, pairingToken, registration)
}

func (s *Service) AuthenticateWorker(ctx context.Context, token string) (remote.Worker, error) {
	return s.repo.AuthenticateWorker(ctx, token)
}

func (s *Service) HeartbeatWorker(ctx context.Context, workerID string, capabilities remote.WorkerCapabilities) error {
	return s.repo.HeartbeatWorker(ctx, workerID, capabilities)
}

func (s *Service) ListWorkers(ctx context.Context) ([]remote.Worker, error) {
	return s.repo.ListWorkers(ctx)
}

func (s *Service) LeaseNextWorkerDerive(ctx context.Context, worker remote.Worker) (*remote.WorkerJob, error) {
	return s.repo.LeaseNextWorkerDerive(ctx, worker, 2*time.Minute)
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
		return credentials.Lease{}, err
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
		if strings.Contains(err.Error(), "worker does not own active job") {
			return domain.DerivedArtifact{}, false, fmt.Errorf("%w: %v", ErrWorkerArtifactLease, err)
		}
		return domain.DerivedArtifact{}, false, err
	}
	preserveExisting := false
	if existing != nil {
		if info, statErr := os.Stat(existing.LocalPath); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
			preserveExisting = true
			stored, reused, commitErr := s.repo.CommitWorkerArtifact(ctx, jobID, workerID, *existing, true)
			if commitErr != nil {
				if strings.Contains(commitErr.Error(), "worker does not own active job") {
					return domain.DerivedArtifact{}, false, fmt.Errorf("%w: %v", ErrWorkerArtifactLease, commitErr)
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
		if strings.Contains(err.Error(), "worker does not own active job") {
			return domain.DerivedArtifact{}, false, fmt.Errorf("%w: %v", ErrWorkerArtifactLease, err)
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
	root, err := s.repo.GetLibraryRoot(ctx, rootID)
	if err != nil {
		return domain.ScanResult{}, err
	}
	result, err := s.scanner.Scan(ctx, root)
	if err != nil {
		return result, err
	}
	assets, listErr := s.repo.ListAssets(ctx, 100000, 0)
	if listErr != nil {
		return result, listErr
	}
	for _, asset := range assets {
		if asset.State != domain.AssetMissing {
			_ = s.pipeline.EnqueueAsset(ctx, asset.ID)
		}
	}
	return result, nil
}

func (s *Service) ListAssets(ctx context.Context, limit, offset int) ([]domain.Asset, error) {
	return s.repo.ListAssets(ctx, limit, offset)
}

func (s *Service) Doctor(ctx context.Context, writer io.Writer) error {
	for _, binary := range []string{"ffmpeg", "ffprobe", "exiftool"} {
		path, err := exec.LookPath(binary)
		if err != nil {
			fmt.Fprintf(writer, "%s: missing\n", binary)
			continue
		}
		fmt.Fprintf(writer, "%s: %s\n", binary, path)
	}
	report := s.HardwareReport()
	fmt.Fprintf(writer, "hardware acceleration: requested=%s selected=%s fallback=%t\n", report.RequestedMode, report.SelectedMode, report.Fallback)
	for _, cap := range report.Capabilities {
		fmt.Fprintf(writer, "  %s: decode=%t encode=%t runtime=%t selected=%t\n", cap.Backend, cap.DecodeAvailable, cap.EncodeAvailable, cap.RuntimeAvailable, cap.Selected)
	}
	if report.Warning != "" {
		fmt.Fprintf(writer, "  warning: %s\n", report.Warning)
	}
	return nil
}

func (s *Service) HardwareReport() media.HardwareReport { return s.hardware }

func (s *Service) RunPipeline(ctx context.Context) error {
	if err := s.pipeline.RunUntilIdle(ctx); err != nil {
		return err
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
	s.pipelineMu.Lock()
	if s.pipelineRunning {
		s.pipelineMu.Unlock()
		return false
	}
	s.pipelineRunning = true
	s.pipelineMu.Unlock()

	go func() {
		defer func() {
			s.pipelineMu.Lock()
			s.pipelineRunning = false
			s.pipelineMu.Unlock()
		}()
		if err := s.RunPipeline(context.Background()); err != nil {
			slog.Error("pipeline run failed", "error", err)
		}
	}()
	return true
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
func (s *Service) Search(ctx context.Context, q string, limit int) ([]string, error) {
	return s.pipeline.repo.Search(ctx, q, limit)
}

func (s *Service) ListAssetShots(ctx context.Context, assetID string) ([]domain.AssetShot, error) {
	return s.repo.ListAssetShots(ctx, assetID)
}

func (s *Service) SearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	return s.repo.SearchShots(ctx, q, limit)
}

func (s *Service) HybridSearchShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	return s.repo.HybridSearchShots(ctx, q, limit)
}

func (s *Service) SimilarShots(ctx context.Context, shotID string, limit int) ([]domain.ShotSearchResult, error) {
	return s.repo.SimilarShots(ctx, shotID, limit)
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

func (s *Service) ReviseRepurposePlan(ctx context.Context, planID string, sections []domain.PlanSection, editorNote string) (domain.RepurposePlanRevision, error) {
	plan, err := s.repo.GetRepurposePlan(ctx, planID)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if plan == nil {
		return domain.RepurposePlanRevision{}, fmt.Errorf("repurpose plan not found: %s", planID)
	}
	if plan.Status == "approved" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("approved repurpose plan is immutable: %s", planID)
	}
	if len(sections) == 0 {
		return domain.RepurposePlanRevision{}, fmt.Errorf("at least one plan section is required")
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
	return s.repo.SaveRepurposePlanRevision(ctx, *plan, editorNote)
}

func (s *Service) ListRepurposePlanRevisions(ctx context.Context, planID string) ([]domain.RepurposePlanRevision, error) {
	return s.repo.ListRepurposePlanRevisions(ctx, planID)
}

func (s *Service) ApproveRepurposePlanRevision(ctx context.Context, planID string, revision int) (domain.RepurposePlanRevision, error) {
	revisions, err := s.repo.ListRepurposePlanRevisions(ctx, planID)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	for _, candidate := range revisions {
		if candidate.Revision != revision {
			continue
		}
		for _, section := range candidate.Plan.Sections {
			if section.Required && len(section.Candidates) > 0 && section.SelectedShotID == "" {
				return domain.RepurposePlanRevision{}, fmt.Errorf("%w: required section needs an explicit selection before approval: %s", ErrInvalidRepurposeRevision, section.Role)
			}
		}
		break
	}
	return s.repo.ApproveRepurposePlanRevision(ctx, planID, revision)
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
func (s *Service) ListAssetCollections(ctx context.Context) ([]domain.AssetCollection, error) {
	return s.repo.ListAssetCollections(ctx)
}
func (s *Service) GetAssetCollection(ctx context.Context, id string) (*domain.AssetCollection, error) {
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
