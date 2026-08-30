package api

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This gate exists because skills/timingdex/ restates the route table by
// hand, in three different documents, with three different spellings of the
// same wildcard ({id} vs {asset-id} vs {shot-id} vs {plan-id}, {n} where the
// route says {revision}), a literal ellipsis ("POST
// /api/v1/repurpose/plans..."), and one table cell in mcp-usage.md that packs
// four endpoints behind a single shared prefix. A route renamed or removed
// leaves a dangling doc reference that nothing previously caught; a new
// agent-reachable route added to Handler() with no corresponding line in
// skills/timingdex/ is invisible unless someone remembers to grep for it.

// docEndpoint is one endpoint mention found under skills/timingdex/, already
// normalised: method is "" when the surrounding text named no HTTP verb
// (e.g. "read `/api/v1/health`"), and path has every {...} wildcard segment
// collapsed to a single canonical token regardless of the name inside, since
// neither the docs nor the route table itself agree on wildcard names.
type docEndpoint struct {
	method string
	path   string
	file   string // path relative to skills/timingdex/, for failure messages
}

func (d docEndpoint) key() string {
	if d.method == "" {
		return d.path
	}
	return d.method + " " + d.path
}

var (
	wildcardSegmentRe = regexp.MustCompile(`\{[^}]*\}`)
	httpMethodWord    = `GET|POST|PUT|DELETE|PATCH`
	// packedEndpointRe matches a documented endpoint followed by one or more
	// ", /fragment" continuations sharing its method and /api/v1 prefix — the
	// mcp-usage.md tools table packs "GET /api/v1/health, /hardware,
	// /setup/status, /jobs/summary" into a single cell this way, so three of
	// the four endpoints carry no literal /api/v1/ of their own.
	packedEndpointRe = regexp.MustCompile(`(?:` + httpMethodWord + `)\s+(/api/v1/[A-Za-z0-9_\-./{}]+)((?:,\s*/[A-Za-z0-9_\-./{}]+)+)`)
	// singleEndpointRe matches one endpoint mention, with an optional leading
	// HTTP method. The character class deliberately excludes '?', '&', '=',
	// '<', '>' so a query string (e.g. "?q=<query>&limit=20") is never part of
	// the match — normalisation strips the query string by construction,
	// not as a separate post-processing step.
	singleEndpointRe = regexp.MustCompile(`(?:(` + httpMethodWord + `)\s+)?(/api/v1/[A-Za-z0-9_\-./{}]+)`)
)

// normalizeEndpointPath collapses every {...} wildcard to one canonical token
// and strips a trailing literal ellipsis, the shorthand api-contract.md uses
// once for "the two POST /api/v1/repurpose/plans... routes above" instead of
// spelling out both.
func normalizeEndpointPath(path string) string {
	path = strings.TrimSuffix(path, "...")
	return wildcardSegmentRe.ReplaceAllString(path, "{}")
}

// endpointsInText extracts every /api/v1/... endpoint mention in text,
// expanding mcp-usage.md's packed table cell into its implied full paths
// before the generic single-endpoint pass runs.
func endpointsInText(text, file string) []docEndpoint {
	var out []docEndpoint
	for _, m := range packedEndpointRe.FindAllStringSubmatch(text, -1) {
		full, rest := m[1], m[2]
		method := strings.Fields(m[0])[0]
		out = append(out, docEndpoint{method: method, path: normalizeEndpointPath(full), file: file})
		for _, frag := range strings.Split(rest, ",") {
			frag = strings.TrimSpace(frag)
			if frag == "" {
				continue
			}
			out = append(out, docEndpoint{method: method, path: normalizeEndpointPath("/api/v1" + frag), file: file})
		}
	}
	for _, m := range singleEndpointRe.FindAllStringSubmatch(text, -1) {
		out = append(out, docEndpoint{method: m[1], path: normalizeEndpointPath(m[2]), file: file})
	}
	return out
}

// extractDocumentedEndpoints scans every file under skills/timingdex/ for
// /api/v1/... endpoint mentions.
func extractDocumentedEndpoints(t *testing.T, skillsDir string) []docEndpoint {
	t.Helper()
	var all []docEndpoint
	err := filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skillsDir, path)
		if err != nil {
			rel = path
		}
		all = append(all, endpointsInText(string(data), rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", skillsDir, err)
	}
	if len(all) == 0 {
		t.Fatalf("found no /api/v1/... endpoint mentions under %s; the extractor likely broke", skillsDir)
	}
	return all
}

// splitRoutePattern separates a routeSpec.Pattern such as "GET
// /api/v1/health" into its method and path. A handful of patterns carry no
// method at all (the "/api/v1/" catch-all, "/spaces/" for WebDAV) — those are
// kept as path-only.
func splitRoutePattern(pattern string) (method, path string) {
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) == 2 {
		switch parts[0] {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodHead, http.MethodOptions:
			return parts[0], parts[1]
		}
	}
	return "", pattern
}

// routeInventoryIndex is the route table normalised the same way documented
// endpoints are, so the two sides can be compared directly.
type routeInventoryIndex struct {
	exact map[string]routeAuthClass // "METHOD /path" -> auth, wildcards collapsed
	paths map[string]bool           // "/path" alone, any method, wildcards collapsed
}

func buildRouteInventoryIndex(specs []routeSpec) routeInventoryIndex {
	idx := routeInventoryIndex{exact: map[string]routeAuthClass{}, paths: map[string]bool{}}
	for _, spec := range specs {
		method, path := splitRoutePattern(spec.Pattern)
		path = wildcardSegmentRe.ReplaceAllString(path, "{}")
		idx.paths[path] = true
		key := path
		if method != "" {
			key = method + " " + path
		}
		idx.exact[key] = spec.Auth
	}
	return idx
}

// undocumentedAgentRoutes is the maintained exception list for Direction B:
// every agent-reachable route (public / trusted-read / agent-or-admin) must
// either be documented somewhere under skills/timingdex/, or be named here
// with a real reason. Adding a new agent-reachable route to Handler() without
// touching this file means TestAgentReachableRoutesAreDocumentedOrAllowlisted
// fails until someone makes the deliberate choice: document it, or explain
// why not.
//
// Most entries below are browser-UI surface the Skill deliberately never
// touches (SKILL.md's frontmatter: "never use it to ... modify media, or run
// the processing pipeline", and its scope is read-only research plus draft
// plans) — collections, tags, storage, library summary and the worker-setup
// wizard have no equivalent in the HTTP workflow or the MCP tool set.
var undocumentedAgentRoutes = map[string]string{
	"GET /favicon.ico": "browser tab icon; not an /api/v1 endpoint and carries no library data an agent could use",

	"GET /api/v1/assets":                     "bare asset-library grid for the browser UI; the Skill always reaches an asset through search or a known asset_id, never by browsing the full list",
	"GET /api/v1/library/processing-summary": "browser dashboard progress widget (queued/running counts by stage); the Skill's readiness flow uses /api/v1/jobs and /api/v1/jobs/summary instead",
	"GET /api/v1/shoot-sessions":             "browser session-grouping feature for organizing footage by shoot; no Skill or MCP workflow groups by session",
	"GET /api/v1/assets/{}/thumbnail":        "raw JPEG bytes; unusable as an MCP tool result or a Skill HTTP response the agent can reason about",
	"GET /api/v1/assets/{}/proxy":            "raw video bytes; same reason as the thumbnail route",
	"GET /api/v1/storage/overview":           "browser storage-usage dashboard widget (disk/cache accounting); not part of the retrieval or planning workflow",
	"GET /api/v1/pipeline/supervisor":        "browser automation-supervisor status widget; the Skill never runs or supervises the pipeline",
	"GET /api/v1/collections":                "browser curation/collections feature; no Skill or MCP workflow reads or writes collections",
	"GET /api/v1/collections/{}":             "browser curation/collections feature; see /api/v1/collections",
	"GET /api/v1/collections/{}/assets":      "browser curation/collections feature; see /api/v1/collections",
	"GET /api/v1/collections/{}/shots":       "browser curation/collections feature; see /api/v1/collections",
	"GET /api/v1/test-drive/suggestions":     "browser onboarding feature suggesting sample footage to try; unrelated to the retrieval/planning workflow",
	"GET /api/v1/search":                     "legacy pre-v2 search endpoint kept for backward compatibility; the Skill and MCP tools use /search/shots/hybrid and the structured POST /search/shots instead",
	"GET /api/v1/search/shots":               "legacy GET verb at this path, superseded by the documented POST /api/v1/search/shots (Search v2); kept for compatibility, not part of the current workflow",
	"GET /api/v1/tags":                       "browser tag-catalog browsing endpoint; no Skill or MCP workflow browses the tag catalog",
	"GET /api/v1/tags/unresolved":            "Tag Curator's human-review queue; a browser-only workflow with no agent counterpart",
	"GET /api/v1/tags/proposals":             "Tag Curator's human-review queue; see /api/v1/tags/unresolved",
	"GET /api/v1/library/summary":            "AI-generated library summary shown in the browser; no Skill or MCP workflow reads it",
	"GET /api/v1/repurpose/plans":            "bare plan-list endpoint; the documented workflow always creates, gets, or revises one plan by id and never lists all plans",
	"GET /api/v1/hub/worker-setup/context":   "browser Worker-setup wizard context data; unrelated to footage research or plan drafting",
}

// TestSkillsDocumentedEndpointsExistInRouteInventory is Direction A: every
// /api/v1/... endpoint mentioned anywhere under skills/timingdex/ must exist
// in the live route table, whether or not the Skill may actually call it.
// Some documented endpoints (the admin-session routes, the plan-approve and
// export routes) are routeAuthHubAdmin or routeAuthBrowserSession and are
// documented specifically as forbidden to this Skill — they still have to
// exist; this direction never asks whether the Skill may reach them, only
// whether the reference resolves.
func TestSkillsDocumentedEndpointsExistInRouteInventory(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "skills-endpoint-gate-a")
	server := NewServer("", service)
	idx := buildRouteInventoryIndex(server.routeInventory())

	root := agentContractGateRepoRoot(t)
	skillsDir := filepath.Join(root, "skills/timingdex")
	docs := extractDocumentedEndpoints(t, skillsDir)

	seen := map[string]docEndpoint{}
	for _, d := range docs {
		if _, ok := seen[d.key()]; !ok {
			seen[d.key()] = d
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := seen[k]
		if d.method != "" {
			if _, ok := idx.exact[d.key()]; !ok {
				t.Errorf("%s documents %q, which does not exist in the API route inventory (renamed or removed route left a dangling doc reference)", d.file, d.key())
			}
			continue
		}
		if !idx.paths[d.path] {
			t.Errorf("%s documents %q (no HTTP method named in the doc text), which does not exist in the API route inventory under any method", d.file, d.path)
		}
	}
}

// TestAgentReachableRoutesAreDocumentedOrAllowlisted is Direction B: every
// agent-reachable route must be either documented under skills/timingdex/ or
// named in undocumentedAgentRoutes with a reason. A route counts as
// documented if it appears with its exact method, or if it appears at all
// under a mention that named no method (e.g. "read `/api/v1/health`") — the
// latter still proves a human wrote the endpoint down, which is what this
// direction is checking for.
//
// The route count is never hardcoded: one route (webdav-space-delivery) only
// registers when WebDAV is configured, and this walks whatever
// routeInventory() returns.
func TestAgentReachableRoutesAreDocumentedOrAllowlisted(t *testing.T) {
	service := newErrorEnvelopeTestService(t, "skills-endpoint-gate-b")
	server := NewServer("", service)
	specs := server.routeInventory()

	root := agentContractGateRepoRoot(t)
	skillsDir := filepath.Join(root, "skills/timingdex")
	docs := extractDocumentedEndpoints(t, skillsDir)

	docExact := map[string]bool{}
	docPathAny := map[string]bool{}
	for _, d := range docs {
		if d.method != "" {
			docExact[d.key()] = true
		} else {
			docPathAny[d.path] = true
		}
	}

	reachable := map[routeAuthClass]bool{
		routeAuthPublic:       true,
		routeAuthTrustedRead:  true,
		routeAuthAgentOrAdmin: true,
	}

	usedAllowlist := map[string]bool{}
	for _, spec := range specs {
		if !reachable[spec.Auth] {
			continue
		}
		method, path := splitRoutePattern(spec.Pattern)
		path = wildcardSegmentRe.ReplaceAllString(path, "{}")
		key := path
		if method != "" {
			key = method + " " + path
		}
		if docExact[key] || docPathAny[path] {
			continue
		}
		if reason, ok := undocumentedAgentRoutes[key]; ok && strings.TrimSpace(reason) != "" {
			usedAllowlist[key] = true
			continue
		}
		t.Errorf("agent-reachable route %q (auth=%s) is not documented anywhere under skills/timingdex/ and is not in undocumentedAgentRoutes; document it or add an allow-list entry with a reason", key, spec.Auth)
	}

	// An allow-list entry for a route that no longer exists, or that got
	// documented and should have been removed, is exactly the kind of stale
	// exception that made the plugin manifest and skills doc drift invisible
	// in the first place — so a leftover entry fails the gate too.
	for key := range undocumentedAgentRoutes {
		if !usedAllowlist[key] {
			t.Errorf("undocumentedAgentRoutes contains %q, which is not an agent-reachable route needing an exception (route renamed, removed, or since documented — remove the stale entry)", key)
		}
	}
}
