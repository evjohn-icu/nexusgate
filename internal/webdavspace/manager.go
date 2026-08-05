package webdavspace

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/webdav"
)

// Manager owns the WebDAV spaces and their HTTP mounting. It is safe for
// concurrent use: Link/Revoke happen from MCP tool calls while the WebDAV
// handler serves GETs on a different goroutine.
type Manager struct {
	linker   Linker
	accounts AccountStore

	mu     sync.RWMutex
	spaces map[string]*Space
}

// NewManager creates a space manager whose assets resolve via linker and
// whose HTTP Basic Auth checks accounts.
func NewManager(linker Linker, accounts AccountStore) *Manager {
	return &Manager{linker: linker, accounts: accounts, spaces: map[string]*Space{}}
}

// CreateSpace registers a new empty space and returns it.
func (m *Manager) CreateSpace(id string) *Space {
	s := NewSpace(id, m.linker)
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
	dav := &webdav.Handler{
		LockSystem: webdav.NewMemLS(),
		Logger: func(_ *http.Request, err error) {
			if err != nil && !errors.Is(err, webdav.ErrNotImplemented) {
				// Failures here are operational noise (a client probing a
				// method); the space itself reports 4xx/5xx to the client.
				_ = err
			}
		},
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
		space := m.Space(spaceID)
		if space == nil {
			http.NotFound(w, r)
			return
		}

		// Basic auth.
		username, password, ok := parseBasicAuth(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="timingdex webdav"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := Authenticate(r.Context(), m.accounts, username, password); err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="timingdex webdav"`)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}

		// Serve the space under its prefix.
		prefix := "/spaces/" + spaceID
		dav.Prefix = prefix
		dav.FileSystem = space.NewHandlerFS(prefix)
		dav.ServeHTTP(w, r)
	})
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

var _ = context.Background // keep context import
