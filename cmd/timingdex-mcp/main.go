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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func newHubClient() *hubClient {
	return &hubClient{
		baseURL:    strings.TrimRight(os.Getenv("TIMINGDEX_BASE_URL"), "/"),
		agentToken: os.Getenv("TIMINGDEX_AGENT_TOKEN"),
		adminToken: os.Getenv("TIMINGDEX_ADMIN_TOKEN"),
		http:       &http.Client{Timeout: 60 * time.Second},
	}
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

func (c *hubClient) searchFootage(ctx context.Context, q string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 20
	}
	params := url.Values{}
	params.Set("q", q)
	params.Set("limit", fmt.Sprintf("%d", limit))
	var out []map[string]any
	if err := c.do(ctx, http.MethodGet, "/api/v1/search/shots/hybrid?"+params.Encode(), c.agentToken, nil, &out); err != nil {
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
	client := newHubClient()
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
			mcp.WithDescription("Search the footage library for shots matching a query. Returns shot ids, asset ids, time ranges, scores and descriptions. Use to find material before creating an edit plan."),
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
			shots, err := client.searchFootage(ctx, q, limit)
			if err != nil {
				return errResult(err), nil
			}
			raw, _ := json.MarshalIndent(shots, "", "  ")
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
