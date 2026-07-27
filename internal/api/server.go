package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ev/timingdex/internal/app"
	"github.com/ev/timingdex/internal/credentials"
	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/remote"
)

type Server struct {
	address string
	service *app.Service
	tlsCert string
	tlsKey  string
}

func NewServer(address string, service *app.Service) *Server {
	return &Server{address: address, service: service}
}

func NewTLSServer(address string, service *app.Service, certificateFile, keyFile string) *Server {
	return &Server{address: address, service: service, tlsCert: certificateFile, tlsKey: keyFile}
}

func (s *Server) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.address,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("local API started", "address", s.address)
		if s.tlsCert != "" && s.tlsKey != "" {
			errCh <- server.ListenAndServeTLS(s.tlsCert, s.tlsKey)
			return
		}
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/hardware", s.hardwareReport)
	mux.HandleFunc("GET /api/v1/agent/capabilities", s.agentCapabilities)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/roots", s.requireHubAdmin(s.listRoots))
	mux.HandleFunc("POST /api/v1/hub/worker-pairings", s.requireHubAdmin(s.createWorkerPairing))
	mux.HandleFunc("GET /api/v1/hub/workers", s.requireHubAdmin(s.listWorkers))
	mux.HandleFunc("GET /api/v1/admin/provider-channels", s.requireHubAdmin(s.listProviderChannels))
	mux.HandleFunc("POST /api/v1/admin/provider-channels", s.requireHubAdmin(s.saveProviderChannel))
	mux.HandleFunc("PATCH /api/v1/admin/provider-channels/{id}", s.requireHubAdmin(s.updateProviderChannel))
	mux.HandleFunc("POST /api/v1/admin/provider-channels/{id}/enable", s.requireHubAdmin(s.enableProviderChannel))
	mux.HandleFunc("POST /api/v1/admin/provider-channels/{id}/disable", s.requireHubAdmin(s.disableProviderChannel))
	mux.HandleFunc("DELETE /api/v1/admin/provider-channels/{id}", s.requireHubAdmin(s.deleteProviderChannel))
	mux.HandleFunc("POST /api/v1/admin/provider-channels/{id}/test", s.requireHubAdmin(s.testProviderChannel))
	mux.HandleFunc("GET /api/v1/admin/assets/{id}/capture-location", s.requireHubAdmin(s.assetCaptureLocation))
	mux.HandleFunc("POST /api/v1/worker/enroll", s.enrollWorker)
	mux.HandleFunc("POST /api/v1/worker/heartbeat", s.workerHeartbeat)
	mux.HandleFunc("POST /api/v1/worker/lease", s.workerLease)
	mux.HandleFunc("POST /api/v1/worker/jobs/{id}/complete", s.workerCompleteJob)
	mux.HandleFunc("POST /api/v1/worker/jobs/{id}/progress", s.workerProgress)
	mux.HandleFunc("POST /api/v1/worker/jobs/{id}/credentials/{operation}", s.workerCredential)
	mux.HandleFunc("POST /api/v1/worker/jobs/{id}/provider/{operation}", s.workerProviderProxy)
	mux.HandleFunc("POST /api/v1/worker/jobs/{id}/artifacts", s.workerUploadArtifactMultipart)
	mux.HandleFunc("PUT /api/v1/worker/jobs/{id}/artifacts/{type}", s.workerUploadArtifactRaw)
	mux.HandleFunc("POST /api/v1/roots", s.requireHubAdmin(s.createRoot))
	mux.HandleFunc("POST /api/v1/roots/{id}/scan", s.requireHubAdmin(s.scanRoot))
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("GET /progress", s.progressPage)
	mux.HandleFunc("GET /workers", s.workersPage)
	mux.HandleFunc("GET /repurpose", s.repurposePage)
	mux.HandleFunc("GET /tags", s.tagsPage)
	mux.HandleFunc("GET /providers", s.providersPage)
	mux.HandleFunc("GET /api/v1/assets", s.listAssetCards)
	mux.HandleFunc("GET /api/v1/library/processing-summary", s.processingSummary)
	mux.HandleFunc("GET /api/v1/collections", s.listCollections)
	mux.HandleFunc("GET /api/v1/collections/{id}", s.getCollection)
	mux.HandleFunc("GET /api/v1/collections/{id}/assets", s.listCollectionAssets)
	mux.HandleFunc("POST /api/v1/collections", s.requireHubAdmin(s.saveCollection))
	mux.HandleFunc("DELETE /api/v1/collections/{id}", s.requireHubAdmin(s.deleteCollection))
	mux.HandleFunc("GET /api/v1/shoot-sessions", s.listShootSessions)
	mux.HandleFunc("GET /api/v1/assets/{id}", s.assetDetail)
	mux.HandleFunc("GET /api/v1/assets/{id}/shots", s.assetShots)
	mux.HandleFunc("GET /api/v1/assets/{id}/thumbnail", s.assetThumbnail)
	mux.HandleFunc("GET /api/v1/assets/{id}/proxy", s.assetProxy)
	mux.HandleFunc("GET /api/v1/jobs", s.requireHubAdmin(s.listJobs))
	mux.HandleFunc("GET /api/v1/admin/worker-jobs/{id}", s.requireHubAdmin(s.workerJobStatus))
	mux.HandleFunc("POST /api/v1/admin/worker-jobs/{id}/assignment", s.requireHubAdmin(s.setWorkerJobAssignment))
	mux.HandleFunc("POST /api/v1/pipeline/run", s.requireHubAdmin(s.runPipeline))
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/search/shots", s.searchShots)
	mux.HandleFunc("GET /api/v1/search/shots/hybrid", s.hybridSearchShots)
	mux.HandleFunc("GET /api/v1/shots/{id}/similar", s.similarShots)
	mux.HandleFunc("GET /api/v1/discover/rare-shots", s.rareShots)
	mux.HandleFunc("GET /api/v1/tags", s.listTags)
	mux.HandleFunc("GET /api/v1/tags/unresolved", s.listUnresolvedTags)
	mux.HandleFunc("POST /api/v1/tags/curate", s.requireHubAdmin(s.runTagCurator))
	mux.HandleFunc("POST /api/v1/tags/clusters", s.requireHubAdmin(s.runTagEmbeddingClusters))
	mux.HandleFunc("GET /api/v1/tags/proposals", s.listTagProposals)
	mux.HandleFunc("POST /api/v1/tags/proposals/{id}/review", s.requireHubAdmin(s.reviewTagProposal))
	mux.HandleFunc("GET /api/v1/library/summary", s.latestLibrarySummary)
	mux.HandleFunc("POST /api/v1/library/summary/generate", s.requireHubAdmin(s.generateLibrarySummary))
	mux.HandleFunc("POST /api/v1/repurpose/plans", s.requireHubAdmin(s.createRepurposePlan))
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}", s.getRepurposePlan)
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}/revisions", s.listRepurposePlanRevisions)
	mux.HandleFunc("POST /api/v1/repurpose/plans/{id}/revisions", s.requireHubAdmin(s.reviseRepurposePlan))
	mux.HandleFunc("POST /api/v1/repurpose/plans/{id}/revisions/{revision}/approve", s.requireHubAdmin(s.approveRepurposePlanRevision))

	return requestLogger(mux)
}

func (s *Server) requireHubAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const scheme = "Bearer "
		value := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(value, scheme) {
			http.Error(w, "Hub administrator authentication required", http.StatusUnauthorized)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(value, scheme))
		expected := s.service.AdminToken()
		if provided == "" || expected == "" || len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			http.Error(w, "Hub administrator authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) createWorkerPairing(w http.ResponseWriter, r *http.Request) {
	pairing, err := s.service.CreateWorkerPairing(r.Context(), 15*time.Minute)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pairing)
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.service.ListWorkers(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if workers == nil {
		workers = []remote.Worker{}
	}
	writeJSON(w, http.StatusOK, workers)
}

func (s *Server) listProviderChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.service.ListProviderChannels(r.Context(), r.URL.Query().Get("capability"))
	if err != nil {
		writeError(w, err)
		return
	}
	if channels == nil {
		channels = []domain.ProviderChannel{}
	}
	writeJSON(w, http.StatusOK, channels)
}

func (s *Server) saveProviderChannel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID           string `json:"id"`
		Capability   string `json:"capability"`
		Label        string `json:"label"`
		ProviderName string `json:"provider_name"`
		Protocol     string `json:"protocol"`
		Endpoint     string `json:"endpoint"`
		Model        string `json:"model"`
		Enabled      bool   `json:"enabled"`
		RouteOrder   int    `json:"route_order"`
		Members      []struct {
			ID          string `json:"id"`
			Label       string `json:"label"`
			APIKey      string `json:"api_key"`
			Enabled     bool   `json:"enabled"`
			Weight      int    `json:"weight"`
			MaxInflight int    `json:"max_inflight"`
		} `json:"members"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		http.Error(w, "invalid provider channel", http.StatusBadRequest)
		return
	}
	channel := domain.ProviderChannel{ID: input.ID, Capability: strings.TrimSpace(input.Capability), Label: strings.TrimSpace(input.Label), ProviderName: strings.TrimSpace(input.ProviderName), Protocol: strings.TrimSpace(input.Protocol), Endpoint: strings.TrimSpace(input.Endpoint), Model: strings.TrimSpace(input.Model), Enabled: input.Enabled, RouteOrder: input.RouteOrder}
	// keys is positional, aligned with channel.Members below: a channel
	// allows several members sharing the same label (Weight/MaxInflight
	// exist so one provider can be configured with multiple keys), so a
	// label-keyed map here would let one input silently clobber or
	// misassign another member's key.
	keys := make([]string, 0, len(input.Members))
	for _, member := range input.Members {
		channel.Members = append(channel.Members, domain.ProviderChannelMember{ID: member.ID, Label: strings.TrimSpace(member.Label), Enabled: member.Enabled, Weight: member.Weight, MaxInflight: member.MaxInflight})
		keys = append(keys, strings.TrimSpace(member.APIKey))
	}
	saved, err := s.service.SaveProviderChannel(r.Context(), channel, keys)
	if err != nil {
		http.Error(w, "provider channel rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Match the read contract: neither a key nor a secret reference belongs in
	// a browser response, including immediately after a successful write.
	for i := range saved.Members {
		saved.Members[i].SecretReady = false
		saved.Members[i].SecretRef = ""
	}
	channels, err := s.service.ListProviderChannels(r.Context(), "")
	if err == nil {
		for _, candidate := range channels {
			if candidate.ID == saved.ID {
				saved = candidate
				break
			}
		}
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) updateProviderChannel(w http.ResponseWriter, r *http.Request) {
	var patch app.ProviderChannelUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
		http.Error(w, "invalid provider channel update", http.StatusBadRequest)
		return
	}
	updated, err := s.service.UpdateProviderChannel(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		http.Error(w, "provider channel update rejected", http.StatusBadRequest)
		return
	}
	s.writeProviderChannel(w, r, http.StatusOK, updated)
}

func (s *Server) enableProviderChannel(w http.ResponseWriter, r *http.Request) {
	updated, err := s.service.SetProviderChannelEnabled(r.Context(), r.PathValue("id"), true)
	if err != nil {
		http.Error(w, "provider channel enable rejected", http.StatusBadRequest)
		return
	}
	s.writeProviderChannel(w, r, http.StatusOK, updated)
}

func (s *Server) disableProviderChannel(w http.ResponseWriter, r *http.Request) {
	updated, err := s.service.SetProviderChannelEnabled(r.Context(), r.PathValue("id"), false)
	if err != nil {
		http.Error(w, "provider channel disable rejected", http.StatusBadRequest)
		return
	}
	s.writeProviderChannel(w, r, http.StatusOK, updated)
}

func (s *Server) deleteProviderChannel(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteProviderChannel(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "provider channel delete rejected", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testProviderChannel(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.TestProviderChannel(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "provider channel test rejected", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) writeProviderChannel(w http.ResponseWriter, r *http.Request, status int, saved domain.ProviderChannel) {
	channels, err := s.service.ListProviderChannels(r.Context(), saved.Capability)
	if err == nil {
		for _, candidate := range channels {
			if candidate.ID == saved.ID {
				writeJSON(w, status, candidate)
				return
			}
		}
	}
	for i := range saved.Members {
		saved.Members[i].SecretRef = ""
		saved.Members[i].SecretReady = false
	}
	writeJSON(w, status, saved)
}

func (s *Server) enrollWorker(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PairingToken string `json:"pairing_token"`
		remote.WorkerRegistration
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.PairingToken) == "" {
		http.Error(w, "invalid worker enrollment", http.StatusBadRequest)
		return
	}
	worker, token, err := s.service.EnrollWorker(r.Context(), request.PairingToken, request.WorkerRegistration)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"worker": worker, "token": token})
}

func (s *Server) workerHeartbeat(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	var request struct {
		Capabilities remote.WorkerCapabilities `json:"capabilities"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid worker heartbeat", http.StatusBadRequest)
		return
	}
	if err := s.service.HeartbeatWorker(r.Context(), worker.ID, request.Capabilities); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerLease(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	job, err := s.service.LeaseNextWorkerDerive(r.Context(), worker)
	if err != nil {
		writeError(w, err)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) workerCompleteJob(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	var request struct {
		State   domain.JobState `json:"state"`
		Message string          `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid worker job completion", http.StatusBadRequest)
		return
	}
	if err := s.service.CompleteWorkerJob(r.Context(), r.PathValue("id"), worker.ID, request.State, request.Message); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerProgress(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	var request struct {
		Stage    string  `json:"stage"`
		Progress float64 `json:"progress"`
		Event    string  `json:"event"`
		Message  string  `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil {
		http.Error(w, "invalid worker job progress", http.StatusBadRequest)
		return
	}
	if err := s.service.RecordWorkerJobProgress(r.Context(), r.PathValue("id"), worker.ID, strings.TrimSpace(request.Stage), request.Progress, strings.TrimSpace(request.Event), strings.TrimSpace(request.Message)); err != nil {
		if strings.Contains(err.Error(), "does not own active job") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "worker progress rejected", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerCredential(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	operation := credentials.Operation(strings.TrimSpace(r.PathValue("operation")))
	lease, err := s.service.IssueWorkerCredential(r.Context(), worker, r.PathValue("id"), operation)
	if err != nil {
		if errors.Is(err, app.ErrWorkerProviderCredentialDeliveryDisabled) {
			http.Error(w, "worker provider credential delivery is disabled", http.StatusForbidden)
			return
		}
		if strings.Contains(err.Error(), "does not own active job") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "credential request rejected", http.StatusBadRequest)
		return
	}
	// Deliberately do not log the lease or any response fields here: it carries
	// an in-memory API key for the authenticated Worker.
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) workerProviderProxy(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		http.Error(w, "provider proxy accepts application/json only", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > app.MaxProviderProxyBodyBytes() {
		http.Error(w, "provider proxy request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, app.MaxProviderProxyBodyBytes()+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "provider proxy request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	if int64(len(body)) > app.MaxProviderProxyBodyBytes() || !json.Valid(body) {
		http.Error(w, "provider proxy accepts valid JSON up to 2 MiB", http.StatusBadRequest)
		return
	}
	operation := credentials.Operation(strings.TrimSpace(r.PathValue("operation")))
	result, err := s.service.ProxyWorkerProviderJSON(r.Context(), worker, r.PathValue("id"), operation, body)
	if err != nil {
		if errors.Is(err, app.ErrProviderProxyRequest) {
			http.Error(w, "provider proxy request failed", http.StatusBadGateway)
			return
		}
		if strings.Contains(err.Error(), "does not own active job") || strings.Contains(err.Error(), "credential operation") {
			http.Error(w, "worker does not own active job for provider proxy", http.StatusConflict)
			return
		}
		http.Error(w, "provider proxy request rejected", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}

type workerArtifactResponse struct {
	ID           string `json:"id"`
	AssetID      string `json:"asset_id"`
	ArtifactType string `json:"artifact_type"`
	ProfileHash  string `json:"profile_hash"`
	SizeBytes    int64  `json:"size_bytes"`
}

func (s *Server) workerUploadArtifactMultipart(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid artifact multipart body", http.StatusBadRequest)
		return
	}
	artifactType := strings.TrimSpace(r.FormValue("type"))
	profileHash := strings.TrimSpace(r.FormValue("profile_hash"))
	file, header, err := r.FormFile("artifact")
	if err != nil {
		http.Error(w, "artifact file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	contentType := ""
	if header != nil {
		contentType = header.Header.Get("Content-Type")
	}
	artifact, reused, err := s.service.UploadWorkerArtifact(r.Context(), r.PathValue("id"), worker.ID, artifactType, profileHash, contentType, file)
	s.writeWorkerArtifactResult(w, artifact, reused, err)
}

func (s *Server) workerUploadArtifactRaw(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	profileHash := strings.TrimSpace(r.URL.Query().Get("profile_hash"))
	if profileHash == "" {
		profileHash = strings.TrimSpace(r.URL.Query().Get("profile"))
	}
	if profileHash == "" {
		profileHash = strings.TrimSpace(r.Header.Get("X-Artifact-Profile"))
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30+1<<20)
	artifact, reused, err := s.service.UploadWorkerArtifact(r.Context(), r.PathValue("id"), worker.ID, strings.TrimSpace(r.PathValue("type")), profileHash, r.Header.Get("Content-Type"), r.Body)
	s.writeWorkerArtifactResult(w, artifact, reused, err)
}

func (s *Server) writeWorkerArtifactResult(w http.ResponseWriter, artifact domain.DerivedArtifact, reused bool, err error) {
	if err != nil {
		switch {
		case errors.Is(err, app.ErrInvalidWorkerArtifact):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, app.ErrWorkerArtifactLease):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			writeError(w, err)
		}
		return
	}
	status := http.StatusCreated
	if reused {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{
		"artifact": workerArtifactResponse{ID: artifact.ID, AssetID: artifact.AssetID, ArtifactType: artifact.Type, ProfileHash: artifact.ProfileHash, SizeBytes: artifact.SizeBytes},
		"reused":   reused,
	})
}

func (s *Server) authenticatedWorker(w http.ResponseWriter, r *http.Request) (remote.Worker, bool) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		http.Error(w, "worker authentication required", http.StatusUnauthorized)
		return remote.Worker{}, false
	}
	worker, err := s.service.AuthenticateWorker(r.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, prefix)))
	if err != nil {
		http.Error(w, "worker authentication failed", http.StatusUnauthorized)
		return remote.Worker{}, false
	}
	return worker, true
}

func (s *Server) hardwareReport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.service.HardwareReport())
}

func (s *Server) agentCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       "v0.12",
		"approval_mode": "human_required",
		"allowed_actions": []string{
			"inspect_readiness",
			"search_shots",
			"create_draft_plan",
			"inspect_plan",
			"revise_draft_plan",
		},
		"denied_actions": []string{
			"approve_plan",
			"run_pipeline",
			"read_provider_keys",
			"access_original_media_paths",
		},
	})
}

func (s *Server) listRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.service.ListLibraryRoots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roots)
}

func (s *Server) createRoot(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Path == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	root, err := s.service.AddLibraryRoot(r.Context(), request.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, root)
}

func (s *Server) scanRoot(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ScanLibraryRoot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	limit := parseInt(r.URL.Query().Get("limit"), 100)
	offset := parseInt(r.URL.Query().Get("offset"), 0)
	assets, err := s.service.ListAssets(r.Context(), limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assets)
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.service.ListJobs(r.Context(), parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		writeError(w, err)
		return
	}
	if jobs == nil {
		jobs = []domain.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) workerJobStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.service.GetWorkerJobStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) setWorkerJobAssignment(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkerID string                      `json:"worker_id"`
		Mode     remote.WorkerAssignmentMode `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input); err != nil {
		http.Error(w, "invalid worker assignment", http.StatusBadRequest)
		return
	}
	if err := s.service.SetDeriveWorkerAssignment(r.Context(), r.PathValue("id"), strings.TrimSpace(input.WorkerID), input.Mode); err != nil {
		http.Error(w, "worker assignment rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	status, err := s.service.GetWorkerJobStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
func (s *Server) runPipeline(w http.ResponseWriter, r *http.Request) {
	if s.service.StartPipeline() {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
	} else {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "already_running"})
	}
}
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	ids, err := s.service.Search(r.Context(), q, parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ids)
}

func (s *Server) searchShots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	hits, err := s.service.SearchShots(r.Context(), q, parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) hybridSearchShots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	hits, err := s.service.HybridSearchShots(r.Context(), q, parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) similarShots(w http.ResponseWriter, r *http.Request) {
	hits, err := s.service.SimilarShots(r.Context(), r.PathValue("id"), parseInt(r.URL.Query().Get("limit"), 20))
	if err != nil {
		if strings.HasPrefix(err.Error(), "shot not found:") {
			http.NotFound(w, r)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) rareShots(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.DiscoverRareShots(r.Context(), parseInt(r.URL.Query().Get("limit"), 50))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listAssetCards(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := domain.AssetCardFilter{
		Limit:       parseInt(query.Get("limit"), 100),
		Offset:      parseInt(query.Get("offset"), 0),
		RegionLabel: query.Get("region"),
		CameraModel: query.Get("camera"),
		SessionID:   query.Get("session"),
		Status:      domain.ProcessingStatus(query.Get("status")),
	}
	if value, err := time.Parse("2006-01-02", query.Get("date_from")); err == nil {
		filter.CapturedFrom = &value
	}
	if value, err := time.Parse("2006-01-02", query.Get("date_to")); err == nil {
		value = value.AddDate(0, 0, 1)
		filter.CapturedTo = &value
	}
	cards, err := s.service.ListAssetCardsFiltered(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cards)
}

func collectionFilterFromQuery(query map[string][]string) domain.AssetCollectionFilter {
	filter := domain.AssetCollectionFilter{
		RegionLabel: queryValue(query, "region"),
		CameraModel: queryValue(query, "camera"),
		SessionID:   queryValue(query, "session"),
		Status:      domain.ProcessingStatus(queryValue(query, "status")),
	}
	if value, err := time.Parse("2006-01-02", queryValue(query, "date_from")); err == nil {
		filter.CapturedFrom = &value
	}
	if value, err := time.Parse("2006-01-02", queryValue(query, "date_to")); err == nil {
		value = value.AddDate(0, 0, 1)
		filter.CapturedTo = &value
	}
	return filter
}

func queryValue(query map[string][]string, key string) string {
	if values := query[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func (s *Server) processingSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.service.GetAssetProcessingSummary(r.Context(), collectionFilterFromQuery(r.URL.Query()))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	collections, err := s.service.ListAssetCollections(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if collections == nil {
		collections = []domain.AssetCollection{}
	}
	writeJSON(w, http.StatusOK, collections)
}

func (s *Server) getCollection(w http.ResponseWriter, r *http.Request) {
	collection, err := s.service.GetAssetCollection(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if collection == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, collection)
}

func (s *Server) listCollectionAssets(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListAssetCardsInCollection(r.Context(), r.PathValue("id"), parseInt(r.URL.Query().Get("limit"), 100), parseInt(r.URL.Query().Get("offset"), 0))
	if err != nil {
		writeError(w, err)
		return
	}
	if items == nil {
		items = []domain.AssetCard{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) saveCollection(w http.ResponseWriter, r *http.Request) {
	var collection domain.AssetCollection
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&collection); err != nil {
		http.Error(w, "invalid collection", http.StatusBadRequest)
		return
	}
	saved, err := s.service.SaveAssetCollection(r.Context(), collection)
	if err != nil {
		http.Error(w, "collection rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteAssetCollection(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listShootSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := domain.ShootSessionFilter{
		RootID:      query.Get("root"),
		State:       query.Get("state"),
		RegionLabel: query.Get("region"),
		CameraLabel: query.Get("camera"),
		Limit:       parseInt(query.Get("limit"), 100),
		Offset:      parseInt(query.Get("offset"), 0),
	}
	if value, err := time.Parse("2006-01-02", query.Get("date_from")); err == nil {
		filter.StartsAfter = &value
	}
	if value, err := time.Parse("2006-01-02", query.Get("date_to")); err == nil {
		value = value.AddDate(0, 0, 1)
		filter.StartsBefore = &value
	}
	sessions, err := s.service.ListShootSessions(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	if sessions == nil {
		sessions = []domain.ShootSession{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) assetDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := s.service.GetAssetDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}
	// The public library response is intentionally safe to render or share.
	// Exact coordinates, absolute NAS paths, file IDs and raw metadata stay
	// behind the Hub administrator boundary.
	if detail.Location != nil {
		detail.Location.AbsolutePath = ""
		detail.Location.FileID = ""
	}
	if detail.Metadata != nil {
		detail.Metadata.Latitude = nil
		detail.Metadata.Longitude = nil
		detail.Metadata.FFProbeRaw = ""
		detail.Metadata.ExifToolRaw = ""
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) assetCaptureLocation(w http.ResponseWriter, r *http.Request) {
	detail, err := s.service.GetAssetDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}
	if detail.Metadata == nil || detail.Metadata.Latitude == nil || detail.Metadata.Longitude == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"latitude":  *detail.Metadata.Latitude,
		"longitude": *detail.Metadata.Longitude,
		"precision": "source",
	})
}

func (s *Server) assetShots(w http.ResponseWriter, r *http.Request) {
	shots, err := s.service.ListAssetShots(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, shots)
}

func (s *Server) assetThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, "thumbnail")
}
func (s *Server) assetProxy(w http.ResponseWriter, r *http.Request) { s.serveArtifact(w, r, "proxy") }
func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, typ string) {
	a, err := s.service.GetArtifact(r.Context(), r.PathValue("id"), typ)
	if err != nil {
		writeError(w, err)
		return
	}
	if a == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(a.LocalPath); err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(a.LocalPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, a.LocalPath)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.Replace(libraryIndexHTML, `<a href="/tags">Tag Curator</a>`, `<a href="/tags">Tag Curator</a><a href="/providers">能力与服务</a>`, 1)
	_, _ = w.Write([]byte(brandedPage(page)))
}

const indexHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 素材库</title><style>
body{font-family:ui-sans-serif,system-ui,-apple-system,sans-serif;margin:0;background:#101827;color:#eef2ff}header{position:sticky;top:0;background:#101827ee;backdrop-filter:blur(12px);padding:14px 20px;display:flex;gap:12px;align-items:center;z-index:2;border-bottom:1px solid #26324a}a{color:#bdc9ff;text-decoration:none;font-size:14px}.brand{font-weight:800;color:white;margin-right:8px}input{flex:1;padding:10px;border-radius:8px;border:1px solid #394862;background:#182237;color:#fff}button{padding:9px 12px;border-radius:8px;border:1px solid #43526d;background:#263656;color:#fff;cursor:pointer}.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:16px;padding:20px}.card{background:#172137;border:1px solid #2c3b58;border-radius:12px;overflow:hidden}.media{aspect-ratio:16/9;background:#000;width:100%;object-fit:cover}.content{padding:12px}.meta{font-size:12px;color:#a9b7ce}.tags{font-size:12px;margin-top:8px;color:#b6c8ff}.summary{margin:8px 0;line-height:1.4}.empty{padding:40px;color:#a9b7ce}</style></head><body><header><span class="brand">Timingdex · 素材库</span><a href="/setup">启动配置</a><a href="/progress">处理进度</a><a href="/repurpose">翻新方案</a><a href="/tags">Tag Curator</a><input id="q" placeholder="搜索 summary / transcript / tags"><button onclick="search()">搜索</button><button onclick="load()">全部</button></header><main id="grid" class="grid"></main><script>
const grid=document.getElementById('grid');const esc=s=>String(s||'').replace(/[&<>\"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;'}[c]));
function card(x){const media=x.proxy_url?'<video class="media" controls preload="metadata" poster="'+x.thumbnail_url+'"><source src="'+x.proxy_url+'"></video>':'<img class="media" loading="lazy" src="'+x.thumbnail_url+'">';return '<article class="card">'+media+'<div class="content"><b>'+esc(x.filename)+'</b><div class="meta">'+Math.round((x.duration_ms||0)/1000)+' 秒 · '+esc(x.orientation)+' · '+(x.has_speech?'有讲话':'无讲话')+'</div><div class="summary">'+esc(x.summary||'等待分析')+'</div><div class="tags">'+esc([x.asset_type,x.camera_motion,x.lighting,...(x.mood_tags||[])].filter(Boolean).join(' · '))+'</div></div></article>'}
async function load(ids){let data=await fetch('/api/v1/assets?limit=300').then(r=>r.json());if(ids)data=data.filter(x=>ids.includes(x.id));grid.innerHTML=data.length?data.map(card).join(''):'<div class="empty">暂无素材</div>'}
async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.json());load(ids)}load();</script></body></html>`

const legacyLibraryIndexHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 素材库</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#0c1422;color:#edf3ff;font:14px ui-sans-serif,system-ui,-apple-system,sans-serif}header{position:sticky;top:0;z-index:5;display:flex;align-items:center;gap:18px;padding:15px max(24px,5vw);background:#0c1422ee;backdrop-filter:blur(15px);border-bottom:1px solid #273750}a{color:#b7c8eb;text-decoration:none;font-size:14px}.brand{font-size:16px;font-weight:850;color:#fff;margin-right:auto;letter-spacing:-.02em}.nav-active{color:#fff}.search{width:min(360px,31vw);display:flex;gap:7px}input{min-width:0;flex:1;padding:10px 11px;border:1px solid #354965;border-radius:9px;background:#111d30;color:#fff;font:inherit}button{border:0;border-radius:9px;padding:10px 13px;background:#324767;color:#eaf1ff;font:inherit;font-weight:750;cursor:pointer}button.primary{background:#91a9ff;color:#0a1324}.wrap{width:min(1440px,100%);margin:auto;padding:36px max(24px,5vw) 64px}.top{display:flex;justify-content:space-between;gap:28px;align-items:end;margin-bottom:26px}.eyebrow{color:#93aaff;font-size:11px;font-weight:850;letter-spacing:.12em}.top h1{margin:8px 0 7px;font-size:clamp(31px,4vw,50px);line-height:1;letter-spacing:-.05em}.muted{margin:0;color:#9fb0ce;line-height:1.6}.legend{display:flex;gap:12px;color:#9fb0ce;font-size:12px;align-items:center}.legend i{width:8px;height:8px;border-radius:50%;display:inline-block;background:#95aaff}.legend i:nth-child(2){background:#70d7b1}.legend i:nth-child(3){background:#ffc783}.library{display:grid;gap:13px}.asset-row{display:grid;grid-template-columns:220px minmax(220px,.75fr) minmax(380px,1.75fr);gap:18px;align-items:stretch;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:16px;transition:border-color .15s,transform .15s}.asset-row:hover{border-color:#506e9d;transform:translateY(-1px)}.thumb,.thumb-empty{width:100%;height:100%;min-height:126px;aspect-ratio:16/9;object-fit:cover;border-radius:10px;background:#070d17}.thumb-empty{display:grid;place-items:center;color:#8193b0;font-size:12px;border:1px dashed #3e536f}.asset-info{display:flex;min-width:0;flex-direction:column;justify-content:center;padding:4px 0}.filename{font-size:16px;font-weight:800;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;letter-spacing:-.02em}.asset-meta{margin-top:7px;color:#a9bad7;font-size:12px}.summary{margin:10px 0;color:#d8e2f4;line-height:1.45}.chips{display:flex;flex-wrap:wrap;gap:5px}.chip{border-radius:99px;padding:4px 7px;background:#223653;color:#b8caef;font-size:11px}.chip.voice{color:#9bf0c6;background:#173e36}.asset-details{display:flex;flex-wrap:wrap;gap:5px;margin-top:8px}.detail{font-size:11px;color:#a9bad7;background:#172a43;border-radius:6px;padding:3px 6px}.detail b{color:#d4e0f8;margin-right:4px}.timeline-card{min-width:0;display:flex;flex-direction:column;justify-content:center;padding:4px 3px}.timeline-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:10px}.timeline-title{font-size:12px;font-weight:800;color:#c9d7ed}.duration{color:#91a3c1;font-size:12px;font-variant-numeric:tabular-nums}.semantic-timeline{position:relative;height:94px;border-radius:10px;border:1px solid #334967;background:linear-gradient(90deg,#0d1727 0%,#111e32 50%,#0d1727 100%);overflow:hidden}.ticks{position:absolute;inset:0;display:flex;justify-content:space-between;padding:6px 9px;color:#71849f;font-size:10px;pointer-events:none}.ticks:before{content:"";position:absolute;top:37px;left:0;right:0;border-top:1px solid #2b405e}.cut-marker{position:absolute;top:4px;left:calc(var(--cut)*1%);z-index:2;max-width:155px;padding-left:7px;color:#c6d4ef;font-size:10px;font-weight:750;line-height:1.2;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;transform:translateX(-4px);pointer-events:none}.cut-marker:before{content:"";position:absolute;left:0;top:18px;height:16px;border-left:1px dashed #8399c0}.shot{position:absolute;top:47px;left:calc(var(--left)*1%);width:max(2%,calc(var(--width)*1%));min-width:13px;height:31px;border-radius:6px;background:var(--tone);border:1px solid #c9d7ff;box-shadow:0 3px 10px #0004;overflow:hidden;cursor:default}.shot-label{display:block;padding:7px 8px;color:#071321;font-size:11px;font-weight:850;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.shot:nth-of-type(4n+1){--tone:#8eaaff}.shot:nth-of-type(4n+2){--tone:#75d9b6}.shot:nth-of-type(4n+3){--tone:#ffc77f}.shot:nth-of-type(4n){--tone:#d69df3}.timeline-empty{height:94px;display:grid;place-items:center;border:1px dashed #405775;border-radius:10px;color:#a0b2ce;font-size:12px}.empty{padding:58px 20px;border:1px dashed #3b5070;border-radius:16px;color:#a8b9d2;text-align:center}.error{color:#ffb2bf}.loading{color:#a3b4cf;font-size:13px;padding:24px}@media(max-width:980px){header{gap:12px}.search{width:260px}.asset-row{grid-template-columns:170px minmax(190px,.8fr) minmax(280px,1.4fr)}}@media(max-width:720px){header{flex-wrap:wrap;padding:14px 20px}.brand{margin-right:0}.search{order:3;width:100%}.wrap{padding:28px 20px 44px}.top{display:block}.legend{margin-top:16px}.asset-row{grid-template-columns:1fr;gap:13px}.thumb,.thumb-empty{min-height:auto;height:auto}.timeline-card{padding:0}.asset-info{padding:0}.semantic-timeline,.timeline-empty{height:98px}.shot{top:51px}.ticks:before{top:41px}.cut-marker:before{height:20px}}</style></head><body><header><span class="brand">Timingdex</span><a class="nav-active" href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a><a href="/repurpose">翻新方案</a><a href="/tags">Tag Curator</a><div class="search"><input id="q" placeholder="搜索内容、口述或标签"><button class="primary" onclick="search()">搜索</button><button onclick="load()">全部</button></div></header><main class="wrap" data-library-browser><section class="top"><div><div class="eyebrow">FOOTAGE LIBRARY · SHOT LEVEL</div><h1>素材库 · 镜头浏览</h1><p class="muted">从缩略图、素材语义到每个时间段的镜头内容，一眼看清你的素材里有什么可以用。</p></div><div class="legend"><span><i></i> 不同镜头</span><span><i></i> 时间范围</span><span><i></i> 语义描述</span></div></section><section id="library" class="library" aria-live="polite"><div class="loading">正在读取素材库…</div></section></main><script>
const library=document.getElementById('library');const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const fmt=ms=>{ms=Math.max(0,Math.floor((ms||0)/1000));return String(Math.floor(ms/60)).padStart(2,'0')+':'+String(ms%60).padStart(2,'0')};
function loadShots(id){return fetch('/api/v1/assets/'+encodeURIComponent(id)+'/shots').then(r=>r.ok?r.json():[]).then(x=>Array.isArray(x)?x:[]).catch(()=>[])}
function chips(x){const values=[x.asset_type,x.camera_motion,x.lighting,...(x.mood_tags||[])].filter(Boolean).slice(0,5);return values.map(v=>'<span class="chip">'+esc(v)+'</span>').join('')+(x.has_speech?'<span class="chip voice">有口述</span>':'')}
function timeline(x,shots){const duration=Math.max(Number(x.duration_ms)||0,1);if(!shots.length)return '<div class="timeline-empty">尚未生成镜头理解；完成分析后会显示可用时间段。</div>';const labels=['00:00',fmt(duration/2),fmt(duration)];const blocks=shots.map(s=>{const start=Math.max(0,Number(s.start_ms)||0),end=Math.max(start,Number(s.end_ms)||start),left=Math.min(100,start/duration*100),width=Math.max(1,(end-start)/duration*100),description=s.description||((s.tags||[]).join(' · '))||'未命名镜头',title=fmt(start)+' — '+fmt(end)+' · '+description;return '<div class="shot" style="--left:'+left.toFixed(3)+';--width:'+width.toFixed(3)+'" title="'+esc(title)+'"><span class="shot-label">'+esc(description)+'</span></div>'}).join('');const cuts=shots.slice(1).map(s=>{const start=Math.max(0,Number(s.start_ms)||0),left=Math.min(100,start/duration*100),description=s.description||((s.tags||[]).join(' · '))||'下一个镜头',time=fmt(start);return '<div class="cut-marker" data-cut-time="'+esc(time)+'" style="--cut:'+left.toFixed(3)+'" title="'+esc(time+' 切入 · '+description)+'">'+esc(time)+'</div>'}).join('');return '<div class="semantic-timeline" aria-label="镜头语义时间轴"><div class="ticks"><span>'+labels[0]+'</span><span>'+labels[1]+'</span><span>'+labels[2]+'</span></div>'+cuts+blocks+'</div>'}
function thumbnail(x){return x.thumbnail_url?'<img class="thumb" loading="lazy" src="'+esc(x.thumbnail_url)+'" alt="'+esc(x.filename)+' 的缩略图" onerror="this.outerHTML=\'<div class=&quot;thumb-empty&quot;>缩略图不可用</div>\'">':'<div class="thumb-empty">暂无缩略图</div>'}
function optionalDetails(x){const fields=[['相机',x.camera_model],['区域',x.region_label],['场次',x.session_id],['色彩',x.source_color],['配置',x.color_profile],['原始格式',x.raw_format],['预览',x.preview_status]].filter(([,value])=>value);return fields.length?'<div class="asset-details" aria-label="拍摄与预览信息">'+fields.map(([label,value])=>'<span class="detail"><b>'+esc(label)+'</b>'+esc(value)+'</span>').join('')+'</div>':''}
function row(x,shots){return '<article class="asset-row"><div>'+thumbnail(x)+'</div><div class="asset-info"><div class="filename" title="'+esc(x.filename)+'">'+esc(x.filename||'未命名素材')+'</div><div class="asset-meta">'+fmt(x.duration_ms)+' · '+esc(x.orientation||'方向未知')+' · '+esc(x.state||'未知状态')+'</div><p class="summary">'+esc(x.summary||'正在等待视频理解结果。')+'</p><div class="chips">'+chips(x)+'</div>'+optionalDetails(x)+'</div><div class="timeline-card"><div class="timeline-head"><span class="timeline-title">镜头语义时间轴</span><span class="duration">'+fmt(x.duration_ms)+'</span></div>'+timeline(x,shots)+'</div></article>'}
async function load(ids){library.innerHTML='<div class="loading">正在整理镜头时间轴…</div>';try{let data=await fetch('/api/v1/assets?limit=300').then(r=>r.ok?r.json():[]);data=Array.isArray(data)?data:[];if(ids)data=data.filter(x=>ids.includes(x.id));if(!data.length){library.innerHTML='<div class="empty">暂无素材。先在「启动配置」添加素材目录并运行处理任务，镜头、缩略图和时间轴会出现在这里。</div>';return}const rows=await Promise.all(data.map(async x=>row(x,await loadShots(x.id))));library.innerHTML=rows.join('')}catch(e){library.innerHTML='<div class="empty error">无法读取素材库：'+esc(e.message)+'</div>'}}
async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[]);load(Array.isArray(ids)?ids:[])}catch(e){library.innerHTML='<div class="empty error">搜索失败：'+esc(e.message)+'</div>'}}document.getElementById('q').addEventListener('keydown',e=>{if(e.key==='Enter')search()});load();</script></body></html>`

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(providersHTML)))
}

// Provider setup is intentionally an explanatory, non-persistent surface in
// this release. It makes the Hub's no-key-in-browser rule visible while the
// authenticated channel APIs are introduced behind the same boundary.
const legacyProvidersHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 能力与服务</title><style>:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#0c1422;color:#edf3ff;font:15px ui-sans-serif,system-ui,sans-serif}header{display:flex;gap:18px;align-items:center;padding:15px 6vw;border-bottom:1px solid #283a57}a{color:#b9ccf5;text-decoration:none}.brand{font-weight:850;color:#fff;margin-right:auto}.wrap{max-width:1080px;margin:auto;padding:42px 24px}.eyebrow{font-size:12px;font-weight:800;letter-spacing:.1em;color:#96aeff}.hero{font-size:clamp(32px,5vw,54px);letter-spacing:-.05em;margin:9px 0}.muted{color:#a9b9d5;line-height:1.65;max-width:760px}.token{margin:24px 0;padding:16px;border:1px solid #3b5074;background:#121f34;border-radius:13px}.token label{display:block;font-size:13px;font-weight:750;margin-bottom:8px}.token input{width:100%;padding:10px;border-radius:8px;border:1px solid #465b7e;background:#091322;color:#fff}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px}.card{padding:17px;border:1px solid #2b405e;border-radius:13px;background:#121f34}.card h2{font-size:16px;margin:0 0 8px}.card p{margin:0;color:#a9bad6;line-height:1.55;font-size:13px}.state{display:inline-block;margin-top:12px;padding:4px 7px;border-radius:99px;background:#203652;color:#bed3ff;font-size:11px;font-weight:750}.notice{margin-top:24px;padding:15px;border-left:3px solid #91a9ff;background:#182743;color:#cfdbf5;line-height:1.6}@media(max-width:600px){.wrap{padding:30px 20px}}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a></header><main class="wrap"><div class="eyebrow">MODEL CHANNELS</div><h1 class="hero">能力与服务</h1><p class="muted">把模型能力当成可替换的通道管理：同一服务商可以配置多个 Key 并轮换；临时故障会重试并切换，长期异常会显示为等待处理。</p><section class="token"><label for="admin-token">管理员访问凭据（仅当前页面内存使用）</label><input id="admin-token" type="password" autocomplete="off" placeholder="需要修改通道时再输入"><p class="muted">当前页面不会保存、上传或回显 API Key；浏览器关闭后管理员凭据即丢弃。</p></section><section class="grid"><article class="card"><h2>Video analysis</h2><p>Gemini、火山视觉、千问 VL 或本地 VLM，用于场景、镜头和语义理解。</p><span class="state">待配置通道</span></article><article class="card"><h2>ASR</h2><p>语音转写通道，可为同一服务商设置多个独立 Key。</p><span class="state">待配置通道</span></article><article class="card"><h2>Embedding</h2><p>用于标签聚类和后续相似镜头检索。</p><span class="state">待配置通道</span></article><article class="card"><h2>Tag curation</h2><p>把模型发散输出整理为可审核的 Canonical Tag。</p><span class="state">待配置通道</span></article><article class="card"><h2>Repurpose</h2><p>为旧素材生成可审核的翻新方案，不自动剪辑。</p><span class="state">待配置通道</span></article></section><section class="notice"><b>API Key 的安全边界</b><br>Key 仅写入 Hub 的受保护密钥库；数据库保存通道与密钥状态，不保存 Key。Worker 不会获得上游 Provider Key。<br><small>adminHeaders 只在当前页面内存中构造，不会进入浏览器长期存储。</small></section></main><script>function adminHeaders(){const token=document.getElementById('admin-token').value.trim();return token?{Authorization:'Bearer '+token}:{}}</script></body></html>`

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(setupHTML)))
}

// The setup page intentionally never posts provider keys: Timingdex loads them
// from the process environment at startup, keeping browser state and SQLite
// free of secrets. It provides a local checklist and a copyable launch command.
const legacySetupHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 启动配置</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 6vw;border-bottom:1px solid #263651;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:960px;margin:0 auto;padding:46px 24px}.eyebrow{color:#8da7ff;font-weight:700;letter-spacing:.09em;font-size:12px}.hero{font-size:clamp(31px,5vw,55px);line-height:1.04;letter-spacing:-.045em;margin:10px 0 14px}.sub{color:#aab8d0;max-width:670px;line-height:1.65}.grid{display:grid;grid-template-columns:1.1fr .9fr;gap:18px;margin-top:30px}.panel{background:#172238;border:1px solid #2c3d5b;border-radius:16px;padding:22px}.panel h2{margin:0 0 8px;font-size:18px}.muted{color:#aab8d0;line-height:1.55;font-size:14px}label{display:block;color:#cbd7ef;font-size:13px;font-weight:700;margin:16px 0 6px}input,select{width:100%;background:#0e1728;border:1px solid #354764;border-radius:9px;color:#fff;padding:11px;font:inherit}button{background:#88a4ff;color:#091227;border:0;border-radius:9px;padding:11px 14px;font-weight:800;cursor:pointer;margin-top:14px}.notice{border-left:3px solid #88a4ff;background:#1c2a46;padding:12px 14px;border-radius:7px;margin-top:18px;color:#cad7f5;font-size:13px;line-height:1.5}.status{display:flex;gap:10px;padding:12px 0;border-bottom:1px solid #2a3953}.status:last-child{border:0}.dot{width:9px;height:9px;border-radius:50%;background:#68758c;margin-top:5px}.dot.ok{background:#60d69a}.dot.bad{background:#ff8d9b}code{display:block;white-space:pre-wrap;word-break:break-all;background:#0c1423;padding:12px;border-radius:8px;color:#d4defd;font-size:12px;margin-top:10px}@media(max-width:700px){.grid{grid-template-columns:1fr}.wrap{padding-top:30px}}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/progress">处理进度</a><a href="/repurpose">翻新方案</a><a href="/tags">Tag Curator</a></header><main class="wrap"><div class="eyebrow">LOCAL-FIRST SETUP</div><h1 class="hero">启动前，只要把模型钥匙交给终端。</h1><p class="sub">这里不会保存或上传你的 API Key。填写下方内容只会在当前浏览器生成一条可复制的本地启动命令；关闭页面后即丢弃。Timingdex 启动时从环境变量读取配置。</p><div class="grid"><section class="panel"><h2>生成启动命令</h2><p class="muted">至少选一个视频理解模型；没有模型时仍可浏览已有素材，但新的分析任务会停在可见的失败状态。</p><label>素材数据目录</label><input id="data" placeholder="$PWD/.timingdex-dev"><label>素材来源</label><select id="staging"><option value="none">本机磁盘（直接处理）</option><option value="copy">NAS / 网络目录（先暂存到本机）</option></select><label>视频理解模型</label><select id="provider"><option value="gemini">Gemini</option><option value="volcengine_video">火山视觉</option><option value="qwen_video">千问 VL</option><option value="local_vlm">本地 VLM</option></select><label>API Key（只用于生成命令，不发送到服务器）</label><input id="key" type="password" autocomplete="off" placeholder="粘贴后复制命令"><button onclick="build()">生成并复制命令</button><code id="command">填写后会出现启动命令</code><div class="notice">提示：复制后请在终端执行。若你已通过 <code style="display:inline;padding:1px 4px">config.json</code> 配好 provider，只需要设置数据目录并运行服务。</div></section><aside class="panel"><h2>本机检查</h2><p class="muted">这只读取当前已经运行的 Timingdex 服务，不暴露配置或密钥。</p><div id="health"><div class="status"><span class="dot"></span><span>正在检查服务…</span></div></div><div class="notice">下一步：打开「处理进度」，添加素材根目录并扫描；作业完成后可在「翻新方案」中输入创作需求。</div></aside></div></main><script>
const esc=s=>String(s||'').replace(/'/g,"'\\''");
function build(){const dir=document.getElementById("data").value.trim()||"$PWD/.timingdex-dev",p=document.getElementById("provider").value,k=document.getElementById("key").value.trim(),staging=document.getElementById("staging").value;const env=p==="gemini"?"GEMINI_API_KEY":p==="volcengine_video"?"ARK_API_KEY":p==="qwen_video"?"DASHSCOPE_API_KEY":"";let c="TIMINGDEX_DATA_DIR="+esc(dir);if(staging==="copy")c+=" TIMINGDEX_SOURCE_STAGING_MODE=copy";if(k&&env)c+=" "+env+"="+esc(k);c+=" ./timingdex serve";document.getElementById("command").textContent=c;navigator.clipboard&&navigator.clipboard.writeText(c).catch(()=>{});}async function check(){const el=document.getElementById('health');try{const [health,hardware]=await Promise.all([fetch('/api/v1/health').then(r=>r.json()),fetch('/api/v1/hardware').then(r=>r.json())]);el.innerHTML='<div class="status"><span class="dot ok"></span><span><b>服务已运行</b><br><small>health: '+health.status+'</small></span></div><div class="status"><span class="dot ok"></span><span><b>媒体能力已识别</b><br><small>'+String(hardware.selected_profile||hardware.mode||'按当前配置')+'</small></span></div>'}catch(e){el.innerHTML='<div class="status"><span class="dot bad"></span><span><b>无法连接本机服务</b><br><small>请在此机器运行 ./timingdex serve</small></span></div>'}}check();</script></body></html>`

func (s *Server) progressPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(progressHTML)))
}

const progressHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 处理进度</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:14px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:1;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:1180px;margin:auto;padding:32px 24px}.top{display:flex;justify-content:space-between;gap:18px;align-items:flex-end}.top h1{font-size:32px;margin:0;letter-spacing:-.04em}.muted{color:#aab8d0;line-height:1.5}button{background:#86a3ff;color:#091227;border:0;border-radius:9px;padding:10px 14px;font-weight:800;cursor:pointer}.metrics{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin:24px 0}.metric,.panel{background:#172238;border:1px solid #2c3d5b;border-radius:14px}.metric{padding:16px}.number{font-size:30px;font-weight:800;letter-spacing:-.04em;margin-top:5px}.panels{display:grid;grid-template-columns:1.4fr .8fr;gap:16px}.panel{padding:18px}.panel h2{margin:0 0 14px;font-size:17px}table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:10px 7px;border-bottom:1px solid #2b3b55;font-size:13px;vertical-align:top}.state{border-radius:99px;padding:3px 8px;font-size:12px;font-weight:700;background:#34445e}.state.succeeded{background:#164b39;color:#9cf0c2}.state.failed{background:#612c3a;color:#ffc0c8}.state.running{background:#3a376b;color:#d8d4ff}.log{font:12px ui-monospace,SFMono-Regular,Menlo,monospace;line-height:1.55;color:#b9c7e3;min-height:280px;max-height:480px;overflow:auto;white-space:pre-wrap}.log div{padding:6px 0;border-bottom:1px solid #263650}@media(max-width:760px){.metrics{grid-template-columns:repeat(2,1fr)}.panels{grid-template-columns:1fr}.top{align-items:flex-start;flex-direction:column}}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/repurpose">翻新方案</a><a href="/tags">Tag Curator</a></header><main class="wrap"><div class="top"><div><h1>处理进度</h1><p class="muted">状态每 2.5 秒更新。右侧是本次浏览器会话的操作记录；下方作业错误来自本地任务队列。</p></div><button id="run" onclick="runPipeline()">运行待处理任务</button></div><section id="metrics" class="metrics"></section><section class="panels"><div class="panel"><h2>最近作业</h2><div id="jobs" class="muted">正在读取…</div></div><div class="panel"><h2>本次操作</h2><div id="log" class="log"></div></div></section></main><script>
const logEl=document.getElementById('log');let logs=[];function log(m){logs.unshift(new Date().toLocaleTimeString()+'  '+m);logs=logs.slice(0,30);logEl.innerHTML=logs.map(x=>'<div>'+x+'</div>').join('')}function esc(s){return String(s||'').replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))}function render(jobs){const counts={pending:0,running:0,succeeded:0,failed:0,skipped:0};jobs.forEach(j=>counts[j.state]=(counts[j.state]||0)+1);document.getElementById('metrics').innerHTML=['待处理','处理中','已完成','需处理'].map((n,i)=>'<div class="metric"><div class="muted">'+n+'</div><div class="number">'+[counts.pending,counts.running,counts.succeeded,counts.failed][i]+'</div></div>').join('');document.getElementById('jobs').innerHTML=jobs.length?'<table><tr><th>类型</th><th>状态</th><th>尝试</th><th>错误 / 下次运行</th></tr>'+jobs.map(j=>'<tr><td>'+esc(j.job_type)+'</td><td><span class="state '+esc(j.state)+'">'+esc(j.state)+'</span></td><td>'+j.attempt_count+'/'+j.max_attempts+'</td><td>'+esc(j.last_error||j.run_after||'—')+'</td></tr>').join('')+'</table>':'暂无作业。先在素材根目录扫描视频。'}async function refresh(){try{const jobs=await fetch('/api/v1/jobs?limit=100').then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});render(jobs)}catch(e){document.getElementById('jobs').textContent='无法读取作业：'+e.message}}async function runPipeline(){const b=document.getElementById('run');b.disabled=true;b.textContent='正在运行…';log('已请求执行待处理任务');try{const r=await fetch('/api/v1/pipeline/run',{method:'POST'});if(!r.ok)throw Error(await r.text());const d=await r.json().catch(()=>({}));log(d.status==='already_running'?'已有处理任务在后台运行':'处理任务已在后台启动，可关闭本页')}catch(e){log('执行失败：'+e.message)}finally{b.disabled=false;b.textContent='运行待处理任务';refresh()}}refresh();setInterval(refresh,2500);log('进度面板已打开');</script></body></html>`

func (s *Server) repurposePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(repurposeWorkspaceHTML)))
}

const repurposeHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 素材翻新方案</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:2;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:1200px;margin:auto;padding:38px 24px}.intro{display:grid;grid-template-columns:1.1fr .9fr;gap:22px;align-items:end}.eyebrow{color:#8da7ff;font-weight:800;font-size:12px;letter-spacing:.1em}.intro h1{font-size:clamp(32px,5vw,54px);line-height:1.02;letter-spacing:-.05em;margin:10px 0}.muted{color:#aab8d0;line-height:1.6}.form,.plan,.section{background:#172238;border:1px solid #2c3d5b;border-radius:16px}.form{padding:20px}.field{margin:11px 0}.field label{font-size:12px;color:#bfcae0;font-weight:800;display:block;margin:0 0 5px}textarea,input{width:100%;font:inherit;padding:10px;border-radius:8px;background:#0e1728;color:#fff;border:1px solid #374965}textarea{min-height:94px;resize:vertical}.row{display:grid;grid-template-columns:repeat(3,1fr);gap:9px}.primary{margin-top:8px;padding:11px 15px;background:#8ca7ff;border:0;border-radius:9px;color:#0d1830;font-weight:900;cursor:pointer}.plan{margin-top:28px;padding:24px}.planhead{display:flex;justify-content:space-between;align-items:flex-start;gap:16px}.planhead h2{font-size:27px;margin:0;letter-spacing:-.035em}.pill{border:1px solid #465b82;color:#b8c9ff;border-radius:99px;padding:4px 9px;font-size:12px;white-space:nowrap}.section{margin-top:13px;padding:16px}.sectionhead{display:flex;justify-content:space-between;gap:12px}.role{font-weight:900;text-transform:capitalize}.rationale{color:#b7c5de;font-size:13px;margin:7px 0 13px}.candidate{display:grid;grid-template-columns:150px 1fr;gap:13px;background:#111b2d;border:1px solid #2a3a56;border-radius:11px;padding:10px;margin:8px 0}.thumb{width:150px;aspect-ratio:16/9;object-fit:cover;background:#090f1b;border-radius:7px}.candidate b{font-size:14px}.small{font-size:12px;color:#aab8d0;margin-top:5px;line-height:1.45}.empty{border:1px dashed #4d607f;border-radius:10px;padding:14px;color:#b5c4df;font-size:13px}.warn{color:#ffc98d}.approve{background:#74ddb2;border:0;border-radius:9px;padding:10px 14px;color:#08241b;font-weight:900;cursor:pointer}.error{color:#ffb5c0;margin-top:12px}@media(max-width:760px){.intro{grid-template-columns:1fr}.row{grid-template-columns:1fr}.candidate{grid-template-columns:1fr}.thumb{width:100%}.wrap{padding-top:28px}}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a><a href="/tags">Tag Curator</a></header><main class="wrap"><div class="intro"><div><div class="eyebrow">REPURPOSE WORKSPACE</div><h1>给旧素材一个新的创作任务。</h1><p class="muted">输入成片需求，Timingdex 只提出可审核的镜头组合和时间范围。它不会替你剪辑，也不会改动原始媒体。</p></div><form class="form" onsubmit="createPlan(event)"><div class="field"><label>这次要做什么？</label><textarea id="brief" required placeholder="例如：做一个 30 秒深圳城市生活宣传片，要有夜景、通勤和人文气息"></textarea></div><div class="row"><div class="field"><label>总时长（秒）</label><input id="duration" type="number" min="5" value="30"></div><div class="field"><label>风格</label><input id="style" placeholder="城市生活"></div><div class="field"><label>受众</label><input id="audience" placeholder="品牌客户"></div></div><button class="primary" id="create">生成可审核方案</button><div id="error" class="error"></div></form></div><section id="result" class="plan" aria-live="polite"><div class="muted">先写下创作需求。结果会显示每段素材的准确时间范围、推荐理由和是否复用。</div></section></main><script>
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const fmt=ms=>{ms=Math.max(0,Math.floor((ms||0)/1000));return String(Math.floor(ms/60)).padStart(2,'0')+':'+String(ms%60).padStart(2,'0')};let activePlan=null;async function api(url,opt){const r=await fetch(url,opt);if(!r.ok)throw Error(await r.text());return r.json()}function candidate(c){const image='/api/v1/assets/'+encodeURIComponent(c.asset_id)+'/thumbnail';const why=(c.reasons||[]).map(esc).join(' · ')||'由检索得分匹配';return '<article class="candidate"><img class="thumb" loading="lazy" src="'+image+'" onerror="this.style.visibility=\'hidden\'" alt="候选镜头缩略图"><div><b>'+fmt(c.start_ms)+' — '+fmt(c.end_ms)+'</b> <span class="pill">匹配 '+Math.round((c.score||0)*100)+'%</span>'+(c.reused?' <span class="pill warn">复用镜头</span>':'')+'<div class="small">素材 '+esc(c.asset_id)+' · 镜头 '+esc(c.shot_id)+'</div><div class="small">'+why+'</div></div></article>'}function render(p){activePlan=p;const sections=(p.sections||[]).map(s=>'<section class="section"><div class="sectionhead"><div><div class="role">'+esc(s.role)+'</div><div class="small">目标 '+fmt(s.duration_ms)+' · 查询：'+esc(s.query)+'</div></div><span class="pill">'+(s.required?'必需':'可选')+'</span></div><div class="rationale">'+esc(s.rationale||'')+'</div>'+(s.candidates&&s.candidates.length?s.candidates.map(candidate).join(''):'<div class="empty">此段没有足够的匹配镜头。保留为空，方便编辑时决定是否补拍或调整需求。</div>')+'</section>').join('');document.getElementById('result').innerHTML='<div class="planhead"><div><div class="eyebrow">'+esc(p.provider||'deterministic')+' · '+esc(p.model||'')+'</div><h2>'+esc(p.title||p.brief)+'</h2><p class="muted">'+fmt(p.duration_ms)+' · '+esc(p.style||'未设定风格')+' · '+esc(p.audience||'未设定受众')+'</p></div><div><span class="pill">'+esc(p.status)+'</span> '+(p.status==='draft'?'<button class="approve" onclick="approve()">批准这一版</button>':'')+'</div></div>'+(p.missing_needs&&p.missing_needs.length?'<p class="warn">还缺：'+p.missing_needs.map(esc).join('、')+'</p>':'')+sections}async function createPlan(e){e.preventDefault();const b=document.getElementById('create'),error=document.getElementById('error');b.disabled=true;error.textContent='';try{const p=await api('/api/v1/repurpose/plans',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({brief:document.getElementById('brief').value,duration_ms:Number(document.getElementById('duration').value)*1000,style:document.getElementById('style').value,audience:document.getElementById('audience').value})});render(p)}catch(e){error.textContent='无法生成方案：'+e.message}finally{b.disabled=false}}async function approve(){if(!activePlan||!confirm('批准后方案会锁定，不能继续修改。确定吗？'))return;try{const revisions=await api('/api/v1/repurpose/plans/'+activePlan.id+'/revisions');const latest=revisions[0];const approved=await api('/api/v1/repurpose/plans/'+activePlan.id+'/revisions/'+latest.revision+'/approve',{method:'POST'});render(approved.plan)}catch(e){alert('批准失败：'+e.message)}}</script></body></html>`

const repurposeWorkspaceHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 翻新工作台</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:2;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:1200px;margin:auto;padding:38px 24px}.intro{display:grid;grid-template-columns:1.1fr .9fr;gap:22px;align-items:end}.eyebrow{color:#8da7ff;font-weight:800;font-size:12px;letter-spacing:.1em}.intro h1{font-size:clamp(32px,5vw,54px);line-height:1.02;letter-spacing:-.05em;margin:10px 0}.muted{color:#aab8d0;line-height:1.6}.form,.plan,.section{background:#172238;border:1px solid #2c3d5b;border-radius:16px}.form{padding:20px}.field{margin:11px 0}.field label{font-size:12px;color:#bfcae0;font-weight:800;display:block;margin:0 0 5px}textarea,input{width:100%;font:inherit;padding:10px;border-radius:8px;background:#0e1728;color:#fff;border:1px solid #374965}textarea{min-height:92px;resize:vertical}.row{display:grid;grid-template-columns:repeat(3,1fr);gap:9px}button{border:0;border-radius:8px;padding:8px 11px;font:inherit;font-weight:800;cursor:pointer}.primary{margin-top:8px;background:#8ca7ff;color:#0d1830}.plan{margin-top:28px;padding:24px}.planhead{display:flex;justify-content:space-between;align-items:flex-start;gap:16px}.planhead h2{font-size:27px;margin:0;letter-spacing:-.035em}.pill{border:1px solid #465b82;color:#b8c9ff;border-radius:99px;padding:4px 9px;font-size:12px;white-space:nowrap}.section{margin-top:13px;padding:16px}.sectionhead{display:flex;justify-content:space-between;gap:12px;align-items:start}.sectionactions{display:flex;gap:7px;align-items:center}.role{font-weight:900;text-transform:capitalize}.rationale{color:#b7c5de;font-size:13px;margin:7px 0 13px}.candidate{display:grid;grid-template-columns:150px 1fr;gap:13px;background:#111b2d;border:1px solid #2a3a56;border-radius:11px;padding:10px;margin:8px 0}.candidate.selected{border-color:#91acff;background:#172747}.candidate.excluded{opacity:.55;border-style:dashed}.thumb{width:150px;aspect-ratio:16/9;object-fit:cover;background:#090f1b;border-radius:7px}.candidate b{font-size:14px}.small{font-size:12px;color:#aab8d0;margin-top:5px;line-height:1.45}.candidate-actions{display:flex;gap:7px;flex-wrap:wrap;margin-top:10px}.select{background:#a7baff;color:#101a30}.select.on{background:#73dcad;color:#082419}.secondary{background:#2b3d5b;color:#cbd7f4}.exclude{background:#4a2f3c;color:#ffc2cb}.empty{border:1px dashed #4d607f;border-radius:10px;padding:14px;color:#b5c4df;font-size:13px}.warn{color:#ffc98d}.approve{background:#74ddb2;color:#08241b}.save{background:#8ca7ff;color:#0d1830}.error{color:#ffb5c0;margin-top:12px}.editor{margin-top:20px;border-top:1px solid #31425f;padding-top:16px}.editorbar{display:flex;justify-content:space-between;gap:12px;align-items:end}.editorbar textarea{min-height:56px}.statusline{font-size:12px;color:#9db1d2;margin-top:7px}@media(max-width:760px){.intro{grid-template-columns:1fr}.row{grid-template-columns:1fr}.candidate{grid-template-columns:1fr}.thumb{width:100%}.wrap{padding-top:28px}.editorbar{align-items:stretch;flex-direction:column}}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a><a href="/tags">Tag Curator</a></header><main class="wrap"><div class="intro"><div><div class="eyebrow">REPURPOSE WORKSPACE · V0.11</div><h1>把推荐，变成你的剪辑选择。</h1><p class="muted">选择一个候选镜头、锁住关键决定、排除不适合的备选，再保存为独立 revision。原视频不会被修改。</p></div><form class="form" onsubmit="createPlan(event)"><div class="field"><label>这次要做什么？</label><textarea id="brief" required placeholder="例如：做一个 30 秒深圳城市生活宣传片，要有夜景、通勤和人文气息"></textarea></div><div class="row"><div class="field"><label>总时长（秒）</label><input id="duration" type="number" min="5" value="30"></div><div class="field"><label>风格</label><input id="style" placeholder="城市生活"></div><div class="field"><label>受众</label><input id="audience" placeholder="品牌客户"></div></div><button class="primary" id="create">生成可审核方案</button><div id="error" class="error"></div></form></div><section id="result" class="plan" aria-live="polite"><div class="muted">先写下创作需求。生成后可为每个段落选择、锁定或排除候选镜头。</div></section></main><script>
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const fmt=ms=>{ms=Math.max(0,Math.floor((ms||0)/1000));return String(Math.floor(ms/60)).padStart(2,'0')+':'+String(ms%60).padStart(2,'0')};let activePlan=null,dirty=false;
async function api(url,opt){const r=await fetch(url,opt);if(!r.ok)throw Error(await r.text());return r.json()}
function section(role){return activePlan.sections.find(s=>s.role===role)}
function rerender(){render(activePlan);dirty=true;document.getElementById('statusline').textContent='有未保存的编辑。保存后会创建新的 revision。'}
function candidate(s,c){const selected=s.selected_shot_id===c.shot_id,excluded=(s.excluded_shot_ids||[]).includes(c.shot_id),image='/api/v1/assets/'+encodeURIComponent(c.asset_id)+'/thumbnail',why=(c.reasons||[]).map(esc).join(' · ')||'由检索得分匹配';return '<article class="candidate '+(selected?'selected ':'')+(excluded?'excluded':'')+'"><img class="thumb" loading="lazy" src="'+image+'" onerror="this.style.visibility=\'hidden\'" alt="候选镜头缩略图"><div><b>'+fmt(c.start_ms)+' — '+fmt(c.end_ms)+'</b> <span class="pill">匹配 '+Math.round((c.score||0)*100)+'%</span>'+(c.reused?' <span class="pill warn">复用镜头</span>':'')+(selected?' <span class="pill">已选</span>':'')+'<div class="small">素材 '+esc(c.asset_id)+' · 镜头 '+esc(c.shot_id)+'</div><div class="small">'+why+'</div><div class="candidate-actions"><button class="select '+(selected?'on':'')+'" data-role="'+esc(s.role)+'" data-shot="'+esc(c.shot_id)+'" onclick="choose(this.dataset.role,this.dataset.shot)">'+(selected?'已选择':'选择此镜头')+'</button><button class="exclude" data-role="'+esc(s.role)+'" data-shot="'+esc(c.shot_id)+'" onclick="toggleExclude(this.dataset.role,this.dataset.shot)" '+(selected?'disabled':'')+'>'+ (excluded?'恢复候选':'排除')+'</button></div></div></article>'}
function render(p){activePlan=p;const sections=(p.sections||[]).map(s=>'<section class="section"><div class="sectionhead"><div><div class="role">'+esc(s.role)+'</div><div class="small">目标 '+fmt(s.duration_ms)+' · 查询：'+esc(s.query)+'</div></div><div class="sectionactions"><span class="pill">'+(s.required?'必需':'可选')+'</span>'+(s.locked?'<button class="secondary" data-role="'+esc(s.role)+'" onclick="unlock(this.dataset.role)">解除锁定</button>':'<button class="secondary" data-role="'+esc(s.role)+'" onclick="lock(this.dataset.role)">锁定选择</button>')+'<button class="secondary" data-role="'+esc(s.role)+'" onclick="findAlternatives(this.dataset.role)">找替代镜头</button></div></div><div class="rationale">'+esc(s.rationale||'')+(s.locked?' · 此段已锁定':'')+'</div>'+(s.candidates&&s.candidates.length?s.candidates.map(c=>candidate(s,c)).join(''):'<div class="empty">此段没有足够的匹配镜头。可以尝试找替代镜头，或保留缺口以便补拍。</div>')+'</section>').join('');document.getElementById('result').innerHTML='<div class="planhead"><div><div class="eyebrow">'+esc(p.provider||'deterministic')+' · '+esc(p.model||'')+'</div><h2>'+esc(p.title||p.brief)+'</h2><p class="muted">'+fmt(p.duration_ms)+' · '+esc(p.style||'未设定风格')+' · '+esc(p.audience||'未设定受众')+'</p></div><div><span class="pill">'+esc(p.status)+'</span> '+(p.status==='draft'?'<button class="approve" onclick="approve()">批准这一版</button>':'')+'</div></div>'+(p.missing_needs&&p.missing_needs.length?'<p class="warn">还缺：'+p.missing_needs.map(esc).join('、')+'</p>':'')+sections+(p.status==='draft'?'<div class="editor"><div class="editorbar"><div style="flex:1"><label class="small" for="editorNote">这次编辑的说明</label><textarea id="editorNote" placeholder="例如：将雨夜航拍锁为开场，排除手持街拍"></textarea><div id="statusline" class="statusline">选择镜头后，保存为新的编辑版。</div></div><button class="save" onclick="saveRevision()">保存编辑版</button></div></div>':'')}
function choose(role,shot){const s=section(role);if(s.locked){alert('此段已锁定，请先解除锁定。');return}s.selected_shot_id=shot;s.excluded_shot_ids=(s.excluded_shot_ids||[]).filter(id=>id!==shot);rerender()}
function lock(role){const s=section(role);if(!s.selected_shot_id){alert('请先选择此段要使用的镜头。');return}s.locked=true;s.unlock=false;rerender()}
function unlock(role){const s=section(role);s.locked=false;s.unlock=true;rerender()}
function toggleExclude(role,shot){const s=section(role);if(s.selected_shot_id===shot){alert('已选镜头不能同时被排除。');return}const ids=s.excluded_shot_ids||[];s.excluded_shot_ids=ids.includes(shot)?ids.filter(id=>id!==shot):ids.concat(shot);rerender()}
async function findAlternatives(role){const s=section(role);try{const hits=await api('/api/v1/search/shots/hybrid?q='+encodeURIComponent(s.query)+'&limit=12');let added=0;(hits||[]).forEach(h=>{if((s.candidates||[]).some(c=>c.shot_id===h.id))return;(s.candidates=s.candidates||[]).push({shot_id:h.id,asset_id:h.asset_id,start_ms:h.start_ms,end_ms:h.end_ms,score:h.score||0,reasons:[h.description||'',...(h.tags||[])] .filter(Boolean)});added++});if(!added)alert('没有找到新的替代镜头。');rerender()}catch(e){alert('无法查找替代镜头：'+e.message)}}
async function saveRevision(){if(!activePlan||!dirty)return;const note=document.getElementById('editorNote').value;try{const r=await api('/api/v1/repurpose/plans/'+activePlan.id+'/revisions',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({sections:activePlan.sections,editor_note:note})});dirty=false;render(r.plan);document.getElementById('statusline').textContent='已保存为 revision '+r.revision+'。'}catch(e){alert('保存失败：'+e.message)}}
async function createPlan(e){e.preventDefault();const b=document.getElementById('create'),error=document.getElementById('error');b.disabled=true;error.textContent='';try{const p=await api('/api/v1/repurpose/plans',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({brief:document.getElementById('brief').value,duration_ms:Number(document.getElementById('duration').value)*1000,style:document.getElementById('style').value,audience:document.getElementById('audience').value})});dirty=false;render(p)}catch(e){error.textContent='无法生成方案：'+e.message}finally{b.disabled=false}}
async function approve(){if(!activePlan)return;if(dirty){alert('请先保存当前编辑，再批准。');return}if(!confirm('批准后方案会锁定，不能继续修改。确定吗？'))return;try{const revisions=await api('/api/v1/repurpose/plans/'+activePlan.id+'/revisions');const latest=revisions[0];const approved=await api('/api/v1/repurpose/plans/'+activePlan.id+'/revisions/'+latest.revision+'/approve',{method:'POST'});render(approved.plan)}catch(e){alert('批准失败：'+e.message)}}</script></body></html>`

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListCanonicalTags(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
func (s *Server) listUnresolvedTags(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListUnresolvedTags(r.Context(), parseInt(r.URL.Query().Get("limit"), 200))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
func (s *Server) runTagCurator(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.RunTagCurator(r.Context(), parseInt(r.URL.Query().Get("limit"), 500))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
func (s *Server) runTagEmbeddingClusters(w http.ResponseWriter, r *http.Request) {
	threshold := 0.86
	if raw := r.URL.Query().Get("threshold"); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil {
			threshold = parsed
		}
	}
	result, err := s.service.RunTagEmbeddingClusters(r.Context(), parseInt(r.URL.Query().Get("limit"), 500), threshold)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
func (s *Server) listTagProposals(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListTagProposals(r.Context(), r.URL.Query().Get("state"), parseInt(r.URL.Query().Get("limit"), 200))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
func (s *Server) reviewTagProposal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.service.ReviewTagProposal(r.Context(), r.PathValue("id"), req.Action, req.Note); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Action + "d"})
}
func (s *Server) latestLibrarySummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.service.LatestLibrarySummary(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if summary == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
func (s *Server) generateLibrarySummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.service.GenerateLibrarySummary(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, summary)
}

func (s *Server) createRepurposePlan(w http.ResponseWriter, r *http.Request) {
	var brief domain.RepurposeBrief
	if err := json.NewDecoder(r.Body).Decode(&brief); err != nil || strings.TrimSpace(brief.Brief) == "" {
		http.Error(w, "brief is required", http.StatusBadRequest)
		return
	}
	plan, err := s.service.CreateRepurposePlan(r.Context(), brief)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, plan)
}

func (s *Server) getRepurposePlan(w http.ResponseWriter, r *http.Request) {
	plan, err := s.service.GetRepurposePlan(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if plan == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) listRepurposePlanRevisions(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListRepurposePlanRevisions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) reviseRepurposePlan(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Sections   []domain.PlanSection `json:"sections"`
		EditorNote string               `json:"editor_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	revision, err := s.service.ReviseRepurposePlan(r.Context(), r.PathValue("id"), request.Sections, request.EditorNote)
	if err != nil {
		if errors.Is(err, app.ErrInvalidRepurposeRevision) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(err.Error(), "immutable") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if strings.Contains(err.Error(), "not found") {
			http.NotFound(w, r)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, revision)
}

func (s *Server) approveRepurposePlanRevision(w http.ResponseWriter, r *http.Request) {
	revision, err := strconv.Atoi(r.PathValue("revision"))
	if err != nil || revision <= 0 {
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}
	approved, err := s.service.ApproveRepurposePlanRevision(r.Context(), r.PathValue("id"), revision)
	if err != nil {
		if errors.Is(err, app.ErrInvalidRepurposeRevision) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(err.Error(), "not found") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(err.Error(), "latest") || strings.Contains(err.Error(), "not draft") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approved)
}

func (s *Server) tagsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(tagsHTML)))
}

const tagsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex Tag Curator</title><style>
body{font-family:system-ui,-apple-system,sans-serif;margin:0;background:#111;color:#eee}header{position:sticky;top:0;padding:16px;background:#181818;display:flex;gap:12px;align-items:center}a{color:#8bc5ff}button{padding:8px 12px;border:0;border-radius:8px;cursor:pointer}.primary{background:#e8e8e8}.approve{background:#b8efc0}.reject{background:#efb8b8}.wrap{padding:16px;display:grid;gap:20px}.panel{background:#1b1b1b;border:1px solid #333;border-radius:12px;padding:14px}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:9px;border-bottom:1px solid #333;vertical-align:top}.muted{color:#aaa;font-size:12px}.pill{display:inline-block;background:#333;padding:3px 7px;border-radius:999px;margin:2px;font-size:12px}</style></head><body><header><strong>Tag Curator</strong><a href="/">素材库</a><button class="primary" onclick="curate()">生成治理提案</button></header><div class="wrap"><section class="panel"><h2>未解析标签</h2><div id="unresolved"></div></section><section class="panel"><h2>待审核提案</h2><div id="proposals"></div></section><section class="panel"><h2>Canonical Tags</h2><div id="tags"></div></section></div><script>
const esc=s=>String(s??'').replace(/[&<>\"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;'}[c]));
async function j(url,opt){const r=await fetch(url,opt);if(!r.ok)throw new Error(await r.text());return r.json()}
async function curate(){await j('/api/v1/tags/curate',{method:'POST'});await load()}
async function review(id,action){await j('/api/v1/tags/proposals/'+id+'/review',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({action})});await load()}
function unresolvedTable(xs){return '<table><tr><th>标准化值</th><th>原始形式</th><th>素材数</th></tr>'+xs.map(x=>'<tr><td>'+esc(x.normalized_tag)+'</td><td>'+x.display_forms.map(v=>'<span class="pill">'+esc(v)+'</span>').join('')+'</td><td>'+x.asset_count+'</td></tr>').join('')+'</table>'}
function proposalTable(xs){return '<table><tr><th>动作</th><th>Canonical</th><th>原因</th><th>影响</th><th></th></tr>'+xs.map(x=>'<tr><td>'+esc(x.proposal_type)+'</td><td>'+esc(x.canonical_name)+'</td><td>'+esc(x.reason)+'<div class="muted">置信度 '+Math.round(x.confidence*100)+'%</div></td><td>'+x.affected_assets+'</td><td><button class="approve" onclick="review(\''+x.id+'\',\'approve\')">批准</button> <button class="reject" onclick="review(\''+x.id+'\',\'reject\')">拒绝</button></td></tr>').join('')+'</table>'}
function tagTable(xs){return '<table><tr><th>Canonical</th><th>分类</th><th>素材</th><th>别名</th></tr>'+xs.map(x=>'<tr><td>'+esc(x.canonical_name)+'</td><td>'+esc(x.category)+'</td><td>'+x.usage_count+'</td><td>'+x.alias_count+'</td></tr>').join('')+'</table>'}
async function load(){const [u,p,t]=await Promise.all([j('/api/v1/tags/unresolved'),j('/api/v1/tags/proposals?state=pending'),j('/api/v1/tags')]);document.getElementById('unresolved').innerHTML=u.length?unresolvedTable(u):'<p class="muted">暂无未解析标签</p>';document.getElementById('proposals').innerHTML=p.length?proposalTable(p):'<p class="muted">暂无待审核提案</p>';document.getElementById('tags').innerHTML=t.length?tagTable(t):'<p class="muted">尚未建立 Canonical Tag</p>'}load();</script></body></html>`
