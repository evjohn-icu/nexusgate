// Command timingdex-mcp exposes Timingdex's semantic library and edit-plan
// workflow to MCP-capable agents (Codex, Claude Code, Cursor, ...) over stdio.
//
// It is a thin client of the Hub's HTTP API: every tool calls the same
// /api/v1/... endpoints a browser or a skills-based agent would call, using
// the agent token (and, for footage delivery, the administrator token) the
// operator configured. Nothing in this binary reaches into the library
// directly — the Hub remains the only process that touches the NAS.
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
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// hubClient talks to the Timingdex Hub HTTP API.
type hubClient struct {
	baseURL    string
	agentToken string
	adminToken string
	http       *http.Client
}

// newHubClient builds the Hub API client. An https base URL requires
// TIMINGDEX_HUB_FINGERPRINT: cross-machine deployments use the Hub's
// self-signed certificate, and accepting it unpinned would silently trust any
// attacker in the path — the exact boundary fingerprint pinning exists for.
// http:// stays available only for loopback/link-local development (see
// httpAllowedForLocalDevelopment); pointing it at any other host would send
// the agent and administrator tokens in cleartext, mirroring the worker CLI's
// refusal of non-https Hubs.
func newHubClient() (*hubClient, error) {
	baseURL := strings.TrimRight(os.Getenv("TIMINGDEX_BASE_URL"), "/")
	fingerprint := strings.TrimSpace(os.Getenv("TIMINGDEX_HUB_FINGERPRINT"))
	if fingerprint != "" && !ValidFingerprint(fingerprint) {
		return nil, fmt.Errorf("TIMINGDEX_HUB_FINGERPRINT must be a 64-character SHA-256 hex fingerprint")
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
		return nil, fmt.Errorf("TIMINGDEX_BASE_URL is https but TIMINGDEX_HUB_FINGERPRINT is not set: refusing to accept an unpinned Hub certificate")
	}
	if strings.HasPrefix(strings.ToLower(baseURL), "http://") && !httpAllowedForLocalDevelopment(hostFromBaseURL(baseURL)) {
		return nil, fmt.Errorf("TIMINGDEX_BASE_URL is http:// and the host is not loopback or link-local: refusing to send the agent and administrator tokens in cleartext to a remote Hub; use https://")
	}
	return &hubClient{
		baseURL:    baseURL,
		agentToken: os.Getenv("TIMINGDEX_AGENT_TOKEN"),
		adminToken: os.Getenv("TIMINGDEX_ADMIN_TOKEN"),
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
// carry the agent and administrator tokens in cleartext: only loopback
// (127.0.0.0/8, ::1, plus the "localhost" hostname) and link-local
// (169.254.0.0/16, fe80::/10) destinations qualify. Any other host is remote
// and must be reached over https with a pinned fingerprint, matching the
// worker CLI's https-only enrollment stance.
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
		return fmt.Errorf("Hub API %s %s: %s (%s)", method, path, strings.TrimSpace(string(raw)), resp.Status)
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
	return result, nil
}

// searchFootage runs the structured Search v2 endpoint so results carry
// per-constraint evidence (confirmed/possible/contradicted/unknown), not just
// a score — matching how the README positions the search surface. Uses the
// agent token like the other trusted reads.
func (c *hubClient) searchFootage(ctx context.Context, q string, limit int) (map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	var out map[string]any
	if err := c.do(ctx, http.MethodPost, "/api/v1/search/shots", c.agentToken, map[string]any{
		"query":            q,
		"mode":             "auto",
		"limit":            limit,
		"include_evidence": true,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// getShots returns every shot of one asset with exact time ranges and shot
// descriptions, so an agent can enumerate a whole clip's timeline (all shots,
// not just the matched ones search returns).
func (c *hubClient) getShots(ctx context.Context, assetID string) ([]map[string]any, error) {
	var out []map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/v1/assets/"+url.PathEscape(assetID)+"/shots", c.agentToken, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// maxTranscriptBytes bounds a transcript response body. A word-level
// transcript is the one API response that legitimately outgrows do()'s 1 MiB
// success-body bound (a long asset's full word stream), so getTranscript
// decodes its own body; the cap still stops a misbehaving Hub from making the
// process allocate without bound. No real transcript approaches it.
const maxTranscriptBytes int64 = 64 << 20

// getTranscript returns the asset's word-level timeline transcript. The
// response's source field tells the caller where the timestamps come from:
// "aligned" (word-level forced alignment, the strongest timing evidence) or
// "asr" (sentence-level segments only). Uses the agent token like the other
// trusted reads.
func (c *hubClient) getTranscript(ctx context.Context, assetID string) (map[string]any, error) {
	path := "/api/v1/assets/" + url.PathEscape(assetID) + "/transcript"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	if c.agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.agentToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("Hub API %s %s: %s (%s)", http.MethodGet, path, strings.TrimSpace(string(raw)), resp.Status)
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTranscriptBytes)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

type planResult struct {
	ID string `json:"id"`
}

func (c *hubClient) createEditPlan(ctx context.Context, brief string) (planResult, error) {
	var out planResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/repurpose/plans", c.agentToken, map[string]string{"brief": brief}, &out); err != nil {
		return planResult{}, err
	}
	return out, nil
}

func (c *hubClient) reviseEditPlan(ctx context.Context, planID string, sections any) (planResult, error) {
	var out planResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/repurpose/plans/"+url.PathEscape(planID)+"/revisions", c.agentToken, map[string]any{"sections": sections}, &out); err != nil {
		return planResult{}, err
	}
	return out, nil
}

// requestSourceMedia links an asset's original media into the on-demand
// WebDAV space and returns the path the editing software will mount it at.
// This is the "对面要什么 → 我们软链进 WebDAV 空间" step of the delivery flow.
func (c *hubClient) requestSourceMedia(ctx context.Context, spaceID, assetID string) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, http.MethodPost, "/api/v1/admin/webdav/spaces/"+url.PathEscape(spaceID)+"/links", c.adminToken, map[string]string{"asset_id": assetID, "kind": "original"}, &out); err != nil {
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
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "error: " + err.Error()}},
	}
}

// requireString extracts a required string argument, returning an error result
// for the MCP client when missing.
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
		fmt.Fprintf(os.Stderr, "timingdex-mcp: %v\n", err)
		os.Exit(1)
	}
	// No startup logging to stderr: MCP stdio clients treat stderr strictly,
	// and the Hub-side logs already cover token absence at serve time. The
	// tools themselves return clear errors when a token is missing.

	srv := server.NewMCPServer("timingdex", "v0.1", server.WithToolCapabilities(true))
	registerTools(srv, client)

	if err := server.ServeStdio(srv); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "MCP server stopped: %v\n", err)
		os.Exit(1)
	}
}

// registerTools wires every Timingdex tool onto the MCP server.
func registerTools(srv *server.MCPServer, client *hubClient) {
	srv.AddTool(
		mcp.NewTool("inspect_library",
			mcp.WithDescription("Inspect the Timingdex library readiness: health and hardware report. Use before searching so you do not plan against an incomplete library."),
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
		mcp.NewTool("search_footage",
			mcp.WithDescription("Search the footage library using the structured Search v2 endpoint: results carry per-constraint evidence (confirmed/possible/contradicted/unknown) alongside scores, so you can tell a retrieval signal from an observational claim. Returns shot ids, asset ids, time ranges, scores, evidence and descriptions. Use to find material before creating an edit plan."),
			mcp.WithString("q", mcp.Required(), mcp.Description("Search query, e.g. 'sunset over water', '城市夜景', or a tag")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of shots (default 20)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			q, rerr := requireString(req.GetArguments(), "q")
			if rerr != nil {
				return rerr, nil
			}
			limit := 20
			if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
				limit = int(v)
			}
			result, err := client.searchFootage(ctx, q, limit)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(result, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("get_shots",
			mcp.WithDescription("Return every shot of one asset, with exact start_ms/end_ms time ranges and shot descriptions. Use to enumerate a whole clip's timeline (all shots, not just matched ones) before building an edit plan."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("The asset id from search_footage results")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			assetID, rerr := requireString(req.GetArguments(), "asset_id")
			if rerr != nil {
				return rerr, nil
			}
			shots, err := client.getShots(ctx, assetID)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(shots, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("get_transcript",
			mcp.WithDescription("Return an asset's word-level timeline transcript. The response's source field says where the timestamps come from: 'aligned' (word-level forced alignment — the strongest timing evidence) or 'asr' (sentence-level segments only, no word boundaries). Use to ground narration or dialogue in precise media time."),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("The asset id from search_footage results")),
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
		mcp.NewTool("create_edit_plan",
			mcp.WithDescription("Create a draft edit plan (repurpose plan) from a brief. The plan is a draft that a human approves later — this tool never approves or runs anything."),
			mcp.WithString("brief", mcp.Required(), mcp.Description("Editorial brief describing the video to make")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			brief, rerr := requireString(req.GetArguments(), "brief")
			if rerr != nil {
				return rerr, nil
			}
			plan, err := client.createEditPlan(ctx, brief)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(plan, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("revise_edit_plan",
			mcp.WithDescription("Revise a draft edit plan with concrete sections (each section picks a shot). Revises only; approval stays human."),
			mcp.WithString("plan_id", mcp.Required(), mcp.Description("The plan id returned by create_edit_plan")),
			mcp.WithString("sections_json", mcp.Required(), mcp.Description("JSON array of sections: [{role, query, duration_ms, selected_shot_id, candidates:[{shot_id,asset_id,start_ms,end_ms}]}]")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			planID, rerr := requireString(req.GetArguments(), "plan_id")
			if rerr != nil {
				return rerr, nil
			}
			sectionsRaw, rerr := requireString(req.GetArguments(), "sections_json")
			if rerr != nil {
				return rerr, nil
			}
			var sections any
			if err := json.Unmarshal([]byte(sectionsRaw), &sections); err != nil {
				return errResult(fmt.Errorf("sections_json is not valid JSON: %w", err)), nil
			}
			plan, err := client.reviseEditPlan(ctx, planID, sections)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(plan, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("request_source_media",
			mcp.WithDescription("Request that an asset's ORIGINAL media be made available in the on-demand WebDAV delivery space. Returns the WebDAV path the editing software can mount. The file is streamed from the NAS on demand; nothing is copied and the real path is never exposed."),
			mcp.WithString("space_id", mcp.Required(), mcp.Description("The WebDAV space id (created by the operator for this editing session)")),
			mcp.WithString("asset_id", mcp.Required(), mcp.Description("The asset id from search_footage results")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			spaceID, rerr := requireString(req.GetArguments(), "space_id")
			if rerr != nil {
				return rerr, nil
			}
			assetID, rerr := requireString(req.GetArguments(), "asset_id")
			if rerr != nil {
				return rerr, nil
			}
			path, err := client.requestSourceMedia(ctx, spaceID, assetID)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(path, "", "  ")
			return toolResult(string(raw)), nil
		},
	)

}
