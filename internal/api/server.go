package api

import (
	"context"
	"crypto/sha256"
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
	"sync"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/credentials"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/nleexport"
	"github.com/evjohn-icu/nexusgate/internal/normalize"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/search"
	"github.com/evjohn-icu/nexusgate/internal/smbdiscover"
	"github.com/evjohn-icu/nexusgate/internal/webdavspace"
)

type Server struct {
	address              string
	service              *app.Service
	tlsCert              string
	tlsKey               string
	trustedReadNetworks  []netip.Prefix
	adminAuth            string
	adminAuthNetworks    []netip.Prefix
	webdav               *webdavspace.Manager
	adminSessionsMu      sync.Mutex
	adminSessions        map[[32]byte]adminSession
	adminLoginMu         sync.Mutex
	adminLoginAttempts   map[string]adminLoginAttempt
	adminLoginGlobal     adminLoginAttempt
	trustedProxyNetworks []netip.Prefix
}

// SetTrustedProxyNetworks configures the set of reverse-proxy addresses the
// Hub sits behind. When set, the immediate TCP peer must fall inside one of
// these prefixes before adminLoginKey will key on the client address carried
// in X-Forwarded-For / X-Real-IP instead of the proxy's RemoteAddr — so one
// admin failing to log in cannot lock out every admin behind the same proxy.
// The forwarded headers are only ever honoured for peers in this list: on a
// directly exposed listener they are attacker-controlled and ignored,
// matching network_guard's stance. Nil (the default) keeps RemoteAddr-only
// keying. Intended to be wired from the deployment's proxy configuration at
// startup, before the server starts serving.
func (s *Server) SetTrustedProxyNetworks(prefixes []netip.Prefix) {
	s.trustedProxyNetworks = prefixes
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
	s := &Server{address: address, service: service, tlsCert: certificateFile, tlsKey: keyFile, trustedReadNetworks: defaultTrustedReadNetworks, adminAuth: "trusted_network", adminAuthNetworks: defaultTrustedReadNetworks}
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
	s.adminAuth = service.AdminAuth()
	if prefixes, err := service.AdminAuthNetworks(); err == nil && len(prefixes) > 0 {
		s.adminAuthNetworks = prefixes
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

// Handler builds the complete HTTP surface from the typed route inventory.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return requestLogger(mux)
}

// isHubAdmin reports whether the request carries the Hub admin token. Routes
// that are wholly administrative use requireHubAdmin; routes that are public but
// hold one privileged field call this directly to decide how much to disclose.
func (s *Server) isHubAdmin(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		_, ok := s.adminSessionForRequest(r)
		return ok
	}
	const scheme = "Bearer "
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, scheme) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, scheme))
	expected := s.service.AdminToken()
	// Compare fixed-size SHA-256 digests of both sides so that a length
	// difference does not short-circuit before subtle.ConstantTimeCompare —
	// the raw constant-time comparison itself runs over the full expected
	// token, so an attacker probing remotely cannot distinguish a wrong-length
	// token from a wrong-token of the same length.
	if provided == "" || expected == "" {
		return false
	}
	providedSum := sha256.Sum256([]byte(provided))
	expectedSum := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedSum[:], expectedSum[:]) == 1
}

func (s *Server) requireHubAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.adminAuthWaived(r) && s.sessionCookieValue(r) == "" {
			next(w, r)
			return
		}
		if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
			if _, ok := s.adminSessionForRequest(r); !ok {
				action := "enter_the_admin_token"
				if s.sessionCookieValue(r) != "" {
					action = "login_again"
				}
				writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_authentication_required", Message: "Hub administrator authentication required", Action: action})
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.requireSessionCSRF(w, r) {
				return
			}
			next(w, r)
			return
		}
		if !s.isHubAdmin(r) {
			writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_authentication_required", Message: "Hub administrator authentication required", Action: "enter_the_admin_token"})
			return
		}
		next(w, r)
	}
}

func (s *Server) createAdminSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.secureBrowserRequest(r) {
		writeAPIError(w, http.StatusForbidden, APIError{Code: "secure_session_required", Message: errSessionHTTPS.Error()})
		return
	}
	if !sameRequestOrigin(r, strings.TrimSpace(r.Header.Get("Origin"))) {
		writeAPIError(w, http.StatusForbidden, APIError{Code: "csrf_origin_failed", Message: "request origin is not allowed"})
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	if !decodeStrictJSON(w, r, &request, 4<<10) {
		return
	}
	provided := strings.TrimSpace(request.Token)
	loginKey := s.adminLoginKey(r)
	if retryAfter, limited := s.adminLoginRateLimit(loginKey, time.Now()); limited {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeAPIError(w, http.StatusTooManyRequests, APIError{Code: "admin_login_rate_limited", Message: "too many administrator login attempts", Retryable: true})
		return
	}
	if !s.adminAuthWaived(r) {
		expected := s.service.AdminToken()
		// Same fixed-size digest comparison as isHubAdmin so the login flow does
		// not leak token length either; the empty-token rejection stays.
		providedSum := sha256.Sum256([]byte(provided))
		expectedSum := sha256.Sum256([]byte(expected))
		if provided == "" || expected == "" || subtle.ConstantTimeCompare(providedSum[:], expectedSum[:]) != 1 {
			s.recordAdminLoginFailure(loginKey, time.Now())
			writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_authentication_required", Message: "Hub administrator authentication required", Action: "enter_the_admin_token"})
			return
		}
		s.clearAdminLoginFailures(loginKey)
	}
	sessionValue, csrfValue, err := s.createAdminSessionRecord(time.Now())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, APIError{Code: "session_creation_failed", Message: "could not create administrator session"})
		return
	}
	setAdminSessionCookies(w, sessionValue, csrfValue)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"authenticated": true, "expires_in_seconds": int(adminSessionTTL / time.Second)})
}

func (s *Server) currentAdminSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		if !s.isHubAdmin(r) {
			writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_authentication_required", Message: "Hub administrator authentication required"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "source": "bearer"})
		return
	}
	if _, ok := s.adminSessionForRequest(r); !ok {
		writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_session_expired", Message: "administrator session expired", Action: "login_again"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "source": "session"})
}

func (s *Server) deleteAdminSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.adminSessionForRequest(r); !ok {
		clearAdminSessionCookies(w)
		writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_session_expired", Message: "administrator session expired", Action: "login_again"})
		return
	}
	if !s.requireSessionCSRF(w, r) {
		return
	}
	s.revokeAdminSession(s.sessionCookieValue(r))
	clearAdminSessionCookies(w)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
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
	// Same fixed-size digest comparison as isHubAdmin: never let a length
	// mismatch exit early and reveal how long the agent token is.
	if provided == "" || expected == "" {
		return false
	}
	providedSum := sha256.Sum256([]byte(provided))
	expectedSum := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedSum[:], expectedSum[:]) == 1
}

// requireAgentOrAdmin accepts either the agent token or the admin token. An
// admin must never be blocked from an action an agent may take, so this is
// strictly an OR, never a replacement for requireHubAdmin: only the two
// draft-plan routes documented in skills/nexusgate use it. Approval and
// pipeline runs stay behind requireHubAdmin so the agent token can never
// reach them, keeping CLAUDE.md's "approval is human-only" boundary enforced
// by access control rather than by prompt text alone.
func (s *Server) requireAgentOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.adminAuthWaived(r) && s.sessionCookieValue(r) == "" {
			next(w, r)
			return
		}
		if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
			if _, ok := s.adminSessionForRequest(r); !ok {
				writeAPIError(w, http.StatusUnauthorized, APIError{Code: "agent_or_admin_authentication_required", Message: "Hub agent or administrator authentication required", Action: "login_again"})
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.requireSessionCSRF(w, r) {
				return
			}
			next(w, r)
			return
		}
		if !s.isHubAgent(r) && !s.isHubAdmin(r) {
			writeAPIError(w, http.StatusUnauthorized, APIError{Code: "agent_or_admin_authentication_required", Message: "Hub agent or administrator authentication required", Action: "enter_the_agent_or_admin_token"})
			return
		}
		next(w, r)
	}
}

func (s *Server) createWorkerPairing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	if !decodeStrictJSON(w, r, &req, 1<<20) {
		return
	}
	if err := s.service.CreateWebDAVAccount(r.Context(), req.Username, req.Password); err != nil {
		switch {
		case errors.Is(err, app.ErrWebDAVAccountInvalid):
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "username and password are required"})
		case errors.Is(err, app.ErrWebDAVAccountExists):
			writeAPIError(w, http.StatusConflict, APIError{Code: "conflict", Message: "account already exists", Retryable: true})
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
	if !decodeStrictJSON(w, r, &req, 1<<20) {
		return
	}
	path, err := s.service.LinkWebDAVAsset(r.Context(), r.PathValue("id"), req.AssetID, req.Kind)
	if err != nil {
		switch {
		case errors.Is(err, app.ErrWebDAVSpaceNotFound):
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "unknown space"})
		case errors.Is(err, app.ErrWebDAVLinkKindInvalid):
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "kind must be original or proxy"})
		case strings.TrimSpace(req.AssetID) == "":
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "asset_id is required"})
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
	// Each item carries the Hub's compatibility verdict alongside the stored
	// version so the workers page can render the pill and the refusal note
	// without re-deriving the semver comparison in JavaScript. The verdict is
	// Hub-side truth, computed here (domain.WorkerCompatibility), not echoed
	// from the worker.
	type workerListItem struct {
		remote.Worker
		Compat domain.WorkerCompat `json:"compat"`
	}
	items := make([]workerListItem, 0, len(workers))
	for _, w := range workers {
		items = append(items, workerListItem{Worker: w, Compat: app.WorkerCompatibility(w.Version)})
	}
	writeJSON(w, http.StatusOK, items)
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
		ID                 string  `json:"id"`
		Capability         string  `json:"capability"`
		Label              string  `json:"label"`
		ProviderName       string  `json:"provider_name"`
		Protocol           string  `json:"protocol"`
		Endpoint           string  `json:"endpoint"`
		Model              string  `json:"model"`
		Enabled            bool    `json:"enabled"`
		RouteOrder         int     `json:"route_order"`
		CostPerRequest     float64 `json:"cost_per_request"`
		CostPerVideoMinute float64 `json:"cost_per_video_minute"`
		CostPerAudioMinute float64 `json:"cost_per_audio_minute"`
		Members            []struct {
			ID          string `json:"id"`
			Label       string `json:"label"`
			APIKey      string `json:"api_key"`
			Enabled     bool   `json:"enabled"`
			Weight      int    `json:"weight"`
			MaxInflight int    `json:"max_inflight"`
		} `json:"members"`
	}
	if !decodeStrictJSON(w, r, &input, 1<<20) {
		return
	}
	channel := domain.ProviderChannel{ID: input.ID, Capability: strings.TrimSpace(input.Capability), Label: strings.TrimSpace(input.Label), ProviderName: strings.TrimSpace(input.ProviderName), Protocol: strings.TrimSpace(input.Protocol), Endpoint: strings.TrimSpace(input.Endpoint), Model: strings.TrimSpace(input.Model), Enabled: input.Enabled, RouteOrder: input.RouteOrder, CostPerRequest: input.CostPerRequest, CostPerVideoMinute: input.CostPerVideoMinute, CostPerAudioMinute: input.CostPerAudioMinute}
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
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText("provider channel rejected: "+err.Error(), 300)})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider channel rejected"})
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
	if !decodeStrictJSON(w, r, &patch, 1<<20) {
		return
	}
	updated, err := s.service.UpdateProviderChannel(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		// Same split as the create handler: a validation failure (e.g. two
		// patched members sharing a label) names the label so the operator
		// can fix it; anything else keeps the generic message rather than
		// echoing a downstream layer's error text.
		if errors.Is(err, app.ErrProviderChannelValidation) {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText("provider channel update rejected: "+err.Error(), 300)})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider channel update rejected"})
		}
		return
	}
	s.writeProviderChannel(w, r, http.StatusOK, updated)
}

func (s *Server) enableProviderChannel(w http.ResponseWriter, r *http.Request) {
	updated, err := s.service.SetProviderChannelEnabled(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider channel enable rejected"})
		return
	}
	s.writeProviderChannel(w, r, http.StatusOK, updated)
}

func (s *Server) disableProviderChannel(w http.ResponseWriter, r *http.Request) {
	updated, err := s.service.SetProviderChannelEnabled(r.Context(), r.PathValue("id"), false)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider channel disable rejected"})
		return
	}
	s.writeProviderChannel(w, r, http.StatusOK, updated)
}

func (s *Server) deleteProviderChannel(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteProviderChannel(r.Context(), r.PathValue("id")); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider channel delete rejected"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testProviderChannel(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.TestProviderChannel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider channel test rejected"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// probeProviderModelList is the one-click model-list read for the channel
// dialog. The operator has not saved the channel yet, so the endpoint and key
// come from the still-open form rather than from a persisted channel or the
// secret store. The key is used for one non-billed GET {endpoint}/models and
// is never stored, never logged, and never reflected in the response (the
// service redacts it from every returned model id). It rides in this request
// body only because that is the same trusted direction it already travels on
// channel save; the route stays Hub-admin-gated like every other
// provider-channels route.
func (s *Server) probeProviderModelList(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProviderName string `json:"provider_name"`
		Endpoint     string `json:"endpoint"`
		APIKey       string `json:"api_key"`
	}
	if !decodeStrictJSON(w, r, &input, 8<<10) {
		return
	}
	result, err := s.service.ProbeProviderModelList(r.Context(), input.ProviderName, input.Endpoint, input.APIKey)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider model probe rejected"})
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
	w.Header().Set("Cache-Control", "no-store")
	var request struct {
		PairingToken string `json:"pairing_token"`
		remote.WorkerRegistration
	}
	if !decodeStrictJSON(w, r, &request, 32<<10) {
		return
	}
	if strings.TrimSpace(request.PairingToken) == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid worker enrollment"})
		return
	}
	worker, token, err := s.service.EnrollWorker(r.Context(), request.PairingToken, request.WorkerRegistration)
	if err != nil {
		if errors.Is(err, app.ErrPairingTokenInvalid) {
			writeAPIError(w, http.StatusUnauthorized, APIError{Code: "worker_enrollment_rejected", Message: "worker enrollment rejected"})
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
	var request remote.WorkerHeartbeat
	if !decodeStrictJSON(w, r, &request, 16<<10) {
		return
	}
	if err := s.service.HeartbeatWorker(r.Context(), worker.ID, request.Version, request.Capabilities); err != nil {
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
			writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
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
	if !decodeStrictJSON(w, r, &request, 32<<10) {
		return
	}
	// Validate state before calling the service so an invalid state is a
	// 400, not a 500 from writeError.
	if request.State != domain.JobSucceeded && request.State != domain.JobFailed {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "worker job state must be succeeded or failed"})
		return
	}
	if err := s.service.CompleteWorkerJob(r.Context(), r.PathValue("id"), worker.ID, request.State, request.Message); err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
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
	if !decodeStrictJSON(w, r, &request, 16<<10) {
		return
	}
	if err := s.service.RecordWorkerJobProgress(r.Context(), r.PathValue("id"), worker.ID, strings.TrimSpace(request.Stage), request.Progress, strings.TrimSpace(request.Event), strings.TrimSpace(request.Message)); err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
			return
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "worker progress rejected"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerCredential(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	operation := credentials.Operation(strings.TrimSpace(r.PathValue("operation")))
	lease, err := s.service.IssueWorkerCredential(r.Context(), worker, r.PathValue("id"), operation)
	if err != nil {
		if errors.Is(err, app.ErrWorkerProviderCredentialDeliveryDisabled) {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "forbidden", Message: "worker provider credential delivery is disabled", Action: "enable_worker_provider_credentials"})
			return
		}
		if errors.Is(err, domain.ErrJobLeaseLost) {
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
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
			writeAPIError(w, http.StatusForbidden, APIError{Code: "forbidden", Message: clipText(fmt.Sprintf("worker provider credential rejected: capability %q is configured as a provider channel, which worker direct-credential access does not read; configure providers.* for this capability or use the Hub provider proxy instead", operation), 300), Action: "configure_providers_in_config_or_use_proxy"})
			return
		}
		if errors.Is(err, app.ErrWorkerProviderNotConfigured) {
			// 503, not 400: nothing about the Worker's request is wrong --
			// the Hub simply has no provider for this capability yet, by
			// either configuration method. An operator configuring one later
			// makes the identical request succeed, which is what 503 signals
			// and 400 does not.
			writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: "service_unavailable", Message: clipText(fmt.Sprintf("worker provider credential rejected: no provider is configured for capability %q", operation), 300)})
			return
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "credential request rejected"})
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
	w.Header().Set("Cache-Control", "no-store")
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeAPIError(w, http.StatusUnsupportedMediaType, APIError{Code: "unsupported_media_type", Message: "provider proxy accepts application/json only"})
		return
	}
	if r.ContentLength > app.MaxProviderProxyBodyBytes() {
		writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "provider proxy request is too large"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, app.MaxProviderProxyBodyBytes()+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "provider proxy request is too large"})
		return
	}
	if int64(len(body)) > app.MaxProviderProxyBodyBytes() || !json.Valid(body) {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider proxy accepts valid JSON up to 2 MiB"})
		return
	}
	operation := credentials.Operation(strings.TrimSpace(r.PathValue("operation")))
	result, err := s.service.ProxyWorkerProviderJSON(r.Context(), worker, r.PathValue("id"), operation, body)
	if err != nil {
		if errors.Is(err, app.ErrProviderProxyRequest) {
			writeAPIError(w, http.StatusBadGateway, APIError{Code: "bad_gateway", Message: "provider proxy request failed"})
			return
		}
		if errors.Is(err, domain.ErrJobLeaseLost) {
			writeAPIError(w, http.StatusConflict, APIError{Code: "conflict", Message: "worker does not own active job for provider proxy", Retryable: true})
			return
		}
		// Same split, same reasoning and same status codes as workerCredential's
		// classification above: the operation name is the Worker's own path
		// parameter, so the message is built from it rather than from
		// err.Error(), and 403/503 replace the old flat 400 because neither
		// sentinel means the Worker's request was wrong.
		if errors.Is(err, app.ErrWorkerProviderConfiguredAsChannelOnly) {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "forbidden", Message: clipText(fmt.Sprintf("provider proxy request rejected: capability %q is configured as a provider channel, which the worker provider proxy does not read; configure providers.* for this capability instead", operation), 300), Action: "configure_providers_in_config_or_use_proxy"})
			return
		}
		if errors.Is(err, app.ErrWorkerProviderNotConfigured) {
			writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: "service_unavailable", Message: clipText(fmt.Sprintf("provider proxy request rejected: no provider is configured for capability %q", operation), 300)})
			return
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "provider proxy request rejected"})
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
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid artifact multipart body"})
		return
	}
	artifactType := strings.TrimSpace(r.FormValue("type"))
	profileHash := strings.TrimSpace(r.FormValue("profile_hash"))
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "artifact file is required"})
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
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		case errors.Is(err, app.ErrWorkerArtifactLease):
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
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
	w.Header().Set("Cache-Control", "no-store")
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		writeAPIError(w, http.StatusUnauthorized, APIError{Code: "worker_authentication_required", Message: "worker authentication required"})
		return remote.Worker{}, false
	}
	worker, err := s.service.AuthenticateWorker(r.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, prefix)))
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, APIError{Code: "worker_authentication_failed", Message: "worker authentication failed"})
		return remote.Worker{}, false
	}
	return worker, true
}

func (s *Server) hardwareReport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.service.HardwareReport())
}

func (s *Server) agentCapabilities(w http.ResponseWriter, r *http.Request) {
	// Confidence describes the persisted capture-time observation, not GPS
	// location confidence; location certainty is represented by precision.
	writeJSON(w, http.StatusOK, map[string]any{
		// version is the agent contract version, not the product version. The
		// skills/nexusgate package — its SKILL.md and
		// references/api-contract.md, titled "NexusGate v0.15 Local Agent API
		// Contract" — is written against this exact string, and server_test.go
		// pins it, so it only moves when the contract itself changes: a route,
		// an action, or a field in this document. It must not track Hub
		// releases; the product is at v0.31 while this stays v0.15, and the
		// gap is the contract not having changed, not this endpoint being
		// stale. Bumping it means re-versioning and re-validating the skills
		// package in the same change.
		"version":       "v0.15",
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
			"read_transcript",
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
		// providers renders what this Hub is actually configured to run —
		// names, protocols, models, never credentials — so an operator (or the
		// diagnostics page) can see at a glance whether ASR, vision, repurpose
		// and the rest are wired, without poking the secret store.
		"providers": s.service.ProviderSummary(),
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

// RootHealth is the projection GET /api/v1/roots/health answers with. listRoots
// already serializes domain.LibraryRoot directly (so /api/v1/roots carries the
// same fields under their domain JSON names); this is the compact shape the
// UIs render, with times preformatted the same way encoding/json formats
// time.Time (RFC3339, fractional seconds only when nonzero).
type RootHealth struct {
	RootID         string                  `json:"root_id"`
	Path           string                  `json:"path"`
	State          string                  `json:"state"` // unknown|healthy|unavailable
	LastHealthy    *string                 `json:"last_healthy_at,omitempty"`
	LastScan       *string                 `json:"last_scan_at,omitempty"`
	Warnings       []string                `json:"warnings,omitempty"`
	WarningDetails []app.RootWarningDetail `json:"warning_details,omitempty"`
}

// rootsHealth is the read-only counterpart to listRoots: same data, same
// admin-only posture (a root path is one of the pieces of infrastructure the
// hub administrator owns), shaped for a status table. An unavailable root
// keeps its last_healthy_at — MarkRootUnavailable deliberately never clears
// it, and this endpoint is what lets the UI show "last healthy" during an
// outage instead of a blank. Warnings is the same advice Doctor prints for a
// root (network mount, staging-copy recommendation, writable mount), so the
// health table surfaces what the path alone cannot tell an operator.
func (s *Server) rootsHealth(w http.ResponseWriter, r *http.Request) {
	roots, err := s.service.ListLibraryRoots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	health := make([]RootHealth, 0, len(roots))
	for _, root := range roots {
		item := RootHealth{
			RootID:         root.ID,
			Path:           root.Path,
			State:          string(root.HealthState),
			Warnings:       s.service.RootWarnings(root.Path, true),
			WarningDetails: s.service.RootWarningDetails(root.Path, true),
		}
		if root.LastHealthyAt != nil {
			value := root.LastHealthyAt.Format(time.RFC3339Nano)
			item.LastHealthy = &value
		}
		if root.LastScanAt != nil {
			value := root.LastScanAt.Format(time.RFC3339Nano)
			item.LastScan = &value
		}
		health = append(health, item)
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) createRoot(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if !decodeStrictJSON(w, r, &request, 1<<20) {
		return
	}
	if request.Path == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
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
				Error: APIError{Code: "share_not_mounted", Message: clipText(err.Error(), 300), Action: "mount_the_share"},
				// The same inspection the wizard would have shown before the
				// operator ever tried to add the root: an API caller that never
				// opens /library-roots still gets the mount commands.
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
	Error      APIError           `json:"error"`
	Inspection app.RootInspection `json:"inspection"`
}

func (s *Server) scanRoot(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ScanLibraryRoot(r.Context(), r.PathValue("id"))
	if err != nil {
		// Scan reconciliation can fail after changed assets have already been
		// queued. Do not strand that work merely because the scan response is an
		// error; preserve the error for the caller and still drain the queue.
		s.service.StartPipeline()
		writeError(w, err)
		return
	}
	// Keep scanning synchronous, but hand the queued work to the existing
	// single-run pipeline guard before returning. The supervisor uses
	// TryRunPipeline after scanning all roots; an explicit root scan is the
	// other user-facing entry point and must not leave its queue idle.
	started := s.service.StartPipeline()
	writeJSON(w, http.StatusOK, struct {
		domain.ScanResult
		PipelineStarted bool   `json:"pipeline_started"`
		PipelineBusy    bool   `json:"pipeline_busy"`
		PipelineStatus  string `json:"pipeline_status"`
	}{
		ScanResult:      result,
		PipelineStarted: started,
		PipelineBusy:    !started,
		PipelineStatus:  pipelineTriggerStatus(started),
	})
}

// discoverRoots probes the local network for SMB servers and returns the
// discovered hosts with their guest-accessible shares. It is an active network
// operation, so it is Hub-admin gated like every other root-mutating route;
// an anonymous caller must not be able to trigger a LAN-wide scan.
func (s *Server) discoverRoots(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.service.DiscoverSMBHosts(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if hosts == nil {
		hosts = []smbdiscover.Host{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func pipelineTriggerStatus(started bool) string {
	if started {
		return "started"
	}
	return "already_running"
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
	if !decodeStrictJSON(w, r, &request, 4<<10) {
		return
	}
	if strings.TrimSpace(request.Path) == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
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

// legacyLimitMax caps the limit parameter of the legacy trusted-read GET
// endpoints (listJobs, searchShots, hybridSearchShots, similarShots, rareShots,
// listAssetCards, and the paged list endpoints via parseListPagination). Those
// endpoints pass a caller-supplied limit straight into SQL LIMIT with no upper
// bound, so an unbounded value lets any trusted-network caller force the Hub
// to allocate and serialize an arbitrarily large result set. The v2 POST
// search path already clamps via search.ValidatePagination and the asset-card
// listing caps its own id set, so only these legacy paths need the bound here.
const legacyLimitMax = 500

// parseBoundedInt is parseInt with an upper bound. A value above max clamps to
// max — an explicit limit is a best-effort cap, not a caller-supplied exact
// set, so clamping (like search's maxListedAssetIDs handling) is deliberate
// rather than erroring. Negative or unparsable values keep the fallback,
// matching parseInt. Default values are passed through unchanged.
func parseBoundedInt(value string, fallback, max int) int {
	parsed := parseInt(value, fallback)
	if parsed > max {
		return max
	}
	return parsed
}

// parseListPagination reads the optional limit and offset of the paged legacy
// list endpoints. An absent value keeps the endpoint's existing default; a
// malformed or negative value is an error so a bad parameter is a 400 rather
// than a silent fallback. An explicit limit above max clamps to max — the cap
// must match what the repository layer will actually return, so the reported
// X-NexusGate-Limit header is always the page size a client can page with.
// An explicit 0 is semantically "no limit given": every paged repository
// method treats limit<=0 as the endpoint default, so the header must report
// that same default page size, or a client deriving its next offset from the
// header would never advance.
func parseListPagination(query url.Values, defaultLimit, maxLimit int) (int, int, error) {
	limit := defaultLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return 0, 0, fmt.Errorf("invalid limit value %q", raw)
		}
		if parsed > maxLimit {
			parsed = maxLimit
		}
		if parsed == 0 {
			parsed = defaultLimit
		}
		limit = parsed
	}
	offset := 0
	if raw := strings.TrimSpace(query.Get("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return 0, 0, fmt.Errorf("invalid offset value %q", raw)
		}
		offset = parsed
	}
	return limit, offset, nil
}

// writeListPaginationHeaders reports the effective page the paged legacy list
// endpoints answered: the limit actually applied (after clamping), the offset,
// and whether another page exists.
func writeListPaginationHeaders(w http.ResponseWriter, limit, offset int, hasMore bool) {
	w.Header().Set("X-NexusGate-Limit", strconv.Itoa(limit))
	w.Header().Set("X-NexusGate-Offset", strconv.Itoa(offset))
	w.Header().Set("X-NexusGate-Has-More", strconv.FormatBool(hasMore))
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
		var values []string
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
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
	if err := validateFacetFilter(&f); err != nil {
		return domain.FacetFilter{}, err
	}
	return f, nil
}

// validateFacetFilter rejects facet values outside the controlled vocabulary
// and inverted duration ranges, for both the query-parameter path
// (parseFacetFilter) and the structured POST body. The shared list
// facetQueryFields keeps the two paths from drifting apart on allowed values.
func validateFacetFilter(f *domain.FacetFilter) error {
	targets := []struct {
		name   string
		values []string
	}{
		{"asset_type", f.AssetTypes},
		{"asset_shot_size", f.ShotSizes},
		{"asset_camera_motion", f.CameraMotions},
		{"asset_audio_type", f.AudioTypes},
		{"asset_quality", f.Qualities},
		{"asset_usable_as", f.UsableAs},
	}
	for i, spec := range facetQueryFields {
		allowed := make(map[string]bool, len(spec.values))
		for _, v := range spec.values {
			allowed[v] = true
		}
		for _, part := range targets[i].values {
			if !allowed[part] {
				return fmt.Errorf("invalid %s value: %q", targets[i].name, part)
			}
		}
	}
	// An inverted range is the same class of mistake as a misspelled facet
	// value: both parse cleanly and compile into SQL that matches nothing, and
	// an empty result is indistinguishable from "you have no footage". Equal
	// bounds stay valid — both ends are inclusive, so that is a legitimate
	// exact-duration query, not an empty one. The check sits after both values
	// have been provided so an unparseable bound still reports as unparseable.
	if f.MinDurationMS != nil && f.MaxDurationMS != nil && *f.MinDurationMS > *f.MaxDurationMS {
		return fmt.Errorf("invalid duration range: min_duration_ms=%d exceeds max_duration_ms=%d", *f.MinDurationMS, *f.MaxDurationMS)
	}
	return nil
}

// validateAssetContextFilter rejects an asset-context status that is not one
// of the six ProcessingStatus literals. The date/region/camera/session fields
// are free text and need no vocabulary check; the status derives from the
// same processingStatusSQL the browse layer renders, so a typo here would
// silently match nothing.
func validateAssetContextFilter(f *domain.AssetContextFilter) error {
	if f == nil || f.Status == "" {
		return nil
	}
	switch f.Status {
	case domain.ProcessingStatusDiscovered, domain.ProcessingStatusQueued,
		domain.ProcessingStatusProcessing, domain.ProcessingStatusReady,
		domain.ProcessingStatusFailed, domain.ProcessingStatusMissing:
		return nil
	default:
		return fmt.Errorf("asset_filter.status: unsupported value %q", f.Status)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// clipText truncates s to at most n bytes, never splitting a UTF-8 rune, and
// marks the cut with the same "…(truncated)" marker truncateMessage uses so
// every truncated string in an envelope is self-advertising. Error envelopes
// bound the message so a relayed or echoed body can never grow the stored job
// error or the browser page without bound.
func clipText(s string, n int) string {
	return truncateMessage(s, n)
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

// writeError answers an unclassified server-side failure. The error envelope's
// classifier decides status and shape; anything it does not recognise is a 500.
func writeError(w http.ResponseWriter, err error) {
	writeErrorEnvelope(w, err)
}

// decodeStrictJSON decodes exactly one bounded JSON value from r.Body and rejects any trailing
// bytes — including bytes already buffered inside json.Decoder that a
// separate drainBody would miss. A successful second Decode (non-EOF) or a
// non-EOF decode error both indicate trailing content: *http.MaxBytesError
// → 413, anything else → 400.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
		}
		return false
	}
	// Second Decode on the same decoder to catch trailing content, including
	// bytes json.Decoder buffered from the underlying reader.
	var dummy struct{}
	if err := dec.Decode(&dummy); err == nil {
		if _, drainErr := io.ReadAll(body); drainErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(drainErr, &maxBytesErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
				return false
			}
		}
		// Another JSON value parsed successfully — trailing content.
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
		return false
	} else if !errors.Is(err, io.EOF) {
		if _, drainErr := io.ReadAll(body); drainErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(drainErr, &maxBytesErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
				return false
			}
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
		}
		return false
	}
	return true
}

// decodeOptionalStrictJSON preserves the historical empty-body behavior for
// the two pipeline control endpoints while keeping non-empty bodies strict.
func decodeOptionalStrictJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) (present, ok bool) {
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return false, true
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
		}
		return true, false
	}
	var dummy struct{}
	if err := dec.Decode(&dummy); err != io.EOF {
		if _, drainErr := io.ReadAll(body); drainErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(drainErr, &maxBytesErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
				return true, false
			}
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
		} else {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid request body"})
		}
		return true, false
	}
	if _, err := io.ReadAll(body); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, APIError{Code: "request_body_too_large", Message: "request body too large"})
			return true, false
		}
	}
	return true, true
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
	limit, offset, err := parseListPagination(r.URL.Query(), 100, legacyLimitMax)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	jobs, hasMore, err := s.service.ListJobsPage(r.Context(), limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	writeListPaginationHeaders(w, limit, offset, hasMore)
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

// costSummary serves the cost ledger's today/month estimates. The payload is
// two sums in the channels' configured relative unit — a guide, never a
// billing record — with no per-run detail, so it needs no admin gate: the
// same class of aggregate as jobSummary.
func (s *Server) costSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.service.CostSummary(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// jobIssues serves the failure backlog grouped by category. Like jobSummary
// it needs no admin gate: the payload is counts, category codes and a
// representative asset id — never last_error_message, which can embed a
// truncated provider response body and stays behind the admin token
// everywhere else.
func (s *Server) jobIssues(w http.ResponseWriter, r *http.Request) {
	issues, err := s.service.JobIssues(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, issues)
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
	if !decodeStrictJSON(w, r, &input, 16<<10) {
		return
	}
	// Validate mode and worker_id before calling the service so an invalid
	// value is a 400, not a 500 from writeError.
	if input.Mode != remote.WorkerAssignmentAny && input.Mode != remote.WorkerAssignmentPreferred && input.Mode != remote.WorkerAssignmentRequired {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "unsupported worker assignment mode"})
		return
	}
	if input.Mode != remote.WorkerAssignmentAny && strings.TrimSpace(input.WorkerID) == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "worker ID is required for a worker assignment"})
		return
	}
	if err := s.service.SetDeriveWorkerAssignment(r.Context(), r.PathValue("id"), strings.TrimSpace(input.WorkerID), input.Mode); err != nil {
		if errors.Is(err, domain.ErrJobNotAssignable) {
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
			return
		}
		if errors.Is(err, domain.ErrInvalidAssignment) {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
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

// setupStatus serves the first-run environment snapshot behind /setup: binary
// presence, directory writability, free disk, database health and the counts
// that drive the next-step heuristic. It is a trusted read, not admin-only,
// because the page it feeds is the one shown before anything has been
// configured.
func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	status := s.service.SetupStatus(r.Context())
	status.AdminAuth = s.service.AdminAuth()
	writeJSON(w, http.StatusOK, status)
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

func (s *Server) storageOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := s.service.StorageOverview(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) savePipelineThrottle(w http.ResponseWriter, r *http.Request) {
	var throttle domain.PipelineThrottle
	if !decodeStrictJSON(w, r, &throttle, 8<<10) {
		return
	}
	if err := throttle.Validate(); err != nil {
		// A rejected throttle is an operator mistake, not a server fault, and the
		// message names the offending field — it is the only feedback the page has.
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
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
//
// An optional {"category":"..."} body narrows the revive to one failure
// category — the strings GET /api/v1/issues reports, so the issues view can
// retry one row at a time. An absent or empty body keeps the original
// all-failed behavior, and a malformed body is refused outright rather than
// silently requeueing everything the caller meant to scope.
func (s *Server) retryFailedJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string `json:"category"`
	}
	if _, ok := decodeOptionalStrictJSON(w, r, &req, 8<<10); !ok {
		return
	}
	var (
		requeued int
		err      error
	)
	if req.Category == "" {
		requeued, err = s.service.RequeueFailedJobs(r.Context())
	} else {
		requeued, err = s.service.RequeueFailedJobsByCategory(r.Context(), req.Category)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"requeued": requeued})
}

// resumeDeferredJobs releases work parked on wall-clock waits early. An
// optional {"reason":"..."} body scopes the release to one deferral category
// (provider_route_exhausted, disk_space_low or budget_exhausted — the codes
// the issues view lists as auto-recovering); an absent or empty body keeps
// the historical default, which is provider_route_exhausted — the only
// reason anything could park when this endpoint first shipped.
func (s *Server) resumeDeferredJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if _, ok := decodeOptionalStrictJSON(w, r, &req, 8<<10); !ok {
		return
	}
	var (
		resumed int
		err     error
	)
	if req.Reason == "" {
		resumed, err = s.service.ResumeDeferredJobs(r.Context())
	} else {
		resumed, err = s.service.ResumeDeferredJobsByCategory(r.Context(), req.Reason)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"resumed": resumed})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "missing q"})
		return
	}
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
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
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "missing q"})
		return
	}
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	hits, err := s.service.SearchShotsFiltered(r.Context(), q, parseBoundedInt(r.URL.Query().Get("limit"), 100, legacyLimitMax), facets)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) searchShotsV2(w http.ResponseWriter, r *http.Request) {
	var req search.SearchRequest
	if !decodeStrictJSON(w, r, &req, 1<<20) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "missing query"})
		return
	}
	if !search.ValidMode(req.Mode) {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "unknown mode: " + req.Mode})
		return
	}
	if req.Offset < 0 {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "offset must not be negative"})
		return
	}
	if err := search.ValidatePagination(req.Limit, req.Offset); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	if err := validateFacetFilter(&req.Facets); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	if err := validateAssetContextFilter(req.AssetFilter); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	response, err := s.service.SearchV2(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) hybridSearchShots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "missing q"})
		return
	}
	facets, err := parseFacetFilter(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	hits, err := s.service.HybridSearchShotsFiltered(r.Context(), q, parseBoundedInt(r.URL.Query().Get("limit"), 100, legacyLimitMax), facets)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) similarShots(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	facets, err := parseFacetFilter(query)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	// The library page's main search carries the caller's active asset
	// context alongside facets (POST /search/shots' asset_filter); this GET
	// sibling had carried neither until now, so "similar shots" could surface
	// footage the operator had just filtered out of view. Same 400-on-bad-input
	// discipline as that path: an unparseable date or an unknown status must
	// not silently compile into a WHERE clause that matches nothing.
	assetFilter, err := assetContextFilterFromQuery(query)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	if err := validateAssetContextFilter(&assetFilter); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	hits, err := s.service.SimilarShotsFiltered(r.Context(), r.PathValue("id"), parseBoundedInt(query.Get("limit"), 20, legacyLimitMax), facets, assetFilter)
	if err != nil {
		// Matched as a sentinel rather than by message prefix: this used to be
		// strings.HasPrefix(err.Error(), "shot not found:"), which made the
		// difference between 404 and 500 depend on repository wording that no
		// compiler checks. Rewording the error there — ordinary maintenance —
		// silently downgraded a missing shot to a 500.
		if errors.Is(err, domain.ErrShotVectorNotFound) {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) rareShots(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.DiscoverRareShots(r.Context(), parseBoundedInt(r.URL.Query().Get("limit"), 50, legacyLimitMax))
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
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	ids, err := parseAssetIDs(query.Get("ids"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
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
	limit, offset, err := parseListPagination(query, defaultLimit, legacyLimitMax)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	filter := domain.AssetCardFilter{
		Limit:       limit,
		Offset:      offset,
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
	cards, hasMore, err := s.service.ListAssetCardsPage(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeListPaginationHeaders(w, limit, offset, hasMore)
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

// assetContextFilterFromQuery parses the GET query-string encoding of
// domain.AssetContextFilter for /api/v1/shots/{id}/similar. It intentionally
// reuses collectionFilterFromQuery's param names (date_from, date_to, region,
// camera, session, status) rather than the JSON field names AssetContextFilter
// itself carries (captured_from, region_label, ...): those JSON names are
// POST /search/shots' request-body wire format, but nothing here is
// JSON-decoded, and the query-string convention is what the library page's
// filterQuery() and every other GET browse endpoint already emit. An
// unparseable date must 400 rather than silently compile into a WHERE clause
// that matches nothing — the same rule collectionFilterFromQuery applies, for
// the same reason.
func assetContextFilterFromQuery(query url.Values) (domain.AssetContextFilter, error) {
	filter := domain.AssetContextFilter{
		RegionLabel: query.Get("region"),
		CameraModel: query.Get("camera"),
		SessionID:   query.Get("session"),
		Status:      domain.ProcessingStatus(query.Get("status")),
	}
	if value := query.Get("date_from"); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			return domain.AssetContextFilter{}, fmt.Errorf("invalid date_from value: %q", value)
		}
		filter.CapturedFrom = &parsed
	}
	if value := query.Get("date_to"); value != "" {
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil {
			return domain.AssetContextFilter{}, fmt.Errorf("invalid date_to value: %q", value)
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
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
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
		collections = []domain.CollectionSummary{}
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	writeJSON(w, http.StatusOK, collection)
}

func (s *Server) listCollectionAssets(w http.ResponseWriter, r *http.Request) {
	// A known collection with zero matching assets is a valid empty page; an
	// unknown collection id is a 404, so a stale client gets told to check the
	// identifier instead of an empty list it would read as "no footage".
	exists, err := s.service.CollectionExists(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !exists {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found", Action: "check_the_identifier"})
		return
	}
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
	if !decodeStrictJSON(w, r, &collection, 64<<10) {
		return
	}
	// Validate required fields before calling the service so an invalid
	// value is a 400/422, not a 500 from writeError.
	collection.Name = strings.TrimSpace(collection.Name)
	if collection.Name == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "collection name is required"})
		return
	}
	if len(collection.Name) > 200 {
		writeAPIError(w, http.StatusUnprocessableEntity, APIError{Code: "unprocessable_entity", Message: "collection name is too long"})
		return
	}
	if len(collection.Description) > 2000 {
		writeAPIError(w, http.StatusUnprocessableEntity, APIError{Code: "unprocessable_entity", Message: "collection description is too long"})
		return
	}
	saved, err := s.service.SaveAssetCollection(r.Context(), collection)
	if err != nil {
		if errors.Is(err, domain.ErrCollectionExists) {
			writeAPIError(w, http.StatusConflict, APIError{Code: "conflict", Message: "collection name already exists", Retryable: true})
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

// addCollectionShot pins a shot into a collection's basket. The repository
// absorbs a duplicate pin as a no-op (the collection_shots primary key), so a
// retried POST answers the same 201 as the first — the call is idempotent by
// construction, and the client never has to guess whether its earlier request
// landed. A collection id naming nothing surfaces as 404 via
// app.ErrCollectionNotFound; the service refuses before the write, so the
// FK constraint is not the thing that reports it.
func (s *Server) addCollectionShot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShotID string `json:"shot_id"`
	}
	if !decodeStrictJSON(w, r, &body, 64<<10) {
		return
	}
	body.ShotID = strings.TrimSpace(body.ShotID)
	if body.ShotID == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "shot_id is required"})
		return
	}
	if err := s.service.AddShotToCollection(r.Context(), r.PathValue("id"), body.ShotID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// removeCollectionShot unpins a shot. Removing a shot that is not pinned is a
// no-op that still answers 204, matching the repository's idempotent delete.
func (s *Server) removeCollectionShot(w http.ResponseWriter, r *http.Request) {
	if err := s.service.RemoveShotFromCollection(r.Context(), r.PathValue("id"), r.PathValue("shot_id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listCollectionShots answers the basket in display order, joined with the
// shot fields a basket view needs without a second round trip.
func (s *Server) listCollectionShots(w http.ResponseWriter, r *http.Request) {
	shots, err := s.service.ListCollectionShots(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if shots == nil {
		shots = []domain.CollectionShotDetail{}
	}
	writeJSON(w, http.StatusOK, shots)
}

// reorderCollectionShots replaces the basket's display order wholesale. The
// repository rejects a list that is not exactly the collection's current pins,
// so a stale client can never silently drop or inject shots through a reorder.
func (s *Server) reorderCollectionShots(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShotIDs []string `json:"shot_ids"`
	}
	if !decodeStrictJSON(w, r, &body, 64<<10) {
		return
	}
	if err := s.service.ReorderCollectionShots(r.Context(), r.PathValue("id"), body.ShotIDs); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listShootSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, offset, err := parseListPagination(query, 100, legacyLimitMax)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	filter := domain.ShootSessionFilter{
		RootID:      query.Get("root"),
		State:       query.Get("state"),
		RegionLabel: query.Get("region"),
		CameraLabel: query.Get("camera"),
		Limit:       limit,
		Offset:      offset,
	}
	// A present but unparseable date is a 400, not a silently-ignored filter
	// that would read as "no footage in range". An absent value keeps the
	// default.
	if raw := strings.TrimSpace(query.Get("date_from")); raw != "" {
		value, err := time.Parse("2006-01-02", raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: fmt.Sprintf("invalid date_from value %q", raw)})
			return
		}
		filter.StartsAfter = &value
	}
	if raw := strings.TrimSpace(query.Get("date_to")); raw != "" {
		value, err := time.Parse("2006-01-02", raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: fmt.Sprintf("invalid date_to value %q", raw)})
			return
		}
		value = value.AddDate(0, 0, 1)
		filter.StartsBefore = &value
	}
	sessions, hasMore, err := s.service.ListShootSessionsPage(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeListPaginationHeaders(w, limit, offset, hasMore)
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
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
	// The derived-artifact paths are absolute on-disk locations under the
	// Hub's own data directory, which carry the operator's username and cache
	// layout. They sat in this response for every trusted-read caller — LAN
	// peers and any agent token, from any network — while the comment two
	// blocks up promised absolute paths stayed behind the administrator
	// boundary. Nothing reads them: the browser and the Skill both fetch
	// artifacts through /assets/{id}/thumbnail and /proxy, which serve the
	// bytes without naming the file.
	detail.ThumbnailPath = ""
	detail.ProxyPath = ""
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) assetCaptureLocation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	detail, err := s.service.GetAssetDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if detail == nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	if detail.Metadata == nil || detail.Metadata.Latitude == nil || detail.Metadata.Longitude == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":               true,
		"latitude":                *detail.Metadata.Latitude,
		"longitude":               *detail.Metadata.Longitude,
		"precision":               detail.Metadata.LocationPrecision,
		"source":                  detail.Metadata.LocationSource,
		"capture_time_confidence": detail.Metadata.CaptureTimeConfidence,
	})
}

func (s *Server) shotDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := s.service.GetShot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if detail == nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) assetShots(w http.ResponseWriter, r *http.Request) {
	// A known asset with zero canonical shots is a valid empty page; an
	// unknown asset id is a 404 so a stale client gets told to check the
	// identifier instead of an empty list it would read as "not analyzed".
	exists, err := s.service.AssetExists(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !exists {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found", Action: "check_the_identifier"})
		return
	}
	shots, err := s.service.ListAssetShots(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if shots == nil {
		shots = []domain.AssetShot{}
	}
	writeJSON(w, http.StatusOK, shots)
}

func (s *Server) assetTranscript(w http.ResponseWriter, r *http.Request) {
	transcript, err := s.service.AssetTranscript(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, transcript)
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	if _, err := os.Stat(a.LocalPath); err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(a.LocalPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Serve the file from the handle opened after the containment check above,
	// not by letting the handler re-resolve the path: http.ServeFile would
	// re-resolve a.LocalPath (and follow whatever symlink is in place at serve
	// time), which re-opens the check-then-serve race for a local actor with
	// DataDir/CacheDir write access. Opening once here means the file handle
	// being served is the one that was verified to live inside the allowed
	// roots — the path can still be swapped for a different inode after open,
	// but never for a path outside those roots.
	f, err := os.Open(a.LocalPath)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	http.ServeContent(w, r, filepath.Base(a.LocalPath), info.ModTime(), f)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// Belt-and-suspenders: the mux pattern "GET /{$}" already guarantees r.URL.Path == "/",
	// but matching on the path explicitly guards against misregistration or future pattern changes.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Fresh-install routing: a hub that has never been pointed at a library
	// root cannot serve a meaningful library, so the first visit lands on the
	// setup wizard instead of a page that coaches "go add a root" from two
	// hops away. Only this page route is affected — /setup and /worker-setup
	// are separate routes — and only the zero-roots state redirects; a failed
	// count renders the page as today, because a broken read must never
	// bounce the browser away from the library.
	if roots, err := s.service.ListLibraryRoots(r.Context()); err == nil && len(roots) == 0 {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	// The shell nav (app_shell.go) carries the Tags and 模型服务 links now;
	// the legacy header's tags→providers splice died with the header.
	s.serveLocalizedPage(w, r, "/", libraryIndexHTML)
}

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/providers", providersHTML)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/setup", setupHTML)
}

func (s *Server) progressPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/progress", progressHTML)
}

func (s *Server) repurposePage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/repurpose", repurposeWorkspaceHTML)
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListCanonicalTags(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if items == nil {
		items = []domain.CanonicalTag{}
	}
	writeJSON(w, http.StatusOK, items)
}
func (s *Server) listUnresolvedTags(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parseListPagination(r.URL.Query(), 200, legacyLimitMax)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	items, hasMore, err := s.service.ListUnresolvedTagsPage(r.Context(), limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	writeListPaginationHeaders(w, limit, offset, hasMore)
	if items == nil {
		items = []domain.UnresolvedTag{}
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
	state := r.URL.Query().Get("state")
	limit, offset, err := parseListPagination(r.URL.Query(), 200, legacyLimitMax)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	items, hasMore, err := s.service.ListTagProposalsPage(r.Context(), state, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	writeListPaginationHeaders(w, limit, offset, hasMore)
	if items == nil {
		items = []domain.TagProposal{}
	}
	writeJSON(w, http.StatusOK, items)
}
func (s *Server) reviewTagProposal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if !decodeStrictJSON(w, r, &req, 1<<20) {
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
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
	if !decodeStrictJSON(w, r, &brief, 1<<20) {
		return
	}
	if strings.TrimSpace(brief.Brief) == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "brief is required"})
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) listRepurposePlans(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "all"
	}
	limit, offset, err := parseListPagination(r.URL.Query(), 50, 200)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	items, hasMore, err := s.service.ListRepurposePlansPage(r.Context(), status, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	writeListPaginationHeaders(w, limit, offset, hasMore)
	if items == nil {
		items = []domain.RepurposePlanSummary{}
	}
	writeJSON(w, http.StatusOK, items)
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
	if !decodeStrictJSON(w, r, &request, 1<<20) {
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
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
			return
		}
		if errors.Is(err, app.ErrPlanImmutable) {
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
			return
		}
		if errors.Is(err, app.ErrPlanNotFound) {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
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
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid revision"})
		return
	}
	approved, err := s.service.ApproveRepurposePlanRevision(r.Context(), r.PathValue("id"), revision)
	if err != nil {
		if errors.Is(err, app.ErrInvalidRepurposeRevision) {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
			return
		}
		if errors.Is(err, app.ErrPlanRevisionNotFound) {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
			return
		}
		if errors.Is(err, app.ErrPlanRevisionNotDraft) || errors.Is(err, app.ErrPlanRevisionNotLatest) {
			status, apiErr := apiErrorFromError(err)
			writeAPIError(w, status, apiErr)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approved)
}

func (s *Server) exportRepurposePlanEDL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	planID := r.PathValue("id")
	document, err := s.service.ExportPlanEDL(r.Context(), planID)
	if err != nil {
		writeExportError(w, r, err)
		return
	}
	writeExport(w, "text/plain; charset=utf-8", planID+".edl", document)
}

func (s *Server) exportRepurposePlanFCPXML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
	case errors.Is(err, app.ErrPlanNotApproved):
		status, apiErr := apiErrorFromError(err)
		writeAPIError(w, status, apiErr)
	case errors.Is(err, app.ErrPlanNotExportable), errors.Is(err, nleexport.ErrInvalidTimeline):
		writeAPIError(w, http.StatusUnprocessableEntity, APIError{Code: "plan_not_exportable", Message: clipText(err.Error(), 300)})
	default:
		writeError(w, err)
	}
}

func (s *Server) tagsPage(w http.ResponseWriter, r *http.Request) {
	s.serveLocalizedPage(w, r, "/tags", tagsHTML)
}
