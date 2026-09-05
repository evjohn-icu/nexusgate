// Command nexusgate-mcp exposes NexusGate's semantic library to MCP-capable
// agents (Codex, Claude Code, Cursor, ...) over stdio.
//
// It is a read-only thin client of the Hub's HTTP API: every tool calls the
// same /api/v1/... endpoints a browser or a skills-based agent would call,
// using the agent token the operator configured. Nothing in this binary
// reaches into the library directly — the Hub remains the only process that
// touches the NAS.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/evjohn-icu/nexusgate/internal/apiclient"
)

// hubClient talks to the NexusGate Hub HTTP API.
type hubClient struct {
	baseURL    string
	agentToken string
	http       *http.Client
}

// newHubClient builds the Hub API client. An https base URL requires
// NEXUSGATE_HUB_FINGERPRINT: cross-machine deployments use the Hub's
// self-signed certificate, and accepting it unpinned would silently trust any
// attacker in the path — the exact boundary fingerprint pinning exists for.
// http:// stays available only for loopback/link-local development (see
// httpAllowedForLocalDevelopment); pointing it at any other host would send
// the agent token in cleartext, mirroring the worker CLI's refusal of
// non-https Hubs.
func newHubClient() (*hubClient, error) {
	baseURL := strings.TrimRight(os.Getenv("NEXUSGATE_BASE_URL"), "/")
	if baseURL == "" {
		// The default Hub endpoint is the local self-signed HTTPS server.
		// Defaulting to https is what makes the unconfigured case safe: it
		// forces NEXUSGATE_HUB_FINGERPRINT below instead of silently talking
		// to an unpinned Hub identity over plaintext.
		baseURL = "https://127.0.0.1:8787"
	}
	fingerprint := strings.TrimSpace(os.Getenv("NEXUSGATE_HUB_FINGERPRINT"))
	if fingerprint != "" && !ValidFingerprint(fingerprint) {
		return nil, fmt.Errorf("NEXUSGATE_HUB_FINGERPRINT must be a 64-character SHA-256 hex fingerprint")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if fingerprint != "" {
		expected := normalizeFingerprint(fingerprint)
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return fmt.Errorf("Hub did not present a certificate")
				}
				sum := sha256.Sum256(rawCerts[0])
				if normalizeFingerprint(hex.EncodeToString(sum[:])) != expected {
					return fmt.Errorf("Hub certificate fingerprint mismatch")
				}
				return nil
			},
		}
	}
	if strings.HasPrefix(strings.ToLower(baseURL), "https://") && fingerprint == "" {
		return nil, fmt.Errorf("NEXUSGATE_HUB_FINGERPRINT is not set: the Hub base URL is https (%s) and accepting an unpinned certificate would silently trust any attacker in the path; set NEXUSGATE_HUB_FINGERPRINT, or use an explicit loopback http:// base URL for TLS-off development", baseURL)
	}
	if strings.HasPrefix(strings.ToLower(baseURL), "http://") && !httpAllowedForLocalDevelopment(hostFromBaseURL(baseURL)) {
		return nil, fmt.Errorf("NEXUSGATE_BASE_URL is http:// and the host is not loopback or link-local: refusing to send the agent token in cleartext to a remote Hub; use https://")
	}
	return &hubClient{
		baseURL:    baseURL,
		agentToken: os.Getenv("NEXUSGATE_AGENT_TOKEN"),
		http:       &http.Client{Transport: transport, Timeout: 60 * time.Second},
	}, nil
}

// hostFromBaseURL returns the host portion (with any port) of a base URL, or
// "" when the URL does not parse.
func hostFromBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// httpAllowedForLocalDevelopment reports whether an http:// Hub base URL may
// carry the agent token in cleartext: only loopback (127.0.0.0/8, ::1, plus
// the "localhost" hostname) and link-local (169.254.0.0/16, fe80::/10)
// destinations qualify. Any other host is remote and must be reached over
// https with a pinned fingerprint, matching the worker CLI's https-only
// enrollment stance.
func httpAllowedForLocalDevelopment(host string) bool {
	if host == "" {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			return false
		}
		addr, err = netip.ParseAddr(h)
		if err != nil {
			return strings.EqualFold(h, "localhost")
		}
	}
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fe80::/10"),
	} {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ValidFingerprint accepts only the canonical 64-character SHA-256 hex form.
func ValidFingerprint(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func normalizeFingerprint(value string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "sha256", "").Replace(value))
}

func (c *hubClient) base() string {
	if c.baseURL == "" {
		return "http://127.0.0.1:8787"
	}
	return c.baseURL
}

// do performs a request with the given bearer token (may be empty for
// trusted-network reads). It decodes a 2xx JSON body into out when non-nil.
func (c *hubClient) do(ctx context.Context, method, path string, token string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return apiclient.DecodeError(resp.StatusCode, resp.Status, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// --- tool implementations -------------------------------------------------

func (c *hubClient) inspectLibrary(ctx context.Context) (map[string]any, error) {
	result := map[string]any{}
	var health map[string]any
	// /health is anonymous by design; /hardware sits behind requireTrustedRead,
	// so a client outside the trusted LAN (or Tailnet) needs the agent token
	// or it gets a 403.
	if err := c.do(ctx, http.MethodGet, "/api/v1/health", "", nil, &health); err != nil {
		return nil, err
	}
	result["health"] = health
	var hardware map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/v1/hardware", c.agentToken, nil, &hardware); err != nil {
		return nil, err
	}
	result["hardware"] = hardware
	// Readiness is not just health and hardware: the setup status (healthy
	// roots, runnable providers, searchable shots, index state) and the job
	// summary (queued/running/failed) describe whether the library can
	// actually serve searches, so they ride along with the agent token.
	var setupStatus map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/v1/setup/status", c.agentToken, nil, &setupStatus); err != nil {
		return nil, err
	}
	result["setup_status"] = setupStatus
	var jobsSummary map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs/summary", c.agentToken, nil, &jobsSummary); err != nil {
		return nil, err
	}
	result["jobs_summary"] = jobsSummary
	return result, nil
}

// searchShots runs the structured Search v2 endpoint so results carry
// per-constraint evidence (confirmed/possible/contradicted/unknown), not just
// a score — matching how the README positions the search surface. Uses the
// agent token like the other trusted reads.
func (c *hubClient) searchShots(ctx context.Context, q string, limit, offset int, filters map[string]any) (map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	body := map[string]any{
		"query":            q,
		"mode":             "auto",
		"limit":            limit,
		"offset":           offset,
		"include_evidence": true,
	}
	if filters != nil {
		body["facets"] = filters
	}
	var out map[string]any
	if err := c.do(ctx, http.MethodPost, "/api/v1/search/shots", c.agentToken, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// getTimeline returns every shot of one asset with exact time ranges and shot
// descriptions, so an agent can enumerate a whole clip's timeline (all shots,
// not just the matched ones search returns).
func (c *hubClient) getTimeline(ctx context.Context, assetID string) ([]map[string]any, error) {
	var out []map[string]any
	// A valid 2,000-shot timeline can exceed do()'s 1 MiB success-body
	// bound, so the full shot list decodes through getLarge like the
	// transcript and asset detail bodies.
	if err := c.getLarge(ctx, "/api/v1/assets/"+url.PathEscape(assetID)+"/shots", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// maxTranscriptBytes bounds the large-body responses getLarge decodes (a long
// asset's full word stream, an AssetDetail embedding it, and a multi-shot
// timeline), all of which legitimately outgrow do()'s 1 MiB success-body
// bound. The cap still stops a misbehaving Hub from making the process
// allocate without bound; no real response approaches it.
const maxTranscriptBytes int64 = 64 << 20

// getLarge GETs a path whose success body can legitimately exceed do()'s
// 1 MiB bound and decodes it into out. Shared by getTranscript (a long
// asset's word stream), getAsset (AssetDetail embeds the same full
// transcript), and getTimeline (a multi-thousand-shot shot list), which
// would otherwise silently truncate and fail to decode.
func (c *hubClient) getLarge(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return err
	}
	if c.agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.agentToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return apiclient.DecodeError(resp.StatusCode, resp.Status, raw)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTranscriptBytes)).Decode(out); err != nil {
		return err
	}
	return nil
}

// getTranscript returns the asset's word-level timeline transcript. The
// response's source field tells the caller where the timestamps come from:
// "aligned" (word-level forced alignment, the strongest timing evidence) or
// "asr" (sentence-level segments only). Uses the agent token like the other
// trusted reads.
func (c *hubClient) getTranscript(ctx context.Context, assetID string) (map[string]any, error) {
	var out map[string]any
	if err := c.getLarge(ctx, "/api/v1/assets/"+url.PathEscape(assetID)+"/transcript", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// getAsset returns asset metadata by calling the existing asset detail
// endpoint. The response embeds the asset's full transcript, which can exceed
// do()'s 1 MiB bound, so it decodes through getLarge.
func (c *hubClient) getAsset(ctx context.Context, assetID string) (map[string]any, error) {
	var out map[string]any
	if err := c.getLarge(ctx, "/api/v1/assets/"+url.PathEscape(assetID), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// getShot returns a single shot's full detail via the shot detail endpoint.
// Decodes through getLarge like the other transcript-bearing reads: the
// response embeds every aligned word inside the shot's time range, and
// nothing in the tree caps how long a shot may be — a locked-off interview
// single take is one shot. That word list is not unbounded the way an
// asset-level transcript is (it cannot grow past the shot), but a long dense
// take approaches do()'s 1 MiB bound closely enough that the cliff is real,
// and crossing it fails the tool with "unexpected end of JSON input" rather
// than anything an agent could act on.
func (c *hubClient) getShot(ctx context.Context, shotID string) (map[string]any, error) {
	var out map[string]any
	if err := c.getLarge(ctx, "/api/v1/shots/"+url.PathEscape(shotID), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- MCP wiring ------------------------------------------------------------

func toolResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: text}},
	}
}

func errResult(err error) *mcp.CallToolResult {
	text := "error: " + err.Error()
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		// A Hub-envelope failure emits its stable fields as JSON text so an
		// MCP client can switch on code, respect retryable, and follow action
		// instead of parsing prose.
		if raw, marshalErr := json.Marshal(apiErr); marshalErr == nil {
			text = "error: " + string(raw)
		}
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: text}},
	}
}

// requireString extracts a required string argument, returning an error result
// for the MCP client when missing.
// facetKeys is the exact set the Hub's domain.FacetFilter accepts. It is
// duplicated here on purpose: the Hub decodes the request body with
// encoding/json's default behaviour, which drops unknown fields silently, so a
// key this list does not contain would widen the search instead of failing it.
// An agent cannot tell a correctly-filtered result set from an accidentally
// unfiltered one by looking at it, so the only safe place to catch a typo is
// before the request leaves.
var facetKeys = map[string]bool{
	"asset_types": true, "shot_sizes": true, "camera_motions": true,
	"audio_types": true, "qualities": true, "usable_as": true,
	"min_duration_ms": true, "max_duration_ms": true,
}

func facetKeyList() string {
	keys := make([]string, 0, len(facetKeys))
	for k := range facetKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// parseFilters accepts the filters argument in either shape an MCP client
// plausibly sends: the documented JSON string, or a real JSON object, which is
// what a model following its instincts produces. Both are honoured; anything
// else is an error.
//
// The previous version type-asserted to string and ignored every other type,
// so an object argument disappeared and the search ran unfiltered while still
// returning plausible results — a wrong answer with no error attached, which
// is the worst failure this tool can have.
func parseFilters(arg any) (map[string]any, error) {
	switch v := arg.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return validateFacets(v)
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return nil, fmt.Errorf("filters is not valid JSON: %w; expected a JSON object of facet filters, e.g. {\"asset_types\":[\"broll\"]}", err)
		}
		return validateFacets(parsed)
	default:
		return nil, fmt.Errorf("filters must be a JSON object or a JSON object encoded as a string, got %T", arg)
	}
}

func validateFacets(filters map[string]any) (map[string]any, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	var unknown []string
	for k := range filters {
		if !facetKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown facet filter %s; supported keys are %s. The Hub ignores unknown keys, so sending one would silently widen the search rather than narrow it", strings.Join(unknown, ", "), facetKeyList())
	}
	return filters, nil
}

func requireString(args map[string]any, key string) (string, *mcp.CallToolResult) {
	v, ok := args[key].(string)
	if !ok || strings.TrimSpace(v) == "" {
		return "", errResult(fmt.Errorf("missing required argument %q", key))
	}
	return v, nil
}

func main() {
	client, err := newHubClient()
	if err != nil {
		// A hard configuration error (an https base URL without a pinned
		// certificate fingerprint) must fail startup loudly rather than
		// silently connect to an arbitrary Hub identity — that is the one
		// case the no-stderr rule below does not cover.
		fmt.Fprintf(os.Stderr, "nexusgate-mcp: %v\n", err)
		os.Exit(1)
	}
	// No startup logging to stderr: MCP stdio clients treat stderr strictly,
	// and the Hub-side logs already cover token absence at serve time. The
	// tools themselves return clear errors when a token is missing.

	srv := server.NewMCPServer("nexusgate", "v0.1", server.WithToolCapabilities(true))
	registerTools(srv, client)

	if err := server.ServeStdio(srv); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "MCP server stopped: %v\n", err)
		os.Exit(1)
	}
}

// registerTools wires every NexusGate tool onto the MCP server.
func registerTools(srv *server.MCPServer, client *hubClient) {
	srv.AddTool(
		mcp.NewTool("inspect_library",
			mcp.WithDescription("Inspect the NexusGate library readiness: health, hardware, setup status (healthy roots, runnable providers, searchable shots, index state) and job summary (queued/running/failed). Use before searching so you do not plan against an incomplete library."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			info, err := client.inspectLibrary(ctx)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(info, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("search_shots",
			mcp.WithDescription("Search the indexed footage library at shot granularity and return exact source time ranges with evidence-backed matches. Each result carries per-constraint evidence (confirmed/possible/contradicted/unknown) alongside scores — treat the score as a retrieval signal, the evidence as the claim. Returns shot IDs, asset IDs, time ranges, scores, evidence, and descriptions."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query, e.g. 'sunset over water', '城市夜景', or a tag")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of shots (default 20)")),
			mcp.WithNumber("offset", mcp.Description("Optional zero-based offset into the full result list for pagination (default 0)")),
			mcp.WithString("filters", mcp.Description("Optional facet filters, as a JSON object or as a JSON object encoded in a string (both are accepted). Supported keys: asset_types, shot_sizes, camera_motions, audio_types, qualities, usable_as (each a JSON array of strings), min_duration_ms, max_duration_ms (integers). Example: '{\"asset_types\":[\"broll\"]}'. An unknown key is rejected with an error rather than ignored, because an ignored filter returns a wider result set that looks correct.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query, rerr := requireString(req.GetArguments(), "query")
			if rerr != nil {
				return rerr, nil
			}
			limit := 20
			if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
				limit = int(v)
			}
			offset := 0
			if v, ok := req.GetArguments()["offset"].(float64); ok && v >= 0 {
				offset = int(v)
			}
			filters, ferr := parseFilters(req.GetArguments()["filters"])
			if ferr != nil {
				return errResult(ferr), nil
			}
			result, err := client.searchShots(ctx, query, limit, offset, filters)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(result, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("get_timeline",
			mcp.WithDescription("Return every shot of an asset in chronological order with exact start_ms/end_ms time ranges, descriptions, and tags. Use to understand the full timeline context around a matched shot — agents should call this after search_shots to see what comes before and after a candidate."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("The asset id from search_shots results")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			assetID, rerr := requireString(req.GetArguments(), "asset_id")
			if rerr != nil {
				return rerr, nil
			}
			shots, err := client.getTimeline(ctx, assetID)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(shots, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("get_transcript",
			mcp.WithDescription("Return an asset's word-level timeline transcript. The response's source field indicates whether timestamps are word-aligned ('aligned', strongest timing evidence) or sentence-level ('asr'). Use to ground narration or dialogue in precise media time."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("The asset id from search_shots results")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			assetID, rerr := requireString(req.GetArguments(), "asset_id")
			if rerr != nil {
				return rerr, nil
			}
			transcript, err := client.getTranscript(ctx, assetID)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(transcript, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("get_asset",
			mcp.WithDescription("Return asset metadata: source reference, duration, analysis state, shot count, capture metadata, transcript availability, and derived/proxy status. Use to assess whether an asset is fully analyzed — and how many indexed shots it has — before searching its shots."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("The asset id from search_shots results")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			assetID, rerr := requireString(req.GetArguments(), "asset_id")
			if rerr != nil {
				return rerr, nil
			}
			asset, err := client.getAsset(ctx, assetID)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(asset, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("get_shot",
			mcp.WithDescription("Return a single shot's full detail: shot metadata, owning asset, transcript fragment within the shot's time range, tags, canonical metadata, and thumbnail/proxy references. Use to inspect a specific shot returned by search_shots."),
			mcp.WithString("shot_id", mcp.Required(), mcp.Description("The shot id from search_shots results")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			shotID, rerr := requireString(req.GetArguments(), "shot_id")
			if rerr != nil {
				return rerr, nil
			}
			shot, err := client.getShot(ctx, shotID)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(shot, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

}
