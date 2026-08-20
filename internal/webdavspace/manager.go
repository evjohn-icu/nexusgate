package webdavspace

import (
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// The WebDAV authentication rate-limit constants mirror the Hub's admin login
// budget (internal/api/admin_session.go): a per-source-IP failure counter that
// blocks a source after webdavLoginMaxFails failures for webdavLoginBlock.
// Without it a LAN peer could brute-force the shared credential at bcrypt
// speed, and every failed attempt also spends real CPU.
const (
	webdavLoginWindow   = 5 * time.Minute
	webdavLoginMaxFails = 5
	webdavLoginBlock    = time.Minute

	// webdavBcryptSlots bounds how many bcrypt verifies the WebDAV handler
	// runs concurrently. bcrypt is deliberately expensive, so an unthrottled
	// flood of Basic-Auth requests can occupy every core; this semaphore
	// caps the verify pipeline while the rate limiter above cuts off repeat
	// offenders at the source.
	webdavBcryptSlots = 6
)

type webdavLoginAttempt struct {
	windowStart time.Time
	failures    int
	blockedTill time.Time
}

// Manager owns the WebDAV spaces and their HTTP mounting. It is safe for
// concurrent use: Link/Revoke happen from MCP tool calls while the WebDAV
// handler serves GETs on a different goroutine.
type Manager struct {
	linker   Linker
	accounts AccountStore
	dataDir  string

	mu         sync.RWMutex
	spaces     map[string]*Space
	lockSystem webdav.LockSystem

	loginMu       sync.Mutex
	loginAttempts map[string]webdavLoginAttempt
	bcryptSlots   chan struct{}
}

// NewManager creates a space manager whose assets resolve via linker and
// whose HTTP Basic Auth checks accounts.
func NewManager(linker Linker, accounts AccountStore, dataDir ...string) *Manager {
	dir := ""
	if len(dataDir) > 0 {
		dir = dataDir[0]
	}
	return &Manager{linker: linker, accounts: accounts, dataDir: dir, spaces: map[string]*Space{}, lockSystem: webdav.NewMemLS(), loginAttempts: map[string]webdavLoginAttempt{}, bcryptSlots: make(chan struct{}, webdavBcryptSlots)}
}

// CreateSpace registers a new empty space and returns it.
func (m *Manager) CreateSpace(id string) *Space {
	s := NewSpace(id, m.linker, m.dataDir)
	m.mu.Lock()
	m.spaces[id] = s
	m.mu.Unlock()
	return s
}

// Space returns the space with id, or nil when unknown.
func (m *Manager) Space(id string) *Space {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.spaces[id]
}

// Revoke removes a space entirely; subsequent reads of its paths 404.
func (m *Manager) Revoke(id string) {
	m.mu.Lock()
	delete(m.spaces, id)
	m.mu.Unlock()
}

// SpaceIDs lists all live space ids (for admin listing).
func (m *Manager) SpaceIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.spaces))
	for id := range m.spaces {
		out = append(out, id)
	}
	return out
}

// Handler returns an http.Handler that serves WebDAV under /spaces/<id>/...,
// authenticating every request with HTTP Basic Auth against accounts. The
// credentials are read from the Authorization header; the space id is taken
// from the URL path, so each space is only reachable by the account that owns
// the shared credential (space-level sharing is a future refinement).
func (m *Manager) Handler() http.Handler {
	logger := func(_ *http.Request, err error) {
		if err != nil && !errors.Is(err, webdav.ErrNotImplemented) {
			// Failures here are operational noise (a client probing a
			// method); the space itself reports 4xx/5xx to the client.
			_ = err
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract space id from path: /spaces/<id>/...
		rest := strings.TrimPrefix(r.URL.Path, "/spaces/")
		if rest == r.URL.Path {
			http.NotFound(w, r)
			return
		}
		spaceID := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			spaceID = rest[:i]
		}
		// Authenticate before checking space existence so an attacker cannot
		// probe which spaces exist: both missing space and bad credentials
		// answer 401.
		username, password, ok := parseBasicAuth(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="timingdex webdav"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		loginKey := webdavLoginKey(r)
		// Spend the failure budget check before any bcrypt so a blocked source
		// neither keeps paying the verify cost nor lets us keep running it.
		if retryAfter, limited := m.loginRateLimited(loginKey, time.Now()); limited {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}
		// Bound concurrent bcrypt verifies so a LAN flood cannot occupy all
		// cores; see webdavBcryptSlots.
		select {
		case m.bcryptSlots <- struct{}{}:
			defer func() { <-m.bcryptSlots }()
		case <-r.Context().Done():
			http.Error(w, "authentication aborted", http.StatusServiceUnavailable)
			return
		}
		if err := Authenticate(r.Context(), m.accounts, username, password); err != nil {
			m.recordLoginFailure(loginKey, time.Now())
			w.Header().Set("WWW-Authenticate", `Basic realm="timingdex webdav"`)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		m.clearLoginFailures(loginKey)

		space := m.Space(spaceID)
		if space == nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="timingdex webdav"`)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}

		// Serve the space under its prefix.
		prefix := "/spaces/" + spaceID
		dav := &webdav.Handler{Prefix: prefix, FileSystem: space.NewHandlerFS(prefix), LockSystem: m.lockSystem, Logger: logger}
		dav.ServeHTTP(w, r)
	})
}

// webdavLoginKey identifies a Basic-Auth attempt's source by client IP, so a
// repeat offender on one host cannot lock out everyone else.
func webdavLoginKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

// loginRateLimited reports whether key is currently blocked. It mirrors
// Server.adminLoginRateLimit (internal/api/admin_session.go): a non-blocked
// source whose failure window has lapsed is forgotten. The returned seconds
// feed the Retry-After header.
func (m *Manager) loginRateLimited(key string, now time.Time) (int, bool) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	attempt, ok := m.loginAttempts[key]
	if !ok {
		return 0, false
	}
	if !attempt.blockedTill.IsZero() && now.Before(attempt.blockedTill) {
		return max(1, int(attempt.blockedTill.Sub(now).Seconds()+0.999)), true
	}
	if now.Sub(attempt.windowStart) >= webdavLoginWindow {
		delete(m.loginAttempts, key)
	}
	return 0, false
}

func (m *Manager) recordLoginFailure(key string, now time.Time) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	attempt := m.loginAttempts[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= webdavLoginWindow {
		attempt = webdavLoginAttempt{windowStart: now}
	}
	attempt.failures++
	if attempt.failures >= webdavLoginMaxFails {
		attempt.blockedTill = now.Add(webdavLoginBlock)
	}
	m.loginAttempts[key] = attempt
}

func (m *Manager) clearLoginFailures(key string) {
	m.loginMu.Lock()
	delete(m.loginAttempts, key)
	m.loginMu.Unlock()
}

// parseBasicAuth decodes an HTTP Basic Authorization header value.
func parseBasicAuth(header string) (username, password string, ok bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return "", "", false
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", false
	}
	return user, pass, true
}
