// Package hubproxy is the authenticated, route-limited HTTP bridge the desktop
// client puts between its WebView and a Hub.
//
// It exists because the WebView must not hold the Hub bearer token: the native
// side keeps the credential, and every request that leaves the WebView is
// checked against three independent things before the token is attached —
// the request is origin-form, it matches an allowlisted (method, path) route,
// and (for unsafe methods) it comes from the application's own origin with the
// per-launch CSRF nonce. A foreign page, a cross-site form post and a crafted
// absolute-form URL all fail before any credential is involved.
package hubproxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Route is one allowlisted method/path pair. Paths use `{name}` placeholders
// that match exactly one segment; everything else is literal.
type Route struct {
	Method string
	Path   string
}

// Config pins the upstream and the browser-side trust boundary.
type Config struct {
	// Upstream is the fixed Hub base URL. It never changes for the life of a
	// connection; reconnecting builds a new Proxy.
	Upstream *url.URL
	// Bearer is the Hub credential. It is attached by this proxy and never
	// returned to JavaScript.
	Bearer string
	// Origin is the exact application origin, e.g. "wails://wails" on macOS or
	// "http://wails.localhost" where Wails serves from a loopback origin.
	Origin string
	// CSRFHeader is the request header carrying the per-launch nonce.
	CSRFHeader string
	// CSRFNonce is the app-session value. It is not the Hub credential.
	CSRFNonce string
	// Routes is the complete allowlist. An empty list denies everything.
	Routes []Route
	// Client performs the upstream request. Nil uses a client that does not
	// follow redirects, so a 3xx is handed back to the WebView rather than
	// being followed with the credential attached.
	Client *http.Client
}

// Proxy is an http.Handler.
type Proxy struct {
	upstream   *url.URL
	bearer     string
	origin     string
	csrfHeader string
	csrfNonce  string
	client     *http.Client
	routes     []compiledRoute
}

type compiledRoute struct {
	method  string
	pattern *regexp.Regexp
}

// ErrNotAllowed is the uniform refusal: it never says whether the path exists
// on the Hub.
var ErrNotAllowed = errors.New("desktop proxy: request is not allowed")

// New compiles the configuration and validates that the origin and nonce are
// set. A proxy without them would be an unauthenticated credential injector.
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == nil || cfg.Upstream.Host == "" {
		return nil, fmt.Errorf("desktop proxy: upstream is required")
	}
	if cfg.Upstream.Scheme != "https" && !isLoopback(cfg.Upstream.Hostname()) {
		return nil, fmt.Errorf("desktop proxy: upstream must be https unless it is loopback")
	}
	if strings.TrimSpace(cfg.Bearer) == "" {
		return nil, fmt.Errorf("desktop proxy: bearer credential is required")
	}
	if strings.TrimSpace(cfg.Origin) == "" {
		return nil, fmt.Errorf("desktop proxy: application origin is required")
	}
	if strings.TrimSpace(cfg.CSRFHeader) == "" || strings.TrimSpace(cfg.CSRFNonce) == "" {
		return nil, fmt.Errorf("desktop proxy: CSRF header and nonce are required")
	}
	client := cfg.Client
	if client == nil {
		clone := *http.DefaultClient
		clone.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &clone
	}
	proxy := &Proxy{
		upstream:   cfg.Upstream,
		bearer:     cfg.Bearer,
		origin:     cfg.Origin,
		csrfHeader: http.CanonicalHeaderKey(cfg.CSRFHeader),
		csrfNonce:  cfg.CSRFNonce,
		client:     client,
	}
	for _, route := range cfg.Routes {
		compiled, err := compileRoute(route)
		if err != nil {
			return nil, err
		}
		proxy.routes = append(proxy.routes, compiled)
	}
	return proxy, nil
}

func compileRoute(route Route) (compiledRoute, error) {
	if strings.TrimSpace(route.Method) == "" || !strings.HasPrefix(route.Path, "/") {
		return compiledRoute{}, fmt.Errorf("desktop proxy: invalid route %q %q", route.Method, route.Path)
	}
	segments := strings.Split(route.Path, "/")
	var pattern strings.Builder
	pattern.WriteString("^")
	for i, segment := range segments {
		if i > 0 {
			pattern.WriteString("/")
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") && len(segment) > 2 {
			pattern.WriteString(`[^/]+`)
			continue
		}
		pattern.WriteString(regexp.QuoteMeta(segment))
	}
	pattern.WriteString("$")
	compiled, err := regexp.Compile(pattern.String())
	if err != nil {
		return compiledRoute{}, err
	}
	return compiledRoute{method: strings.ToUpper(route.Method), pattern: compiled}, nil
}

// ServeHTTP validates and forwards one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// An absolute-form request names its own destination; go's server accepts
	// that form for proxies, so it has to be refused explicitly rather than
	// inheriting the Host we validated.
	if r.URL.IsAbs() || !strings.HasPrefix(r.RequestURI, "/") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !p.routeAllowed(r.Method, r.URL.Path) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !isSafeMethod(r.Method) {
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Origin")), p.origin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtleConstantTimeCompare(r.Header.Get(p.csrfHeader), p.csrfNonce) == false {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	if isUpgrade(r) || r.Method == http.MethodConnect {
		// Worker lease/credential routes are not on the allowlist either, but
		// refusing upgrades here keeps a future WebSocket endpoint from
		// inheriting credential injection by accident.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	target := *p.upstream
	target.Path = joinPath(p.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	copyHeaders(request.Header, r.Header)
	request.Header.Del("Authorization")
	request.Header.Del("Cookie")
	request.Header.Set("Authorization", "Bearer "+p.bearer)
	request.Host = p.upstream.Host
	request.ContentLength = r.ContentLength

	response, err := p.client.Do(request)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.Header().Del("Set-Cookie")
	w.WriteHeader(response.StatusCode)
	// Stream so Range responses (206) keep their original status, headers and
	// byte offset; buffering would turn a seek into a full download.
	_, _ = ioCopy(w, response.Body)
}

func (p *Proxy) routeAllowed(method, requestPath string) bool {
	cleaned := path.Clean(requestPath)
	if cleaned != requestPath {
		return false
	}
	for _, route := range p.routes {
		if route.method != strings.ToUpper(method) {
			continue
		}
		if route.pattern.MatchString(requestPath) {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func isSafeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func isUpgrade(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Connection"), "upgrade") {
		return true
	}
	return r.Header.Get("Upgrade") != ""
}

func joinPath(base, requestPath string) string {
	base = strings.TrimSuffix(base, "/")
	return base + requestPath
}

var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHop[canonical] {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

// subtleConstantTimeCompare keeps the nonce comparison free of an early exit.
// A nonce is short-lived and random, but the cost of doing this right is zero.
func subtleConstantTimeCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ioCopy is a small indirection so the response body is copied without
// buffering; it exists as a named function so tests can observe that the proxy
// streams rather than reads everything first.
func ioCopy(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
