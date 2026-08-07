package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/credentials"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/nleexport"
	"github.com/evjohn-icu/timingdex/internal/normalize"
	"github.com/evjohn-icu/timingdex/internal/remote"
	"github.com/evjohn-icu/timingdex/internal/webdavspace"
)

type Server struct {
	address             string
	service             *app.Service
	tlsCert             string
	tlsKey              string
	trustedReadNetworks []netip.Prefix
	webdav              *webdavspace.Manager
}

// SetWebDAVSpaceManager attaches the on-demand WebDAV space manager. When set,
// the server serves /spaces/<id>/... (Basic-Auth'd, read-only) for delivering
// footage to editing agents. Safe to leave nil in tests and minimal setups.
func (s *Server) SetWebDAVSpaceManager(m *webdavspace.Manager) {
	s.webdav = m
}

func NewServer(address string, service *app.Service) *Server {
	return newServer(address, service, "", "")
}

func NewTLSServer(address string, service *app.Service, certificateFile, keyFile string) *Server {
	return newServer(address, service, certificateFile, keyFile)
}

func newServer(address string, service *app.Service, certificateFile, keyFile string) *Server {
	s := &Server{address: address, service: service, tlsCert: certificateFile, tlsKey: keyFile, trustedReadNetworks: defaultTrustedReadNetworks}
	if service == nil {
		return s
	}
	// An explicit allowlist replaces the defaults outright rather than adding
	// to them: an operator narrowing the range to one subnet must not silently
	// keep the whole RFC1918 space. An unusable value keeps the restrictive
	// defaults — config.Load has already rejected it, so this only ever fires
	// for a Server built outside the normal startup path.
	if prefixes, err := service.TrustedReadNetworks(); err == nil && len(prefixes) > 0 {
		s.trustedReadNetworks = prefixes
	}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.address,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
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
	if s.webdav != nil {
		mux.Handle("/spaces/", s.webdav.Handler())
	}
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/hardware", s.requireTrustedRead(s.hardwareReport))
	mux.HandleFunc("GET /api/v1/agent/capabilities", s.agentCapabilities)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/roots", s.requireHubAdmin(s.listRoots))
	mux.HandleFunc("POST /api/v1/hub/worker-pairings", s.requireHubAdmin(s.createWorkerPairing))
	mux.HandleFunc("GET /api/v1/admin/webdav/accounts", s.requireHubAdmin(s.listWebDAVAccounts))
	mux.HandleFunc("POST /api/v1/admin/webdav/accounts", s.requireHubAdmin(s.createWebDAVAccount))
	mux.HandleFunc("DELETE /api/v1/admin/webdav/accounts/{username}", s.requireHubAdmin(s.deleteWebDAVAccount))
	mux.HandleFunc("POST /api/v1/admin/webdav/spaces", s.requireHubAdmin(s.createWebDAVSpace))
	mux.HandleFunc("GET /api/v1/admin/webdav/spaces", s.requireHubAdmin(s.listWebDAVSpaces))
	mux.HandleFunc("DELETE /api/v1/admin/webdav/spaces/{id}", s.requireHubAdmin(s.deleteWebDAVSpace))
	mux.HandleFunc("POST /api/v1/admin/webdav/spaces/{id}/links", s.requireHubAdmin(s.linkWebDAVAsset))
	mux.HandleFunc("GET /api/v1/hub/workers", s.requireHubAdmin(s.listWorkers))
	mux.HandleFunc("GET /api/v1/admin/provider-channels", s.requireHubAdmin(s.listProviderChannels))
	mux.HandleFunc("GET /api/v1/admin/provider-channels/status", s.requireHubAdmin(s.providerChannelRuntimeStatus))
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
	mux.HandleFunc("POST /api/v1/roots/inspect", s.requireHubAdmin(s.inspectRoot))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("GET /progress", s.progressPage)
	mux.HandleFunc("GET /workers", s.workersPage)
	mux.HandleFunc("GET /library-roots", s.libraryRootsPage)
	mux.HandleFunc("GET /repurpose", s.repurposePage)
	mux.HandleFunc("GET /tags", s.tagsPage)
	mux.HandleFunc("GET /providers", s.providersPage)
	mux.HandleFunc("GET /api/v1/assets", s.requireTrustedRead(s.listAssetCards))
	mux.HandleFunc("GET /api/v1/library/processing-summary", s.requireTrustedRead(s.processingSummary))
	mux.HandleFunc("GET /api/v1/collections", s.requireTrustedRead(s.listCollections))
	mux.HandleFunc("GET /api/v1/collections/{id}", s.requireTrustedRead(s.getCollection))
	mux.HandleFunc("GET /api/v1/collections/{id}/assets", s.requireTrustedRead(s.listCollectionAssets))
	mux.HandleFunc("POST /api/v1/collections", s.requireHubAdmin(s.saveCollection))
	mux.HandleFunc("DELETE /api/v1/collections/{id}", s.requireHubAdmin(s.deleteCollection))
	mux.HandleFunc("GET /api/v1/shoot-sessions", s.requireTrustedRead(s.listShootSessions))
	mux.HandleFunc("GET /api/v1/assets/{id}", s.requireTrustedRead(s.assetDetail))
	mux.HandleFunc("GET /api/v1/assets/{id}/shots", s.requireTrustedRead(s.assetShots))
	mux.HandleFunc("GET /api/v1/assets/{id}/thumbnail", s.requireTrustedRead(s.assetThumbnail))
	mux.HandleFunc("GET /api/v1/assets/{id}/proxy", s.requireTrustedRead(s.assetProxy))
	// Queue status is readable without a token from a trusted network; the
	// failure text stays admin-only regardless — see jobView.
	mux.HandleFunc("GET /api/v1/jobs", s.requireTrustedRead(s.listJobs))
	mux.HandleFunc("GET /api/v1/jobs/summary", s.requireTrustedRead(s.jobSummary))
	mux.HandleFunc("GET /api/v1/admin/worker-jobs/{id}", s.requireHubAdmin(s.workerJobStatus))
	mux.HandleFunc("POST /api/v1/admin/worker-jobs/{id}/assignment", s.requireHubAdmin(s.setWorkerJobAssignment))
	mux.HandleFunc("POST /api/v1/pipeline/run", s.requireHubAdmin(s.runPipeline))
	mux.HandleFunc("POST /api/v1/pipeline/retry-failed", s.requireHubAdmin(s.retryFailedJobs))
	mux.HandleFunc("POST /api/v1/pipeline/resume-deferred", s.requireHubAdmin(s.resumeDeferredJobs))
	// Same reasoning as /api/v1/jobs: the schedule is ordinary status the
	// progress page polls without a token, and only the failure text is held
	// back from unauthenticated callers.
	mux.HandleFunc("GET /api/v1/pipeline/supervisor", s.requireTrustedRead(s.librarySupervisorStatus))
	// Readable from a trusted network so the settings page can show the current
	// limits without a token; changing them is administrative.
	mux.HandleFunc("GET /api/v1/pipeline/throttle", s.requireTrustedRead(s.getPipelineThrottle))
	mux.HandleFunc("PUT /api/v1/pipeline/throttle", s.requireHubAdmin(s.savePipelineThrottle))
	mux.HandleFunc("GET /settings", s.settingsPage)
	s.registerWorkerSetupRoutes(mux)
	mux.HandleFunc("GET /api/v1/search", s.requireTrustedRead(s.search))
	mux.HandleFunc("GET /api/v1/search/shots", s.requireTrustedRead(s.searchShots))
	mux.HandleFunc("GET /api/v1/search/shots/hybrid", s.requireTrustedRead(s.hybridSearchShots))
	mux.HandleFunc("GET /api/v1/shots/{id}/similar", s.requireTrustedRead(s.similarShots))
	mux.HandleFunc("GET /api/v1/discover/rare-shots", s.requireTrustedRead(s.rareShots))
	mux.HandleFunc("GET /api/v1/tags", s.requireTrustedRead(s.listTags))
	mux.HandleFunc("GET /api/v1/tags/unresolved", s.requireTrustedRead(s.listUnresolvedTags))
	mux.HandleFunc("POST /api/v1/tags/curate", s.requireHubAdmin(s.runTagCurator))
	mux.HandleFunc("POST /api/v1/tags/clusters", s.requireHubAdmin(s.runTagEmbeddingClusters))
	mux.HandleFunc("GET /api/v1/tags/proposals", s.requireTrustedRead(s.listTagProposals))
	mux.HandleFunc("POST /api/v1/tags/proposals/{id}/review", s.requireHubAdmin(s.reviewTagProposal))
	mux.HandleFunc("GET /api/v1/library/summary", s.requireTrustedRead(s.latestLibrarySummary))
	mux.HandleFunc("POST /api/v1/library/summary/generate", s.requireHubAdmin(s.generateLibrarySummary))
	mux.HandleFunc("POST /api/v1/repurpose/plans", s.requireAgentOrAdmin(s.createRepurposePlan))
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}", s.requireTrustedRead(s.getRepurposePlan))
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}/revisions", s.requireTrustedRead(s.listRepurposePlanRevisions))
	mux.HandleFunc("POST /api/v1/repurpose/plans/{id}/revisions", s.requireAgentOrAdmin(s.reviseRepurposePlan))
	mux.HandleFunc("POST /api/v1/repurpose/plans/{id}/revisions/{revision}/approve", s.requireHubAdmin(s.approveRepurposePlanRevision))
	// Export is administrator-only, and deliberately not reachable with the
	// agent token even though the plan it renders is readable with one. An
	// FCPXML names the absolute path of every original file, which is the one
	// thing access_original_media_paths denies; an EDL does not, but it is the
	// artifact someone cuts with, so both sit on the human side of the line.
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}/export.edl", s.requireHubAdmin(s.exportRepurposePlanEDL))
	mux.HandleFunc("GET /api/v1/repurpose/plans/{id}/export.fcpxml", s.requireHubAdmin(s.exportRepurposePlanFCPXML))

	return requestLogger(mux)
}

// isHubAdmin reports whether the request carries the Hub admin token. Routes
// that are wholly administrative use requireHubAdmin; routes that are public but
// hold one privileged field call this directly to decide how much to disclose.
func (s *Server) isHubAdmin(r *http.Request) bool {
	const scheme = "Bearer "
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, scheme) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, scheme))
	expected := s.service.AdminToken()
	if provided == "" || expected == "" || len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (s *Server) requireHubAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isHubAdmin(r) {
			http.Error(w, "Hub administrator authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// isHubAgent reports whether the request carries the Hub agent token — a
// distinct, narrower credential from the admin token (see
// app.Service.AgentToken). It authorizes only the draft-plan routes wrapped in
// requireAgentOrAdmin; it must never be accepted by requireHubAdmin routes
// such as approve or pipeline/run.
func (s *Server) isHubAgent(r *http.Request) bool {
	const scheme = "Bearer "
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, scheme) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, scheme))
	expected := s.service.AgentToken()
	if provided == "" || expected == "" || len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// requireAgentOrAdmin accepts either the agent token or the admin token. An
// admin must never be blocked from an action an agent may take, so this is
// strictly an OR, never a replacement for requireHubAdmin: only the two
// draft-plan routes documented in skills/timingdex use it. Approval and
// pipeline runs stay behind requireHubAdmin so the agent token can never
// reach them, keeping CLAUDE.md's "approval is human-only" boundary enforced
// by access control rather than by prompt text alone.
func (s *Server) requireAgentOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isHubAgent(r) && !s.isHubAdmin(r) {
			http.Error(w, "Hub agent or administrator authentication required", http.StatusUnauthorized)
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

func (s *Server) listWebDAVAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.service.ListWebDAVAccounts(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if accounts == nil {
		accounts = []string{}
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (s *Server) createWebDAVAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.service.CreateWebDAVAccount(r.Context(), req.Username, req.Password); err != nil {
		switch {
		case errors.Is(err, app.ErrWebDAVAccountInvalid):
			http.Error(w, "username and password are required", http.StatusBadRequest)
		case errors.Is(err, app.ErrWebDAVAccountExists):
			http.Error(w, "account already exists", http.StatusConflict)
		default:
			writeError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) deleteWebDAVAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteWebDAVAccount(r.Context(), r.PathValue("username")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createWebDAVSpace(w http.ResponseWriter, r *http.Request) {
	spaceID, err := s.service.CreateWebDAVSpace(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"space_id": spaceID})
}

func (s *Server) listWebDAVSpaces(w http.ResponseWriter, r *http.Request) {
	spaces := s.service.ListWebDAVSpaces()
	if spaces == nil {
		spaces = []string{}
	}
	writeJSON(w, http.StatusOK, spaces)
}

func (s *Server) deleteWebDAVSpace(w http.ResponseWriter, r *http.Request) {
	if err := s.service.RevokeWebDAVSpace(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) linkWebDAVAsset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AssetID string `json:"asset_id"`
		Kind    string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	path, err := s.service.LinkWebDAVAsset(r.Context(), r.PathValue("id"), req.AssetID, req.Kind)
	if err != nil {
		switch {
		case errors.Is(err, app.ErrWebDAVSpaceNotFound):
			http.Error(w, "unknown space", http.StatusNotFound)
		case errors.Is(err, app.ErrWebDAVLinkKindInvalid):
			http.Error(w, "kind must be original or proxy", http.StatusBadRequest)
		case strings.TrimSpace(req.AssetID) == "":
			http.Error(w, "asset_id is required", http.StatusBadRequest)
		default:
			writeError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
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

// providerChannelRuntimeStatus answers "what is the Hub actually doing about
// a provider route right now" -- distinct from listProviderChannels, which
// only echoes the provider_channels table. A key that has retired itself
// (providerpool.MemberSpent on a 401/402/403) narrows a route silently; this
// is the only place that state becomes visible outside process memory. See
// app.ProviderChannelCapabilityStatus for why a capability nothing has
// routed through yet reports has_runtime_data=false rather than a fabricated
// all-healthy snapshot.
func (s *Server) providerChannelRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	statuses := s.service.ProviderChannelRuntimeStatus(r.Context())
	if capability := strings.TrimSpace(r.URL.Query().Get("capability")); capability != "" {
		filtered := make([]app.ProviderChannelCapabilityStatus, 0, len(statuses))
		for _, status := range statuses {
			if string(status.Capability) == capability {
				filtered = append(filtered, status)
			}
		}
		statuses = filtered
	}
	writeJSON(w, http.StatusOK, statuses)
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
	// keys is positional, aligned with channel.Members below. Labels are
	// unique per channel (UNIQUE(channel_id, label), migration 0013), so
	// keys is positional because Members itself is positional — not because
	// two members could share a label: a label-keyed map here would still
	// let one input silently clobber or misassign another member's key
	// ahead of SaveProviderChannel's duplicate-label check.
	keys := make([]string, 0, len(input.Members))
	for _, member := range input.Members {
		channel.Members = append(channel.Members, domain.ProviderChannelMember{ID: member.ID, Label: strings.TrimSpace(member.Label), Enabled: member.Enabled, Weight: member.Weight, MaxInflight: member.MaxInflight})
		keys = append(keys, strings.TrimSpace(member.APIKey))
	}
	saved, err := s.service.SaveProviderChannel(r.Context(), channel, keys)
	if err != nil {
		// Only a validation failure's text was written to be shown to an
		// operator. SaveProviderChannel also calls UpsertProviderChannel and
		// the secret store, and neither of those errors is safe to echo — a
		// duplicate label used to reach this response as a bare SQLite
		// UNIQUE-constraint string.
		if errors.Is(err, app.ErrProviderChannelValidation) {
			http.Error(w, "provider channel rejected: "+err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, "provider channel rejected", http.StatusBadRequest)
		}
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
		// Same split as the create handler: a validation failure (e.g. two
		// patched members sharing a label) names the label so the operator
		// can fix it; anything else keeps the generic message rather than
		// echoing a downstream layer's error text.
		if errors.Is(err, app.ErrProviderChannelValidation) {
			http.Error(w, "provider channel update rejected: "+err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, "provider channel update rejected", http.StatusBadRequest)
		}
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
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if !decodeStrictJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.PairingToken) == "" {
		http.Error(w, "invalid worker enrollment", http.StatusBadRequest)
		return
	}
	worker, token, err := s.service.EnrollWorker(r.Context(), request.PairingToken, request.WorkerRegistration)
	if err != nil {
		if errors.Is(err, app.ErrPairingTokenInvalid) {
			http.Error(w, "worker enrollment rejected", http.StatusUnauthorized)
		} else {
			writeError(w, err)
		}
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
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !decodeStrictJSON(w, r, &request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	// Enforce the body size limit even though this endpoint does not use the body.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid request body", http.StatusBadRequest)
		}
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
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if !decodeStrictJSON(w, r, &request) {
		return
	}
	// Validate state before calling the service so an invalid state is a
	// 400, not a 500 from writeError.
	if request.State != domain.JobSucceeded && request.State != domain.JobFailed {
		http.Error(w, "worker job state must be succeeded or failed", http.StatusBadRequest)
		return
	}
	if err := s.service.CompleteWorkerJob(r.Context(), r.PathValue("id"), worker.ID, request.State, request.Message); err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeError(w, err)
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
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !decodeStrictJSON(w, r, &request) {
		return
	}
	if err := s.service.RecordWorkerJobProgress(r.Context(), r.PathValue("id"), worker.ID, strings.TrimSpace(request.Stage), request.Progress, strings.TrimSpace(request.Event), strings.TrimSpace(request.Message)); err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
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
		if errors.Is(err, domain.ErrJobLeaseLost) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// The operation name is already the Worker's own path parameter, so
		// echoing it back carries nothing it didn't already know. Neither
		// message is built from err.Error(): ErrWorkerProviderConfiguredAsChannelOnly
		// and ErrWorkerProviderNotConfigured are wrapped with operation/provider
		// names only (see their doc comments in internal/app/service.go and
		// credentials.Broker.resolve/Issue), but the fixed wording here is
		// chosen deliberately, not merely because the wrapped text happens to
		// be safe -- it keeps a maintainer's edit to the sentinel's own
		// errors.New string from silently changing this response.
		if errors.Is(err, app.ErrWorkerProviderConfiguredAsChannelOnly) {
			// 403, not 400: the Worker's request is well-formed and the
			// capability is genuinely configured -- Worker direct-credential
			// access deliberately never reads provider channels (CLAUDE.md's
			// Worker trust boundary), so this is a standing policy refusal for
			// this capability via this route, the same shape as the
			// AllowWorkerProviderCredentials-disabled 403 above, not a
			// malformed request.
			http.Error(w, fmt.Sprintf("worker provider credential rejected: capability %q is configured as a provider channel, which worker direct-credential access does not read; configure providers.* for this capability or use the Hub provider proxy instead", operation), http.StatusForbidden)
			return
		}
		if errors.Is(err, app.ErrWorkerProviderNotConfigured) {
			// 503, not 400: nothing about the Worker's request is wrong --
			// the Hub simply has no provider for this capability yet, by
			// either configuration method. An operator configuring one later
			// makes the identical request succeed, which is what 503 signals
			// and 400 does not.
			http.Error(w, fmt.Sprintf("worker provider credential rejected: no provider is configured for capability %q", operation), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "credential request rejected", http.StatusBadRequest)
		return
	}
	// Deliberately do not log the lease or any response fields here: it carries
	// an in-memory API key for the authenticated Worker.
	//
	// Credential.MarshalJSON redacts api_key to [redacted] as a safety net
	// against accidental serialisation. This handler is the authorised delivery
	// path for an explicitly trusted Worker that has cleared authentication and
	// lease-ownership checks, so it serialises the real key. The local type
	// alias credentialForDelivery bypasses MarshalJSON.
	type credentialForDelivery credentials.Credential
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct {
		JobID      string                `json:"job_id"`
		WorkerID   string                `json:"worker_id"`
		Provider   string                `json:"provider"`
		Operation  credentials.Operation `json:"operation"`
		ExpiresAt  string                `json:"expires_at"`
		Credential credentialForDelivery `json:"credential"`
	}{
		JobID:      lease.JobID,
		WorkerID:   lease.WorkerID,
		Provider:   lease.Provider,
		Operation:  lease.Operation,
		ExpiresAt:  lease.ExpiresAt,
		Credential: credentialForDelivery(lease.Credential),
	})
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
		if errors.Is(err, domain.ErrJobLeaseLost) {
			http.Error(w, "worker does not own active job for provider proxy", http.StatusConflict)
			return
		}
		// Same split, same reasoning and same status codes as workerCredential's
		// classification above: the operation name is the Worker's own path
		// parameter, so the message is built from it rather than from
		// err.Error(), and 403/503 replace the old flat 400 because neither
		// sentinel means the Worker's request was wrong.
		if errors.Is(err, app.ErrWorkerProviderConfiguredAsChannelOnly) {
			http.Error(w, fmt.Sprintf("provider proxy request rejected: capability %q is configured as a provider channel, which the worker provider proxy does not read; configure providers.* for this capability instead", operation), http.StatusForbidden)
			return
		}
		if errors.Is(err, app.ErrWorkerProviderNotConfigured) {
			http.Error(w, fmt.Sprintf("provider proxy request rejected: no provider is configured for capability %q", operation), http.StatusServiceUnavailable)
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
		// version is the agent contract version, not the product version. The
		// skills/timingdex package — its SKILL.md and
		// references/api-contract.md, titled "Timingdex v0.13 Local Agent API
		// Contract" — is written against this exact string, and server_test.go
		// pins it, so it only moves when the contract itself changes: a route,
		// an action, or a field in this document. It must not track Hub
		// releases; the product is at v0.21 while this stays v0.13, and the
		// gap is the contract not having changed, not this endpoint being
		// stale. Bumping it means re-versioning and re-validating the skills
		// package in the same change.
		"version":       "v0.13",
		"approval_mode": "human_required",
		"auth": map[string]any{
			"header": "Authorization: Bearer <agent-token>",
			"note": "The agent token is a credential distinct from the Hub " +
				"administrator token. It is accepted only on the two routes " +
				"listed in allowed_write_routes below; every other write route, " +
				"including plan approval and pipeline runs, rejects it with 401 " +
				"and requires the Hub administrator token instead. Read routes " +
				"need no credential from a trusted network (loopback, RFC1918, " +
				"CGNAT) and return 403 elsewhere unless the agent token is sent.",
		},
		"allowed_actions": []string{
			"inspect_readiness",
			"search_shots",
			"create_draft_plan",
			"inspect_plan",
			"revise_draft_plan",
		},
		"allowed_write_routes": []string{
			"POST /api/v1/repurpose/plans",
			"POST /api/v1/repurpose/plans/{id}/revisions",
		},
		"denied_actions": []string{
			"approve_plan",
			"run_pipeline",
			"read_provider_keys",
			"access_original_media_paths",
			"export_timeline",
			"manage_tags",
			"manage_collections",
			"manage_webdav_accounts",
			"manage_webdav_spaces",
			"manage_roots",
			"manage_workers",
			"manage_provider_channels",
			"manage_pipeline_throttle",
			"manage_library_summary",
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil || request.Path == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	root, err := s.service.AddLibraryRoot(r.Context(), request.Path)
	if err != nil {
		var shareErr app.ErrShareNotMounted
		if errors.As(err, &shareErr) {
			// A bare 500 here would be indistinguishable from a stat failure or a
			// permissions problem, and an API caller that never opens
			// /library-roots still deserves the mount commands rather than a dead
			// end — so this gets its own status and a body carrying the same
			// inspection the wizard would have shown before the operator ever
			// tried to add the root.
			writeJSON(w, http.StatusUnprocessableEntity, shareNotMountedResponse{
				Error:      err.Error(),
				Inspection: s.service.InspectRootPath(r.Context(), request.Path, ""),
			})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, root)
}

// shareNotMountedResponse is what createRoot answers with when AddLibraryRoot
// finds a network share where a mounted path was expected.
type shareNotMountedResponse struct {
	Error      string             `json:"error"`
	Inspection app.RootInspection `json:"inspection"`
}

func (s *Server) scanRoot(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ScanLibraryRoot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// inspectRoot is the read-only counterpart to createRoot: it answers whether a
// path parses as a network share (with the exact mount commands when it does),
// whether the path exists and is a directory, and what its filesystem looks
// like — everything the /library-roots wizard needs before it ever commits to
// adding a root. It never accepts a password (mount.Share has nowhere to put
// one) and never reports on anything beyond the single path given: no
// directory listing, no globbing.
func (s *Server) inspectRoot(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path       string `json:"path"`
		Mountpoint string `json:"mountpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&request); err != nil || strings.TrimSpace(request.Path) == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, s.service.InspectRootPath(r.Context(), request.Path, request.Mountpoint))
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// facetQueryFields pairs each controlled-vocabulary facet with the vocabulary
// it must be drawn from, listed once so the browse endpoint and the three
// shot-search endpoints can't drift out of sync on param names or allowed
// values.
//
// The params are asset_* names because that is what they filter: all six
// fields live only in asset_analysis, one row per asset, so a shot search
// resolves them through the shot's own asset (see domain.FacetFilter). The
// un-prefixed legacy names remain accepted as aliases so existing callers and
// bookmarked URLs keep working; new UI and new callers should use the
// asset_* names to avoid implying shot-level truth. When both names are
// present, the asset_* parameter wins and the alias is ignored.
var facetQueryFields = []struct {
	param  string
	alias  string
	values []string
}{
	{"asset_type", "", normalize.AssetTypeValues},
	{"asset_shot_size", "shot_size", normalize.ShotSizeValues},
	{"asset_camera_motion", "camera_motion", normalize.MotionValues},
	{"asset_audio_type", "audio_type", normalize.AudioTypeValues},
	{"asset_quality", "quality", normalize.QualityValues},
	{"asset_usable_as", "usable_as", normalize.UsableAsValues},
}

// parseFacetFilter reads the controlled-vocabulary and duration query
// parameters shared by the asset browse endpoint and the three shot-search
// endpoints (see domain.FacetFilter). A facet parameter takes a
// comma-separated list of values that are OR'd together; different facet
// parameters are AND'd by the caller's SQL. An unrecognized value is
// rejected here — a typo must 400, not silently compile into a WHERE clause
// that matches nothing and reads as "you have no footage".
func parseFacetFilter(query url.Values) (domain.FacetFilter, error) {
	var f domain.FacetFilter
	targets := map[string]*[]string{
		"asset_type":          &f.AssetTypes,
		"asset_shot_size":     &f.ShotSizes,
		"asset_camera_motion": &f.CameraMotions,
		"asset_audio_type":    &f.AudioTypes,
		"asset_quality":       &f.Qualities,
		"asset_usable_as":     &f.UsableAs,
	}
	for _, spec := range facetQueryFields {
		raw := strings.TrimSpace(query.Get(spec.param))
		if raw == "" && spec.alias != "" {
			raw = strings.TrimSpace(query.Get(spec.alias))
		}
		if raw == "" {
			continue
		}
		allowed := make(map[string]bool, len(spec.values))
		for _, v := range spec.values {
			allowed[v] = true
		}
		var values []string
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !allowed[part] {
				return domain.FacetFilter{}, fmt.Errorf("invalid %s value: %q", spec.param, part)
			}
			values = append(values, part)
		}
		*targets[spec.param] = values
	}
	if raw := strings.TrimSpace(query.Get("min_duration_ms")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return domain.FacetFilter{}, fmt.Errorf("invalid min_duration_ms value: %q", raw)
		}
		f.MinDurationMS = &v
	}
	if raw := strings.TrimSpace(query.Get("max_duration_ms")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return domain.FacetFilter{}, fmt.Errorf("invalid max_duration_ms value: %q", raw)
		}
		f.MaxDurationMS = &v
	}
	// An inverted range is the same class of mistake as a misspelled facet
	// value: both parse cleanly and compile into SQL that matches nothing, and
	// an empty result is indistinguishable from "you have no footage". Equal
	// bounds stay valid — both ends are inclusive, so that is a legitimate
	// exact-duration query, not an empty one. The check sits after both values
	// have parsed so an unparseable bound still reports as unparseable.
	if f.MinDurationMS != nil && f.MaxDurationMS != nil && *f.MinDurationMS > *f.MaxDurationMS {
		return domain.FacetFilter{}, fmt.Errorf("invalid duration range: min_duration_ms=%d exceeds max_duration_ms=%d", *f.MinDurationMS, *f.MaxDurationMS)
	}
	return f, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// pathInsideRoot returns true when localPath, after symlink resolution,
// is inside the directory root. Both paths are resolved with
// filepath.EvalSymlinks before computing the relative path, so symlink
// escapes and .. traversal are both caught.
func pathInsideRoot(localPath, root string) bool {
	resolvedLocal, err := filepath.EvalSymlinks(localPath)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedLocal)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func writeError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// decodeStrictJSON decodes exactly one JSON value from r.Body (already
// wrapped with http.MaxBytesReader by the caller) and rejects any trailing
// bytes — including bytes already buffered inside json.Decoder that a
// separate drainBody would miss. A successful second Decode (non-EOF) or a
// non-EOF decode error both indicate trailing content: *http.MaxBytesError
// → 413, anything else → 400.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid request body", http.StatusBadRequest)
		}
		return false
	}
	// Second Decode on the same decoder to catch trailing content, including
	// bytes json.Decoder buffered from the underlying reader.
	var dummy struct{}
	if err := dec.Decode(&dummy); err == nil {
		// Another JSON value parsed successfully — trailing content.
		http.Error(w, "request body has trailing content", http.StatusBadRequest)
		return false
	} else if !errors.Is(err, io.EOF) {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "request body has trailing content", http.StatusBadRequest)
		}
		return false
	}
	return true
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}

// jobView is a Job with its failure text gated. The queue's shape — types,
// states, attempt counts — is ordinary status any viewer of the library may see,
// but last_error_message can embed a truncated upstream provider response body,
// so the text itself stays behind the admin token and unauthenticated callers
// get only the boolean.
type jobView struct {
	domain.Job
	HasError bool `json:"has_error"`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.service.ListJobs(r.Context(), parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		writeError(w, err)
		return
	}
	admin := s.isHubAdmin(r)
	views := make([]jobView, 0, len(jobs))
	for _, job := range jobs {
		view := jobView{Job: job, HasError: strings.TrimSpace(job.LastError) != ""}
		if !admin {
			view.LastError = ""
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, views)
}

// jobSummary needs no admin gate even though listJobs withholds error text from
// anonymous callers: a count carries no upstream response body, only how much
// work exists and in what state, which is the same class of fact the job list's
// states and attempt counts already are.
func (s *Server) jobSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.service.JobSummary(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
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
	// Validate mode and worker_id before calling the service so an invalid
	// value is a 400, not a 500 from writeError.
	if input.Mode != remote.WorkerAssignmentAny && input.Mode != remote.WorkerAssignmentPreferred && input.Mode != remote.WorkerAssignmentRequired {
		http.Error(w, "unsupported worker assignment mode", http.StatusBadRequest)
		return
	}
	if input.Mode != remote.WorkerAssignmentAny && strings.TrimSpace(input.WorkerID) == "" {
		http.Error(w, "worker ID is required for a worker assignment", http.StatusBadRequest)
		return
	}
	if err := s.service.SetDeriveWorkerAssignment(r.Context(), r.PathValue("id"), strings.TrimSpace(input.WorkerID), input.Mode); err != nil {
		if errors.Is(err, domain.ErrJobNotAssignable) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, domain.ErrInvalidAssignment) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeError(w, err)
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

// librarySupervisorView adds the one bit an unauthenticated caller may know
// about a failed pass. The text itself follows jobView's rule: a scan error
// carries filesystem paths, and error strings are exactly where upstream detail
// leaks, so it stays behind the admin token.
type librarySupervisorView struct {
	app.LibrarySupervisorStatus
	HasError bool `json:"has_error"`
}

// librarySupervisorStatus is how an operator tells an unattended loop that is
// running from one that has quietly died. A background loop nobody can see is
// indistinguishable from one that stopped.
func (s *Server) librarySupervisorStatus(w http.ResponseWriter, r *http.Request) {
	status := s.service.LibrarySupervisorStatus()
	view := librarySupervisorView{LibrarySupervisorStatus: status, HasError: strings.TrimSpace(status.LastError) != ""}
	if !s.isHubAdmin(r) {
		view.LastError = ""
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) getPipelineThrottle(w http.ResponseWriter, r *http.Request) {
	throttle, err := s.service.PipelineThrottle(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now()
	// The derived fields exist so the page can explain the current state instead
	// of only echoing the configuration back: "held until 01:00" is actionable,
	// a window that happens to be shut is not.
	writeJSON(w, http.StatusOK, map[string]any{
		"throttle":      throttle,
		"off_peak_open": throttle.OffPeakOpenAt(now),
		"holding_above": throttle.MaxAssetBytesAt(now),
		"next_off_peak": nextOffPeakLabel(throttle, now),
		"server_time":   now.Format("15:04"),
		// Offset only: a zone with no abbreviation renders MST as the offset too,
		// which produced "+08+08:00".
		"server_zone":    "UTC" + now.Format("-07:00"),
		"cooldown_now_s": int(throttle.CooldownAt(now).Seconds()),
	})
}

func nextOffPeakLabel(throttle domain.PipelineThrottle, now time.Time) string {
	start, ok := throttle.NextOffPeakStart(now)
	if !ok {
		return ""
	}
	return start.Format("2006-01-02 15:04")
}

func (s *Server) savePipelineThrottle(w http.ResponseWriter, r *http.Request) {
	var throttle domain.PipelineThrottle
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&throttle); err != nil {
		http.Error(w, "invalid throttle payload", http.StatusBadRequest)
		return
	}
	if err := throttle.Validate(); err != nil {
		// A rejected throttle is an operator mistake, not a server fault, and the
		// message names the offending field — it is the only feedback the page has.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.service.SavePipelineThrottle(r.Context(), throttle); err != nil {
		writeError(w, err)
		return
	}
	s.getPipelineThrottle(w, r)
}

// retryFailedJobs is the escape hatch for work that failed for a reason the
// operator has since fixed — most often a provider that was not configured yet.
// Neither exhausted attempts nor a terminal classification is undone by
// rescanning, so without this the queue has no way back.
func (s *Server) retryFailedJobs(w http.ResponseWriter, r *http.Request) {
	requeued, err := s.service.RequeueFailedJobs(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"requeued": requeued})
}

func (s *Server) resumeDeferredJobs(w http.ResponseWriter, r *http.Request) {
	resumed, err := s.service.ResumeDeferredJobs(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"resumed": resumed})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The browser turns this id list into cards with a follow-up GET
	// /api/v1/assets?ids=..., which 400s past maxListedAssetIDs rather than
	// silently truncate. An uncapped limit here would just move that failure
	// one request later: search would "succeed" with a list the second leg
	// can never consume. Clamping instead of erroring is deliberate — unlike
	// ids=, which reflects a caller-supplied exact set where dropping members
	// is data loss the caller can't detect, a limit has always been a
	// best-effort cap, so capping it at the ceiling the next request enforces
	// isn't a new kind of loss, only today's existing one landing sooner.
	limit := parseInt(r.URL.Query().Get("limit"), 100)
	if limit > maxListedAssetIDs {
		limit = maxListedAssetIDs
	}
	ids, err := s.service.SearchFiltered(r.Context(), q, limit, facets)
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
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hits, err := s.service.SearchShotsFiltered(r.Context(), q, parseInt(r.URL.Query().Get("limit"), 100), facets)
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
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hits, err := s.service.HybridSearchShotsFiltered(r.Context(), q, parseInt(r.URL.Query().Get("limit"), 100), facets)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) similarShots(w http.ResponseWriter, r *http.Request) {
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hits, err := s.service.SimilarShotsFiltered(r.Context(), r.PathValue("id"), parseInt(r.URL.Query().Get("limit"), 20), facets)
	if err != nil {
		// Matched as a sentinel rather than by message prefix: this used to be
		// strings.HasPrefix(err.Error(), "shot not found:"), which made the
		// difference between 404 and 500 depend on repository wording that no
		// compiler checks. Rewording the error there — ordinary maintenance —
		// silently downgraded a missing shot to a 500.
		if errors.Is(err, domain.ErrShotVectorNotFound) {
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

// maxListedAssetIDs bounds GET /api/v1/assets?ids=... . The endpoint exists
// so a facet-aware /api/v1/search result can be rendered by id instead of
// re-derived from a capped card listing (see parseFacetFilter's doc comment
// and Repository.SearchFiltered); silently truncating an over-cap id list
// would reintroduce the same silent loss this endpoint was added to remove,
// so parseAssetIDs 400s instead.
const maxListedAssetIDs = 200

// parseAssetIDs reads the comma-separated "ids" query parameter. An absent
// or empty value returns (nil, nil) — unset, matching every asset — because
// an empty-but-present ids filter would otherwise be indistinguishable from
// "match nothing", which is not a state a caller can usefully ask for (a
// client that computed zero search hits should skip calling this endpoint
// rather than send ids= empty).
func parseAssetIDs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var ids []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ids = append(ids, part)
	}
	if len(ids) > maxListedAssetIDs {
		return nil, fmt.Errorf("too many ids: %d exceeds limit of %d", len(ids), maxListedAssetIDs)
	}
	return ids, nil
}

func (s *Server) listAssetCards(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	facets, err := parseFacetFilter(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ids, err := parseAssetIDs(query.Get("ids"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A caller narrowing by ids without an explicit limit gets a default sized
	// to the id list, not the ordinary browse default of 100: an id set larger
	// than 100 (up to maxListedAssetIDs) silently truncating back to 100 would
	// reintroduce, one field over, the exact silent-loss bug ids= was added to
	// remove. An explicit limit still wins either way.
	defaultLimit := 100
	if len(ids) > 0 && strings.TrimSpace(query.Get("limit")) == "" {
		defaultLimit = len(ids)
	}
	filter := domain.AssetCardFilter{
		Limit:       parseInt(query.Get("limit"), defaultLimit),
		Offset:      parseInt(query.Get("offset"), 0),
		RegionLabel: query.Get("region"),
		CameraModel: query.Get("camera"),
		SessionID:   query.Get("session"),
		Status:      domain.ProcessingStatus(query.Get("status")),
		Facets:      facets,
		IDs:         ids,
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

func collectionFilterFromQuery(query map[string][]string) (domain.AssetCollectionFilter, error) {
	filter := domain.AssetCollectionFilter{
		RegionLabel: queryValue(query, "region"),
		CameraModel: queryValue(query, "camera"),
		SessionID:   queryValue(query, "session"),
		Status:      domain.ProcessingStatus(queryValue(query, "status")),
	}
	facets, err := parseFacetFilter(url.Values(query))
	if err != nil {
		return domain.AssetCollectionFilter{}, err
	}
	filter.FacetFilter = facets
	// An unparseable date is the same failure class as an invalid facet
	// value — both would silently compile into a WHERE clause that matches
	// nothing and read as "no footage" — so it must error and 400, matching
	// parseFacetFilter's decision. Empty stays unset: the browser's date
	// input either sends nothing or a valid "2006-01-02" value.
	if value := queryValue(query, "date_from"); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			return domain.AssetCollectionFilter{}, fmt.Errorf("invalid date_from value: %q", value)
		}
		filter.CapturedFrom = &parsed
	}
	if value := queryValue(query, "date_to"); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			return domain.AssetCollectionFilter{}, fmt.Errorf("invalid date_to value: %q", value)
		}
		parsed = parsed.AddDate(0, 0, 1)
		filter.CapturedTo = &parsed
	}
	return filter, nil
}

func queryValue(query map[string][]string, key string) string {
	if values := query[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func (s *Server) processingSummary(w http.ResponseWriter, r *http.Request) {
	filter, err := collectionFilterFromQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	summary, err := s.service.GetAssetProcessingSummary(r.Context(), filter)
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
	// Validate required fields before calling the service so an invalid
	// value is a 400/422, not a 500 from writeError.
	collection.Name = strings.TrimSpace(collection.Name)
	if collection.Name == "" {
		http.Error(w, "collection name is required", http.StatusBadRequest)
		return
	}
	if len(collection.Name) > 200 {
		http.Error(w, "collection name is too long", http.StatusUnprocessableEntity)
		return
	}
	if len(collection.Description) > 2000 {
		http.Error(w, "collection description is too long", http.StatusUnprocessableEntity)
		return
	}
	saved, err := s.service.SaveAssetCollection(r.Context(), collection)
	if err != nil {
		if errors.Is(err, domain.ErrCollectionExists) {
			http.Error(w, "collection name already exists", http.StatusConflict)
			return
		}
		writeError(w, err)
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
	// Verify the artifact lives inside the Hub data directory or its
	// cache directory. Derived artifacts (thumbnails, proxies) live under
	// CacheDir which defaults to DataDir/cache but may be configured to
	// an independent location. Check both roots.
	//
	// Use filepath.Rel after resolving symlinks so that .. traversal and
	// symlink escapes are both caught, and an empty root fails open (no
	// path can be relative to nothing).
	roots := []string{s.service.DataDir(), s.service.CacheDir()}
	var allowed bool
	for _, root := range roots {
		if root == "" {
			continue
		}
		if ok := pathInsideRoot(a.LocalPath, root); ok {
			allowed = true
			break
		}
	}
	if !allowed {
		slog.Warn("artifact path is outside allowed roots", "path", a.LocalPath, "datadir", s.service.DataDir(), "cachedir", s.service.CacheDir())
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(a.LocalPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, a.LocalPath)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// Belt-and-suspenders: the mux pattern "GET /{$}" already guarantees r.URL.Path == "/",
	// but matching on the path explicitly guards against misregistration or future pattern changes.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.Replace(libraryIndexHTML, `<a href="/tags">Tag Curator</a>`, `<a href="/tags">Tag Curator</a><a href="/providers">能力与服务</a>`, 1)
	_, _ = w.Write([]byte(brandedPage(page)))
}

const legacyLibraryIndexHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 素材库</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#0c1422;color:#edf3ff;font:14px ui-sans-serif,system-ui,-apple-system,sans-serif}header{position:sticky;top:0;z-index:5;display:flex;align-items:center;gap:18px;padding:15px max(24px,5vw);background:#0c1422ee;backdrop-filter:blur(15px);border-bottom:1px solid #273750}a{color:#b7c8eb;text-decoration:none;font-size:14px}.brand{font-size:16px;font-weight:850;color:#fff;margin-right:auto;letter-spacing:-.02em}.nav-active{color:#fff}.search{width:min(360px,31vw);display:flex;gap:7px}input{min-width:0;flex:1;padding:10px 11px;border:1px solid #354965;border-radius:9px;background:#111d30;color:#fff;font:inherit}button{border:0;border-radius:9px;padding:10px 13px;background:#324767;color:#eaf1ff;font:inherit;font-weight:750;cursor:pointer}button.primary{background:#91a9ff;color:#0a1324}.wrap{width:min(1440px,100%);margin:auto;padding:36px max(24px,5vw) 64px}.top{display:flex;justify-content:space-between;gap:28px;align-items:end;margin-bottom:26px}.eyebrow{color:#93aaff;font-size:11px;font-weight:850;letter-spacing:.12em}.top h1{margin:8px 0 7px;font-size:clamp(31px,4vw,50px);line-height:1;letter-spacing:-.05em}.muted{margin:0;color:#9fb0ce;line-height:1.6}.legend{display:flex;gap:12px;color:#9fb0ce;font-size:12px;align-items:center}.legend i{width:8px;height:8px;border-radius:50%;display:inline-block;background:#95aaff}.legend i:nth-child(2){background:#70d7b1}.legend i:nth-child(3){background:#ffc783}.library{display:grid;gap:13px}.asset-row{display:grid;grid-template-columns:220px minmax(220px,.75fr) minmax(380px,1.75fr);gap:18px;align-items:stretch;padding:13px;background:#121f34;border:1px solid #2b3e5b;border-radius:16px;transition:border-color .15s,transform .15s}.asset-row:hover{border-color:#506e9d;transform:translateY(-1px)}.thumb,.thumb-empty{width:100%;height:100%;min-height:126px;aspect-ratio:16/9;object-fit:cover;border-radius:10px;background:#070d17}.thumb-empty{display:grid;place-items:center;color:#8193b0;font-size:12px;border:1px dashed #3e536f}.asset-info{display:flex;min-width:0;flex-direction:column;justify-content:center;padding:4px 0}.filename{font-size:16px;font-weight:800;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;letter-spacing:-.02em}.asset-meta{margin-top:7px;color:#a9bad7;font-size:12px}.summary{margin:10px 0;color:#d8e2f4;line-height:1.45}.chips{display:flex;flex-wrap:wrap;gap:5px}.chip{border-radius:99px;padding:4px 7px;background:#223653;color:#b8caef;font-size:11px}.chip.voice{color:#9bf0c6;background:#173e36}.asset-details{display:flex;flex-wrap:wrap;gap:5px;margin-top:8px}.detail{font-size:11px;color:#a9bad7;background:#172a43;border-radius:6px;padding:3px 6px}.detail b{color:#d4e0f8;margin-right:4px}.timeline-card{min-width:0;display:flex;flex-direction:column;justify-content:center;padding:4px 3px}.timeline-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:10px}.timeline-title{font-size:12px;font-weight:800;color:#c9d7ed}.duration{color:#91a3c1;font-size:12px;font-variant-numeric:tabular-nums}.semantic-timeline{position:relative;height:94px;border-radius:10px;border:1px solid #334967;background:linear-gradient(90deg,#0d1727 0%,#111e32 50%,#0d1727 100%);overflow:hidden}.ticks{position:absolute;inset:0;display:flex;justify-content:space-between;padding:6px 9px;color:#71849f;font-size:10px;pointer-events:none}.ticks:before{content:"";position:absolute;top:37px;left:0;right:0;border-top:1px solid #2b405e}.cut-marker{position:absolute;top:4px;left:calc(var(--cut)*1%);z-index:2;max-width:155px;padding-left:7px;color:#c6d4ef;font-size:10px;font-weight:750;line-height:1.2;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;transform:translateX(-4px);pointer-events:none}.cut-marker:before{content:"";position:absolute;left:0;top:18px;height:16px;border-left:1px dashed #8399c0}.shot{position:absolute;top:47px;left:calc(var(--left)*1%);width:max(2%,calc(var(--width)*1%));min-width:13px;height:31px;border-radius:6px;background:var(--tone);border:1px solid #c9d7ff;box-shadow:0 3px 10px #0004;overflow:hidden;cursor:default}.shot-label{display:block;padding:7px 8px;color:#071321;font-size:11px;font-weight:850;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.shot:nth-of-type(4n+1){--tone:#8eaaff}.shot:nth-of-type(4n+2){--tone:#75d9b6}.shot:nth-of-type(4n+3){--tone:#ffc77f}.shot:nth-of-type(4n){--tone:#d69df3}.timeline-empty{height:94px;display:grid;place-items:center;border:1px dashed #405775;border-radius:10px;color:#a0b2ce;font-size:12px}.empty{padding:58px 20px;border:1px dashed #3b5070;border-radius:16px;color:#a8b9d2;text-align:center}.error{color:#ffb2bf}.loading{color:#a3b4cf;font-size:13px;padding:24px}@media(max-width:980px){header{gap:12px}.search{width:260px}.asset-row{grid-template-columns:170px minmax(190px,.8fr) minmax(280px,1.4fr)}}@media(max-width:720px){header{flex-wrap:wrap;padding:14px 20px}.brand{margin-right:0}.search{order:3;width:100%}.wrap{padding:28px 20px 44px}.top{display:block}.legend{margin-top:16px}.asset-row{grid-template-columns:1fr;gap:13px}.thumb,.thumb-empty{min-height:auto;height:auto}.timeline-card{padding:0}.asset-info{padding:0}.semantic-timeline,.timeline-empty{height:98px}.shot{top:51px}.ticks:before{top:41px}.cut-marker:before{height:20px}}</style></head><body><header><span class="brand">Timingdex</span><a class="nav-active" href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a><a href="/library-roots">添加素材目录</a><a href="/settings">设置</a><a href="/repurpose">翻新方案</a><a href="/tags">Tag Curator</a><div class="search"><input id="q" placeholder="搜索内容、口述或标签"><button class="primary" onclick="search()">搜索</button><button onclick="load()">全部</button></div></header><main class="wrap" data-library-browser><section class="top"><div><div class="eyebrow">FOOTAGE LIBRARY · SHOT LEVEL</div><h1>素材库 · 镜头浏览</h1><p class="muted">从缩略图、素材语义到每个时间段的镜头内容，一眼看清你的素材里有什么可以用。</p></div><div class="legend"><span><i></i> 不同镜头</span><span><i></i> 时间范围</span><span><i></i> 语义描述</span></div></section><section id="library" class="library" aria-live="polite"><div class="loading">正在读取素材库…</div></section></main><script>
const library=document.getElementById('library');const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const fmt=ms=>{ms=Math.max(0,Math.floor((ms||0)/1000));return String(Math.floor(ms/60)).padStart(2,'0')+':'+String(ms%60).padStart(2,'0')};
function loadShots(id){return fetch('/api/v1/assets/'+encodeURIComponent(id)+'/shots').then(r=>r.ok?r.json():[]).then(x=>Array.isArray(x)?x:[]).catch(()=>[])}
function chips(x){const values=[x.asset_type,x.camera_motion,x.lighting,...(x.mood_tags||[])].filter(Boolean).slice(0,5);return values.map(v=>'<span class="chip">'+esc(v)+'</span>').join('')+(x.has_speech?'<span class="chip voice">有口述</span>':'')}
function timeline(x,shots){const duration=Math.max(Number(x.duration_ms)||0,1);if(!shots.length)return '<div class="timeline-empty">尚未生成镜头理解；完成分析后会显示可用时间段。</div>';const labels=['00:00',fmt(duration/2),fmt(duration)];const blocks=shots.map(s=>{const start=Math.max(0,Number(s.start_ms)||0),end=Math.max(start,Number(s.end_ms)||start),left=Math.min(100,start/duration*100),width=Math.max(1,(end-start)/duration*100),description=s.description||((s.tags||[]).join(' · '))||'未命名镜头',title=fmt(start)+' — '+fmt(end)+' · '+description;return '<div class="shot" style="--left:'+left.toFixed(3)+';--width:'+width.toFixed(3)+'" title="'+esc(title)+'"><span class="shot-label">'+esc(description)+'</span></div>'}).join('');const cuts=shots.slice(1).map(s=>{const start=Math.max(0,Number(s.start_ms)||0),left=Math.min(100,start/duration*100),description=s.description||((s.tags||[]).join(' · '))||'下一个镜头',time=fmt(start);return '<div class="cut-marker" data-cut-time="'+esc(time)+'" style="--cut:'+left.toFixed(3)+'" title="'+esc(time+' 段落边界 · '+description)+'">'+esc(time)+'</div>'}).join('');return '<div class="semantic-timeline" aria-label="镜头语义时间轴"><div class="ticks"><span>'+labels[0]+'</span><span>'+labels[1]+'</span><span>'+labels[2]+'</span></div>'+cuts+blocks+'</div>'}
function thumbnail(x){return x.thumbnail_url?'<img class="thumb" loading="lazy" src="'+esc(x.thumbnail_url)+'" alt="'+esc(x.filename)+' 的缩略图" onerror="this.outerHTML=\'<div class=&quot;thumb-empty&quot;>缩略图不可用</div>\'">':'<div class="thumb-empty">暂无缩略图</div>'}
function optionalDetails(x){const fields=[['相机',x.camera_model],['区域',x.region_label],['场次',x.session_id],['色彩',x.source_color],['配置',x.color_profile],['原始格式',x.raw_format],['预览',x.preview_status]].filter(([,value])=>value);return fields.length?'<div class="asset-details" aria-label="拍摄与预览信息">'+fields.map(([label,value])=>'<span class="detail"><b>'+esc(label)+'</b>'+esc(value)+'</span>').join('')+'</div>':''}
function row(x,shots){return '<article class="asset-row"><div>'+thumbnail(x)+'</div><div class="asset-info"><div class="filename" title="'+esc(x.filename)+'">'+esc(x.filename||'未命名素材')+'</div><div class="asset-meta">'+fmt(x.duration_ms)+' · '+esc(x.orientation||'方向未知')+' · '+esc(x.state||'未知状态')+'</div><p class="summary">'+esc(x.summary||'正在等待视频理解结果。')+'</p><div class="chips">'+chips(x)+'</div>'+optionalDetails(x)+'</div><div class="timeline-card"><div class="timeline-head"><span class="timeline-title">镜头语义时间轴</span><span class="duration">'+fmt(x.duration_ms)+'</span></div>'+timeline(x,shots)+'</div></article>'}
async function load(ids){library.innerHTML='<div class="loading">正在整理镜头时间轴…</div>';try{let data=await fetch('/api/v1/assets?limit=300').then(r=>r.ok?r.json():[]);data=Array.isArray(data)?data:[];if(ids)data=data.filter(x=>ids.includes(x.id));if(!data.length){library.innerHTML='<div class="empty">暂无素材。先在「启动配置」添加素材目录并运行处理任务，镜头、缩略图和时间轴会出现在这里。</div>';return}const rows=await Promise.all(data.map(async x=>row(x,await loadShots(x.id))));library.innerHTML=rows.join('')}catch(e){library.innerHTML='<div class="empty error">无法读取素材库：'+esc(e.message)+'</div>'}}
async function search(){const q=document.getElementById('q').value.trim();if(!q)return load();try{const ids=await fetch('/api/v1/search?q='+encodeURIComponent(q)).then(r=>r.ok?r.json():[]);load(Array.isArray(ids)?ids:[])}catch(e){library.innerHTML='<div class="empty error">搜索失败：'+esc(e.message)+'</div>'}}document.getElementById('q').addEventListener('keydown',e=>{if(e.key==='Enter')search()});load();</script></body></html>`

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(providersHTML)))
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(setupHTML)))
}

func (s *Server) progressPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(progressHTML)))
}

const progressHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 处理进度</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:14px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:1;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:1180px;margin:auto;padding:32px 24px}.top{display:flex;justify-content:space-between;gap:18px;align-items:flex-end}.top h1{font-size:32px;margin:0;letter-spacing:-.04em}.muted{color:#aab8d0;line-height:1.5}button{background:#86a3ff;color:#091227;border:0;border-radius:9px;padding:10px 14px;font-weight:800;cursor:pointer}.metrics{display:grid;grid-template-columns:repeat(6,1fr);gap:12px;margin:24px 0}.metric,.panel{background:#172238;border:1px solid #2c3d5b;border-radius:14px}.metric{padding:16px}.number{font-size:30px;font-weight:800;letter-spacing:-.04em;margin-top:5px}.panels{display:grid;grid-template-columns:1.4fr .8fr;gap:16px}.panel{padding:18px}.panel h2{margin:0 0 14px;font-size:17px}table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:10px 7px;border-bottom:1px solid #2b3b55;font-size:13px;vertical-align:top}.state{border-radius:99px;padding:3px 8px;font-size:12px;font-weight:700;background:#34445e}.state.succeeded{background:#164b39;color:#9cf0c2}.state.failed{background:#612c3a;color:#ffc0c8}.state.running{background:#3a376b;color:#d8d4ff}.state.deferred{background:#5a4a1f;color:#ffdfa6}.log{font:12px ui-monospace,SFMono-Regular,Menlo,monospace;line-height:1.55;color:#b9c7e3;min-height:280px;max-height:480px;overflow:auto;white-space:pre-wrap}.log div{padding:6px 0;border-bottom:1px solid #263650}@media(max-width:760px){.metrics{grid-template-columns:repeat(2,1fr)}.panels{grid-template-columns:1fr}.top{align-items:flex-start;flex-direction:column}}header input{width:auto;max-width:260px;padding:9px 10px;border-radius:8px;border:1px solid #354764;background:#0e1728;color:#fff;font:inherit}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/library-roots">添加素材目录</a><a href="/repurpose">翻新方案</a><a href="/tags">Tag Curator</a><input id="admin-token" type="password" autocomplete="off" placeholder="Hub 管理 Token（仅存于本页内存）"></header><main class="wrap"><div class="top"><div><h1>处理进度</h1><p class="muted">状态每 2.5 秒更新。右侧是本次浏览器会话的操作记录；下方作业错误来自本地任务队列。若通道内所有 API Key 都失败（多为额度用尽），作业会转入「等待额度」并在若干小时后自动重试，不消耗尝试次数。</p></div><div style="display:flex;gap:10px"><button id="resume" onclick="resumeDeferred()" style="background:#31446a;color:#dbe6ff">立即重试等待额度的作业</button><button id="retry" onclick="retryFailed()" style="background:#31446a;color:#dbe6ff">重试失败作业</button><button id="run" onclick="runPipeline()">运行待处理任务</button></div></div><section id="supervisor" class="muted" style="margin-top:18px">无人值守巡检：正在读取…</section><section id="metrics" class="metrics"></section><section class="panels"><div class="panel"><h2>最近作业</h2><div id="jobs" class="muted">正在读取…</div></div><div class="panel"><h2>本次操作</h2><div id="log" class="log"></div></div></section></main><script>
function adminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}
function authHeaders(base){const headers=new Headers(base||{});const token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
const logEl=document.getElementById('log');let logs=[];function log(m){logs.unshift(new Date().toLocaleTimeString()+'  '+esc(m));logs=logs.slice(0,30);logEl.innerHTML=logs.map(x=>'<div>'+x+'</div>').join('')}function esc(s){return String(s||'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}function when(v){const t=new Date(v);return isNaN(t.getTime())?String(v||''):t.toLocaleString()}
function stateCell(j){if(j.deferred_reason)return '<span class="state deferred">等待服务商额度</span>';return '<span class="state '+esc(j.state)+'">'+esc(j.terminal?j.state+'（永久，不再重试）':j.state)+'</span>'}
function detailCell(j){if(j.deferred_reason)return '通道内所有 API Key 都失败（多为套餐额度用尽）。已暂停，'+esc(when(j.run_after))+' 自动重试，本次不计入尝试次数。';return esc(j.last_error||(j.has_error?'有错误（填入管理 Token 查看详情）':'')||j.run_after||'—')}
// The metric row is counted by the database, not tallied from the rows below
// it. Those rows are the newest hundred jobs, which during a scan are all
// freshly enqueued work -- tallying them reported a library with thousands of
// finished jobs as "0 done" and left it there.
function renderMetrics(s){document.getElementById('metrics').innerHTML=[['待处理',s.pending],['处理中',s.running],['已完成',s.succeeded],['需处理',s.failed],['永久失败',s.terminal],['等待额度',s.deferred]].map(m=>'<div class="metric"><div class="muted">'+m[0]+'</div><div class="number">'+(m[1]||0)+'</div></div>').join('')}
function render(jobs){document.getElementById('jobs').innerHTML=jobs.length?'<table><tr><th>类型</th><th>状态</th><th>尝试</th><th>错误 / 下次运行</th></tr>'+jobs.map(j=>'<tr><td>'+esc(j.job_type)+'</td><td>'+stateCell(j)+'</td><td>'+j.attempt_count+'/'+j.max_attempts+'</td><td>'+detailCell(j)+'</td></tr>').join('')+'</table>':'暂无作业。先在素材根目录扫描视频。'}async function refresh(){try{const jobs=await fetch('/api/v1/jobs?limit=100',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});render(jobs)}catch(e){document.getElementById('jobs').textContent='无法读取作业：'+e.message}try{const s=await fetch('/api/v1/jobs/summary',{headers:authHeaders()}).then(async r=>{if(!r.ok)throw Error(await r.text());return r.json()});renderMetrics(s)}catch(e){log('无法读取队列统计：'+e.message)}refreshSupervisor()}
function supervisorText(d){if(!d.enabled)return '无人值守巡检：<b>未开启</b>。在 Hub 的 config.json 设置 library_supervisor.enabled=true 并重启 Hub 后，Hub 会自动定时扫描素材目录并处理队列（会消耗服务商额度）。';
if(!d.running)return '无人值守巡检：<b>已配置但未在运行</b>。当前进程可能不是 timingdex serve。';
const parts=['无人值守巡检：<b>运行中</b>','每 '+Math.round(d.interval_seconds/60)+' 分钟扫描一次'];
parts.push(d.last_pass_at?'上次 '+esc(when(d.last_pass_at)):'尚未扫描');
parts.push(d.scanning?'正在扫描…':(d.next_pass_at?'下次 '+esc(when(d.next_pass_at)):'—'));
if(d.last_outcome==='held_off_peak')parts.push('当前在凌晨时段之外，'+esc(d.held_until?when(d.held_until):'时段开始时')+'才会扫描');
else if(d.last_outcome==='pipeline_busy')parts.push('已有处理任务在运行，本次只扫描');
else if(d.last_outcome==='error')parts.push('上次巡检有错误：'+esc(d.last_error||'填入管理 Token 查看详情'));
else if(d.last_outcome==='scanned')parts.push('上次扫描 '+d.roots_scanned+' 个目录，新增 '+d.discovered+' 个素材');
return parts.join(' · ')}
async function refreshSupervisor(){const el=document.getElementById('supervisor');try{const r=await fetch('/api/v1/pipeline/supervisor',{headers:authHeaders()});if(!r.ok)throw Error(await r.text());el.innerHTML=supervisorText(await r.json())}catch(e){el.textContent='无法读取无人值守巡检状态：'+e.message}}
async function retryFailed(){const b=document.getElementById('retry');b.disabled=true;log('已请求重试失败作业');try{const r=await fetch('/api/v1/pipeline/retry-failed',{method:'POST',headers:authHeaders()});if(!r.ok)throw Error(await r.text());const d=await r.json();log('已重新排队 '+d.requeued+' 个失败作业；点击「运行待处理任务」开始处理')}catch(e){log('重试失败：'+e.message)}finally{b.disabled=false;refresh()}}async function resumeDeferred(){const b=document.getElementById('resume');b.disabled=true;log('已请求提前释放等待额度的作业');try{const r=await fetch('/api/v1/pipeline/resume-deferred',{method:'POST',headers:authHeaders()});if(!r.ok)throw Error(await r.text());const d=await r.json();log(d.resumed?'已释放 '+d.resumed+' 个等待额度的作业；点击「运行待处理任务」开始处理':'当前没有等待额度的作业')}catch(e){log('释放失败：'+e.message)}finally{b.disabled=false;refresh()}}
async function runPipeline(){const b=document.getElementById('run');b.disabled=true;b.textContent='正在运行…';log('已请求执行待处理任务');try{const r=await fetch('/api/v1/pipeline/run',{method:'POST',headers:authHeaders()});if(!r.ok)throw Error(await r.text());const d=await r.json().catch(()=>({}));log(d.status==='already_running'?'已有处理任务在后台运行':'处理任务已在后台启动，可关闭本页')}catch(e){log('执行失败：'+e.message)}finally{b.disabled=false;b.textContent='运行待处理任务';refresh()}}refresh();setInterval(refresh,2500);log('进度面板已打开');</script></body></html>`

func (s *Server) repurposePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(repurposeWorkspaceHTML)))
}

const repurposeWorkspaceHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · 翻新工作台</title><style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#101827;color:#edf3ff;font:15px ui-sans-serif,system-ui,-apple-system,sans-serif}header{padding:15px 5vw;border-bottom:1px solid #293953;display:flex;gap:18px;align-items:center;background:#101827ee;position:sticky;top:0;z-index:2;backdrop-filter:blur(12px)}a{color:#b8c8ff;text-decoration:none}.brand{color:#fff;font-weight:800;margin-right:auto}.wrap{max-width:1200px;margin:auto;padding:38px 24px}.intro{display:grid;grid-template-columns:1.1fr .9fr;gap:22px;align-items:end}.eyebrow{color:#8da7ff;font-weight:800;font-size:12px;letter-spacing:.1em}.intro h1{font-size:clamp(32px,5vw,54px);line-height:1.02;letter-spacing:-.05em;margin:10px 0}.muted{color:#aab8d0;line-height:1.6}.form,.plan,.section{background:#172238;border:1px solid #2c3d5b;border-radius:16px}.form{padding:20px}.field{margin:11px 0}.field label{font-size:12px;color:#bfcae0;font-weight:800;display:block;margin:0 0 5px}textarea,input{width:100%;font:inherit;padding:10px;border-radius:8px;background:#0e1728;color:#fff;border:1px solid #374965}textarea{min-height:92px;resize:vertical}.row{display:grid;grid-template-columns:repeat(3,1fr);gap:9px}button{border:0;border-radius:8px;padding:8px 11px;font:inherit;font-weight:800;cursor:pointer}.primary{margin-top:8px;background:#8ca7ff;color:#0d1830}.plan{margin-top:28px;padding:24px}.planhead{display:flex;justify-content:space-between;align-items:flex-start;gap:16px}.planhead h2{font-size:27px;margin:0;letter-spacing:-.035em}.pill{border:1px solid #465b82;color:#b8c9ff;border-radius:99px;padding:4px 9px;font-size:12px;white-space:nowrap}.section{margin-top:13px;padding:16px}.sectionhead{display:flex;justify-content:space-between;gap:12px;align-items:start}.sectionactions{display:flex;gap:7px;align-items:center}.role{font-weight:900;text-transform:capitalize}.rationale{color:#b7c5de;font-size:13px;margin:7px 0 13px}.candidate{display:grid;grid-template-columns:150px 1fr;gap:13px;background:#111b2d;border:1px solid #2a3a56;border-radius:11px;padding:10px;margin:8px 0}.candidate.selected{border-color:#91acff;background:#172747}.candidate.excluded{opacity:.55;border-style:dashed}.thumb{width:150px;aspect-ratio:16/9;object-fit:cover;background:#090f1b;border-radius:7px}.candidate b{font-size:14px}.small{font-size:12px;color:#aab8d0;margin-top:5px;line-height:1.45}.candidate-actions{display:flex;gap:7px;flex-wrap:wrap;margin-top:10px}.select{background:#a7baff;color:#101a30}.select.on{background:#73dcad;color:#082419}.secondary{background:#2b3d5b;color:#cbd7f4}.exclude{background:#4a2f3c;color:#ffc2cb}.empty{border:1px dashed #4d607f;border-radius:10px;padding:14px;color:#b5c4df;font-size:13px}.warn{color:#ffc98d}.approve{background:#74ddb2;color:#08241b}.save{background:#8ca7ff;color:#0d1830}.error{color:#ffb5c0;margin-top:12px}.editor{margin-top:20px;border-top:1px solid #31425f;padding-top:16px}.editorbar{display:flex;justify-content:space-between;gap:12px;align-items:end}.editorbar textarea{min-height:56px}.statusline{font-size:12px;color:#9db1d2;margin-top:7px}@media(max-width:760px){.intro{grid-template-columns:1fr}.row{grid-template-columns:1fr}.candidate{grid-template-columns:1fr}.thumb{width:100%}.wrap{padding-top:28px}.editorbar{align-items:stretch;flex-direction:column}}header input{width:auto;max-width:260px;padding:9px 10px}</style></head><body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a><a href="/settings">设置</a><a href="/tags">Tag Curator</a><input id="admin-token" type="password" autocomplete="off" placeholder="Hub 管理 Token（仅存于本页内存）"></header><main class="wrap"><div class="intro"><div><div class="eyebrow">REPURPOSE WORKSPACE · V0.11</div><h1>把推荐，变成你的剪辑选择。</h1><p class="muted">选择一个候选镜头、锁住关键决定、排除不适合的备选，再保存为独立 revision。原视频不会被修改。</p></div><form class="form" onsubmit="createPlan(event)"><div class="field"><label>这次要做什么？</label><textarea id="brief" required placeholder="例如：做一个 30 秒深圳城市生活宣传片，要有夜景、通勤和人文气息"></textarea></div><div class="row"><div class="field"><label>总时长（秒）</label><input id="duration" type="number" min="5" value="30"></div><div class="field"><label>风格</label><input id="style" placeholder="城市生活"></div><div class="field"><label>受众</label><input id="audience" placeholder="品牌客户"></div></div><button class="primary" id="create">生成可审核方案</button><div id="error" class="error"></div></form></div><section id="result" class="plan" aria-live="polite"><div class="muted">先写下创作需求。生成后可为每个段落选择、锁定或排除候选镜头。</div></section></main><script>
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const fmt=ms=>{ms=Math.max(0,Math.floor((ms||0)/1000));return String(Math.floor(ms/60)).padStart(2,'0')+':'+String(ms%60).padStart(2,'0')};let activePlan=null,dirty=false;
function adminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}
function authHeaders(base){const headers=new Headers(base||{});const token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
async function api(url,opt){opt=opt||{};const r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok){if(r.status===401)throw Error('需要 Hub 管理 Token：请先在顶部填入');throw Error(await r.text())}return r.json()}
// Exports are fetched rather than linked because the route needs the admin
// token in a header, which an <a href> cannot carry. The response is handed to
// the browser as a Blob so the file never round-trips through a URL that would
// put the token in history.
async function downloadExport(kind){if(!activePlan)return;try{const r=await fetch('/api/v1/repurpose/plans/'+encodeURIComponent(activePlan.id)+'/export.'+kind,{headers:authHeaders()});if(!r.ok){if(r.status===401)throw Error('需要 Hub 管理 Token：请先在顶部填入');throw Error(await r.text())}const url=URL.createObjectURL(await r.blob());const a=document.createElement('a');a.href=url;a.download=activePlan.id+'.'+kind;document.body.appendChild(a);a.click();a.remove();URL.revokeObjectURL(url)}catch(e){alert('导出失败：'+e.message)}}
function section(role){return activePlan.sections.find(s=>s.role===role)}
function rerender(){render(activePlan);dirty=true;document.getElementById('statusline').textContent='有未保存的编辑。保存后会创建新的 revision。'}
function candidate(s,c){const selected=s.selected_shot_id===c.shot_id,excluded=(s.excluded_shot_ids||[]).includes(c.shot_id),image='/api/v1/assets/'+encodeURIComponent(c.asset_id)+'/thumbnail',why=(c.reasons||[]).map(esc).join(' · ')||'由检索得分匹配';return '<article class="candidate '+(selected?'selected ':'')+(excluded?'excluded':'')+'"><img class="thumb" loading="lazy" src="'+image+'" onerror="this.style.visibility=\'hidden\'" alt="候选镜头缩略图"><div><b>'+fmt(c.start_ms)+' — '+fmt(c.end_ms)+'</b> <span class="pill">匹配 '+Math.round((c.score||0)*100)+'%</span>'+(c.reused?' <span class="pill warn">复用镜头</span>':'')+(selected?' <span class="pill">已选</span>':'')+'<div class="small">素材 '+esc(c.asset_id)+' · 镜头 '+esc(c.shot_id)+'</div><div class="small">'+why+'</div><div class="candidate-actions"><button class="select '+(selected?'on':'')+'" data-role="'+esc(s.role)+'" data-shot="'+esc(c.shot_id)+'" onclick="choose(this.dataset.role,this.dataset.shot)">'+(selected?'已选择':'选择此镜头')+'</button><button class="exclude" data-role="'+esc(s.role)+'" data-shot="'+esc(c.shot_id)+'" onclick="toggleExclude(this.dataset.role,this.dataset.shot)" '+(selected?'disabled':'')+'>'+ (excluded?'恢复候选':'排除')+'</button></div></div></article>'}
function render(p){activePlan=p;const sections=(p.sections||[]).map(s=>'<section class="section"><div class="sectionhead"><div><div class="role">'+esc(s.role)+'</div><div class="small">目标 '+fmt(s.duration_ms)+' · 查询：'+esc(s.query)+'</div></div><div class="sectionactions"><span class="pill">'+(s.required?'必需':'可选')+'</span>'+(s.locked?'<button class="secondary" data-role="'+esc(s.role)+'" onclick="unlock(this.dataset.role)">解除锁定</button>':'<button class="secondary" data-role="'+esc(s.role)+'" onclick="lock(this.dataset.role)">锁定选择</button>')+'<button class="secondary" data-role="'+esc(s.role)+'" onclick="findAlternatives(this.dataset.role)">找替代镜头</button></div></div><div class="rationale">'+esc(s.rationale||'')+(s.locked?' · 此段已锁定':'')+'</div>'+(s.candidates&&s.candidates.length?s.candidates.map(c=>candidate(s,c)).join(''):'<div class="empty">此段没有足够的匹配镜头。可以尝试找替代镜头，或保留缺口以便补拍。</div>')+'</section>').join('');document.getElementById('result').innerHTML='<div class="planhead"><div><div class="eyebrow">'+esc(p.provider||'deterministic')+' · '+esc(p.model||'')+'</div><h2>'+esc(p.title||p.brief)+'</h2><p class="muted">'+fmt(p.duration_ms)+' · '+esc(p.style||'未设定风格')+' · '+esc(p.audience||'未设定受众')+'</p></div><div><span class="pill">'+esc(p.status)+'</span> '+(p.status==='draft'?'<button class="approve" onclick="approve()">批准这一版</button>':'')+(p.status==='approved'?'<button class="secondary" onclick="downloadExport(\'edl\')">导出 EDL</button> <button class="secondary" onclick="downloadExport(\'fcpxml\')">导出 FCPXML</button>':'')+'</div></div>'+(p.missing_needs&&p.missing_needs.length?'<p class="warn">还缺：'+p.missing_needs.map(esc).join('、')+'</p>':'')+sections+(p.status==='draft'?'<div class="editor"><div class="editorbar"><div style="flex:1"><label class="small" for="editorNote">这次编辑的说明</label><textarea id="editorNote" placeholder="例如：将雨夜航拍锁为开场，排除手持街拍"></textarea><div id="statusline" class="statusline">选择镜头后，保存为新的编辑版。</div></div><button class="save" onclick="saveRevision()">保存编辑版</button></div></div>':'')}
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&brief); err != nil || strings.TrimSpace(brief.Brief) == "" {
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	revision, err := s.service.ReviseRepurposePlan(r.Context(), r.PathValue("id"), request.Sections, request.EditorNote)
	if err != nil {
		// Classified structurally, with errors.Is against sentinels the app
		// layer returns, rather than by message text -- see writeExportError
		// (export.go) for the export boundary this mirrors and why a
		// substring match on "immutable"/"not found" was the wrong tool: a
		// reworded message, or an unrelated lower-layer error that happened
		// to contain the same phrase, silently reclassified the response.
		if errors.Is(err, app.ErrInvalidRepurposeRevision) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if errors.Is(err, app.ErrPlanImmutable) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, app.ErrPlanNotFound) {
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
		if errors.Is(err, app.ErrPlanRevisionNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, app.ErrPlanRevisionNotDraft) || errors.Is(err, app.ErrPlanRevisionNotLatest) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approved)
}

func (s *Server) exportRepurposePlanEDL(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("id")
	document, err := s.service.ExportPlanEDL(r.Context(), planID)
	if err != nil {
		writeExportError(w, r, err)
		return
	}
	writeExport(w, "text/plain; charset=utf-8", planID+".edl", document)
}

func (s *Server) exportRepurposePlanFCPXML(w http.ResponseWriter, r *http.Request) {
	planID := r.PathValue("id")
	document, err := s.service.ExportPlanFCPXML(r.Context(), planID)
	if err != nil {
		writeExportError(w, r, err)
		return
	}
	writeExport(w, "application/xml; charset=utf-8", planID+".fcpxml", document)
}

// writeExport sends a finished export as a download. The filename is built from
// the plan id rather than its title: titles are model-authored free text, and a
// Content-Disposition header is one of the few places where unescaped text
// crosses back out of the JSON layer. Plan ids are Hub-generated and safe.
func writeExport(w http.ResponseWriter, contentType, filename, document string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, document)
}

// writeExportError separates "this plan is not ready to be cut" from "the Hub
// failed". Everything nleexport rejects — a mixed frame rate, a shot past the
// end of its file, a section whose selection no longer resolves — is a fact
// about the library that the operator has to act on, so it must not arrive as a
// 500 that reads like a Hub bug and gets retried. A plan id that names nothing
// is a 404, and because GetRepurposePlan returns (nil, nil) rather than
// sql.ErrNoRows that refusal has no structural marker of its own — which is
// exactly why it gets one. The boundaries are matched structurally, with
// errors.Is against sentinels the app and nleexport return, rather than by
// message text: a reworded error is ordinary maintenance, and classification
// must survive it.
func writeExportError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrPlanNotFound):
		http.NotFound(w, r)
	case errors.Is(err, app.ErrPlanNotApproved):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, app.ErrPlanNotExportable), errors.Is(err, nleexport.ErrInvalidTimeline):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		writeError(w, err)
	}
}

func (s *Server) tagsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(brandedPage(tagsHTML)))
}

const tagsHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex Tag Curator</title><style>
body{font-family:system-ui,-apple-system,sans-serif;margin:0;background:#111;color:#eee}header{position:sticky;top:0;padding:16px;background:#181818;display:flex;gap:12px;align-items:center}a{color:#8bc5ff}button{padding:8px 12px;border:0;border-radius:8px;cursor:pointer}.primary{background:#e8e8e8}.approve{background:#b8efc0}.reject{background:#efb8b8}.wrap{padding:16px;display:grid;gap:20px}.panel{background:#1b1b1b;border:1px solid #333;border-radius:12px;padding:14px}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:9px;border-bottom:1px solid #333;vertical-align:top}.muted{color:#aaa;font-size:12px}.pill{display:inline-block;background:#333;padding:3px 7px;border-radius:999px;margin:2px;font-size:12px}input{flex:1;max-width:280px;padding:8px 10px;border-radius:8px;border:1px solid #444;background:#222;color:#eee;font:inherit}</style></head><body><header><span class="brand">Timingdex</span><strong>Tag Curator</strong><a href="/">素材库</a><input id="admin-token" type="password" autocomplete="off" placeholder="Hub 管理 Token（仅存于本页内存）"><button class="primary" onclick="curate()">生成治理提案</button></header><div class="wrap"><section class="panel"><h2>未解析标签</h2><div id="unresolved"></div></section><section class="panel"><h2>待审核提案</h2><div id="proposals"></div></section><section class="panel"><h2>Canonical Tags</h2><div id="tags"></div></section></div><script>
const esc=s=>String(s??'').replace(/[&<>\"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;',"'":'&#39;'}[c]));
function adminToken(){const el=document.getElementById('admin-token');return el?el.value.trim():''}
function authHeaders(base){const headers=new Headers(base||{});const token=adminToken();if(token)headers.set('Authorization','Bearer '+token);return headers}
async function j(url,opt){opt=opt||{};const r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok){if(r.status===401)throw new Error('需要 Hub 管理 Token：请先在顶部填入');throw new Error(await r.text())}return r.json()}
async function curate(){await j('/api/v1/tags/curate',{method:'POST'});await load()}
async function review(id,action){await j('/api/v1/tags/proposals/'+id+'/review',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({action})});await load()}
function unresolvedTable(xs){return '<table><tr><th>标准化值</th><th>原始形式</th><th>素材数</th></tr>'+xs.map(x=>'<tr><td>'+esc(x.normalized_tag)+'</td><td>'+x.display_forms.map(v=>'<span class="pill">'+esc(v)+'</span>').join('')+'</td><td>'+x.asset_count+'</td></tr>').join('')+'</table>'}
function proposalTable(xs){return '<table><tr><th>动作</th><th>Canonical</th><th>原因</th><th>影响</th><th></th></tr>'+xs.map(x=>'<tr><td>'+esc(x.proposal_type)+'</td><td>'+esc(x.canonical_name)+'</td><td>'+esc(x.reason)+'<div class="muted">置信度 '+Math.round(x.confidence*100)+'%</div></td><td>'+x.affected_assets+'</td><td><button class="approve" onclick="review(\''+x.id+'\',\'approve\')">批准</button> <button class="reject" onclick="review(\''+x.id+'\',\'reject\')">拒绝</button></td></tr>').join('')+'</table>'}
function tagTable(xs){return '<table><tr><th>Canonical</th><th>分类</th><th>素材</th><th>别名</th></tr>'+xs.map(x=>'<tr><td>'+esc(x.canonical_name)+'</td><td>'+esc(x.category)+'</td><td>'+x.usage_count+'</td><td>'+x.alias_count+'</td></tr>').join('')+'</table>'}
async function load(){const [u,p,t]=await Promise.all([j('/api/v1/tags/unresolved'),j('/api/v1/tags/proposals?state=pending'),j('/api/v1/tags')]);document.getElementById('unresolved').innerHTML=u.length?unresolvedTable(u):'<p class="muted">暂无未解析标签</p>';document.getElementById('proposals').innerHTML=p.length?proposalTable(p):'<p class="muted">暂无待审核提案</p>';document.getElementById('tags').innerHTML=t.length?tagTable(t):'<p class="muted">尚未建立 Canonical Tag</p>'}load();</script></body></html>`
