package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	adminSessionCookie = "__Host-timingdex_admin_session"
	adminCSRFCookie    = "__Host-timingdex_csrf"
	adminSessionTTL    = 30 * time.Minute
	adminSessionMaxAge = 8 * time.Hour
	adminSessionLimit  = 64
	adminLoginWindow   = 5 * time.Minute
	adminLoginMaxFails = 5
	adminLoginBlock    = time.Minute
)

type adminSession struct {
	createdAt time.Time
	lastSeen  time.Time
	csrfHash  [sha256.Size]byte
}

type adminLoginAttempt struct {
	windowStart time.Time
	failures    int
	blockedTill time.Time
}

func randomSessionValue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func sessionDigest(value string) [sha256.Size]byte {
	return sha256.Sum256([]byte(value))
}

func (s *Server) createAdminSessionRecord(now time.Time) (string, string, error) {
	sessionValue, err := randomSessionValue()
	if err != nil {
		return "", "", err
	}
	csrfValue, err := randomSessionValue()
	if err != nil {
		return "", "", err
	}
	session := adminSession{createdAt: now, lastSeen: now, csrfHash: sessionDigest(csrfValue)}
	s.adminSessionsMu.Lock()
	pruneAdminSessionsLocked(s.adminSessions, now)
	if s.adminSessions == nil {
		s.adminSessions = make(map[[sha256.Size]byte]adminSession)
	}
	for len(s.adminSessions) >= adminSessionLimit {
		var oldest [sha256.Size]byte
		var oldestAt time.Time
		for digest, candidate := range s.adminSessions {
			if oldestAt.IsZero() || candidate.lastSeen.Before(oldestAt) {
				oldest, oldestAt = digest, candidate.lastSeen
			}
		}
		delete(s.adminSessions, oldest)
	}
	s.adminSessions[sessionDigest(sessionValue)] = session
	s.adminSessionsMu.Unlock()
	return sessionValue, csrfValue, nil
}

func pruneAdminSessionsLocked(sessions map[[sha256.Size]byte]adminSession, now time.Time) {
	for digest, session := range sessions {
		if now.Sub(session.createdAt) >= adminSessionMaxAge || now.Sub(session.lastSeen) >= adminSessionTTL {
			delete(sessions, digest)
		}
	}
}

func adminLoginKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (s *Server) adminLoginRateLimit(key string, now time.Time) (int, bool) {
	s.adminLoginMu.Lock()
	defer s.adminLoginMu.Unlock()
	if s.adminLoginAttempts == nil {
		s.adminLoginAttempts = make(map[string]adminLoginAttempt)
	}
	attempt, ok := s.adminLoginAttempts[key]
	if !ok {
		return 0, false
	}
	if !attempt.blockedTill.IsZero() && now.Before(attempt.blockedTill) {
		return max(1, int(attempt.blockedTill.Sub(now).Seconds()+0.999)), true
	}
	if now.Sub(attempt.windowStart) >= adminLoginWindow {
		delete(s.adminLoginAttempts, key)
	}
	return 0, false
}

func (s *Server) recordAdminLoginFailure(key string, now time.Time) {
	s.adminLoginMu.Lock()
	defer s.adminLoginMu.Unlock()
	attempt := s.adminLoginAttempts[key]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) >= adminLoginWindow {
		attempt = adminLoginAttempt{windowStart: now}
	}
	attempt.failures++
	if attempt.failures >= adminLoginMaxFails {
		attempt.blockedTill = now.Add(adminLoginBlock)
	}
	s.adminLoginAttempts[key] = attempt
}

func (s *Server) clearAdminLoginFailures(key string) {
	s.adminLoginMu.Lock()
	delete(s.adminLoginAttempts, key)
	s.adminLoginMu.Unlock()
}

func (s *Server) revokeAdminSession(value string) {
	if value == "" {
		return
	}
	s.adminSessionsMu.Lock()
	delete(s.adminSessions, sessionDigest(value))
	s.adminSessionsMu.Unlock()
}

func (s *Server) adminSessionForRequest(r *http.Request) (adminSession, bool) {
	if r.TLS == nil {
		return adminSession{}, false
	}
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return adminSession{}, false
	}
	now := time.Now()
	digest := sessionDigest(cookie.Value)
	s.adminSessionsMu.Lock()
	defer s.adminSessionsMu.Unlock()
	pruneAdminSessionsLocked(s.adminSessions, now)
	session, ok := s.adminSessions[digest]
	if !ok {
		return adminSession{}, false
	}
	if now.Sub(session.createdAt) >= adminSessionMaxAge || now.Sub(session.lastSeen) >= adminSessionTTL {
		delete(s.adminSessions, digest)
		return adminSession{}, false
	}
	session.lastSeen = now
	s.adminSessions[digest] = session
	return session, true
}

func (s *Server) sessionCookieValue(r *http.Request) string {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *Server) secureBrowserRequest(r *http.Request) bool {
	return r.TLS != nil
}

func (s *Server) requireSessionCSRF(w http.ResponseWriter, r *http.Request) bool {
	if !s.secureBrowserRequest(r) {
		writeAPIError(w, http.StatusForbidden, APIError{Code: "secure_session_required", Message: "browser administrator sessions require HTTPS"})
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if !sameRequestOrigin(r, origin) {
		writeAPIError(w, http.StatusForbidden, APIError{Code: "csrf_origin_failed", Message: "request origin is not allowed"})
		return false
	}
	cookie, err := r.Cookie(adminCSRFCookie)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		writeAPIError(w, http.StatusForbidden, APIError{Code: "csrf_failed", Message: "CSRF token required"})
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	cookieHash := sessionDigest(cookie.Value)
	providedHash := sessionDigest(provided)
	if provided == "" || subtle.ConstantTimeCompare(cookieHash[:], providedHash[:]) != 1 {
		writeAPIError(w, http.StatusForbidden, APIError{Code: "csrf_failed", Message: "CSRF token invalid"})
		return false
	}
	session, ok := s.adminSessionForRequest(r)
	if !ok || subtle.ConstantTimeCompare(session.csrfHash[:], providedHash[:]) != 1 {
		writeAPIError(w, http.StatusUnauthorized, APIError{Code: "admin_session_expired", Message: "administrator session expired", Action: "login_again"})
		return false
	}
	return true
}

func sameRequestOrigin(r *http.Request, origin string) bool {
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, scheme) && strings.EqualFold(parsed.Host, r.Host)
}

func setAdminSessionCookies(w http.ResponseWriter, sessionValue, csrfValue string) {
	http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: sessionValue, Path: "/", MaxAge: int(adminSessionMaxAge / time.Second), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: adminCSRFCookie, Value: csrfValue, Path: "/", MaxAge: int(adminSessionMaxAge / time.Second), Secure: true, SameSite: http.SameSiteStrictMode})
}

func clearAdminSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: adminCSRFCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, SameSite: http.SameSiteStrictMode})
}

var errSessionHTTPS = errors.New("administrator sessions require HTTPS")
