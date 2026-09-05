package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// This gate exists because mcp-usage.md's tools table is a hand-maintained
// restatement of registerTools: same six names, same six Hub endpoints, kept
// in a markdown table nobody re-generates from the source. TestToolNamesRegistered
// already pins the registered set against a hardcoded literal in this
// package; this file instead pins it against the doc, and additionally
// checks that each documented Hub endpoint is one the tool's own Go code
// actually requests — so a tool rewired to call a different endpoint, or a
// new tool added without a doc row, fails here instead of at the next audit.

// repoRootFromMCPTest locates the repository root from this test file's own
// path, the same runtime.Caller technique search_relevance_eval_test.go uses.
func repoRootFromMCPTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

// toolEndpoint is one Hub endpoint documented for one tool in mcp-usage.md's
// table.
type toolEndpoint struct {
	method string
	path   string
}

// mcpUsageRowRe matches one row of a markdown table whose first cell is a
// single backtick-quoted lowercase identifier. This matches both the tools
// table and the unrelated "search_shots arguments" table further down the
// same file; parseMCPUsageTable filters the latter out by requiring the
// second cell to actually name a Hub endpoint.
var mcpUsageRowRe = regexp.MustCompile("^\\| `([a-z0-9_]+)` \\| (.+?) \\|.*\\|\\s*$")

// parseHubEndpointCell parses one "Hub endpoint" table cell. Most tools
// document a single "METHOD /path"; inspect_library's cell packs four
// endpoints behind one shared method and /api/v1 prefix ("GET /api/v1/health,
// /hardware, /setup/status, /jobs/summary"), so three of the four have no
// literal /api/v1/ of their own.
func parseHubEndpointCell(t *testing.T, tool, cell string) []toolEndpoint {
	t.Helper()
	fields := strings.Split(cell, ",")
	first := strings.Fields(strings.TrimSpace(fields[0]))
	if len(first) != 2 {
		t.Fatalf("mcp-usage.md: could not parse Hub endpoint cell for tool %q: %q", tool, cell)
	}
	method, path := first[0], first[1]
	out := []toolEndpoint{{method: method, path: path}}
	for _, frag := range fields[1:] {
		frag = strings.TrimSpace(frag)
		if frag == "" {
			continue
		}
		out = append(out, toolEndpoint{method: method, path: "/api/v1" + frag})
	}
	return out
}

// parseMCPUsageTable returns the tool names and per-tool Hub endpoints from
// mcp-usage.md's "## Tools" table.
func parseMCPUsageTable(t *testing.T, content string) (names []string, endpoints map[string][]toolEndpoint) {
	t.Helper()
	endpoints = map[string][]toolEndpoint{}
	for _, line := range strings.Split(content, "\n") {
		m := mcpUsageRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name, cell := m[1], m[2]
		if !strings.Contains(cell, "/api/v1/") {
			continue
		}
		names = append(names, name)
		endpoints[name] = parseHubEndpointCell(t, name, cell)
	}
	if len(names) == 0 {
		t.Fatal("mcp-usage.md: found no tool rows in the Tools table; the table shape likely changed")
	}
	return names, endpoints
}

// parseToolClientMethods walks registerTools in the parsed main.go AST and
// returns, for each tool name passed to mcp.NewTool, the name of the
// *hubClient method its handler closure calls. This is how the gate finds
// "the Go code for that tool" mechanically rather than by a hand-maintained
// tool-name-to-method table that could itself drift.
func parseToolClientMethods(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	var registerTools *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "registerTools" {
			registerTools = fn
			break
		}
	}
	if registerTools == nil {
		t.Fatal("main.go: no registerTools function declaration found")
	}
	result := map[string]string{}
	ast.Inspect(registerTools.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddTool" || len(call.Args) < 2 {
			return true
		}
		newToolCall, ok := call.Args[0].(*ast.CallExpr)
		if !ok || len(newToolCall.Args) < 1 {
			return true
		}
		nameLit, ok := newToolCall.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		name, err := strconv.Unquote(nameLit.Value)
		if err != nil {
			return true
		}
		handler, ok := call.Args[1].(*ast.FuncLit)
		if !ok {
			return true
		}
		if method := findClientMethodCall(handler.Body); method != "" {
			result[name] = method
		}
		return true
	})
	return result
}

// findClientMethodCall returns the name of the first client.<Method>(...)
// call found in body, or "" if none.
func findClientMethodCall(body ast.Node) string {
	var found string
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "client" {
			return true
		}
		found = sel.Sel.Name
		return false
	})
	return found
}

// methodBodySource returns the exact source text of the named *hubClient
// method, sliced out of src by the parsed AST node's own positions.
func methodBodySource(t *testing.T, fset *token.FileSet, file *ast.File, src []byte, methodName string) string {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != methodName {
			continue
		}
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		return string(src[start:end])
	}
	t.Fatalf("main.go: no method %q declared on *hubClient", methodName)
	return ""
}

// methodTokenPresent reports whether body's source plausibly issues the given
// HTTP method. do() takes the method as an explicit http.MethodX argument;
// getLarge is GET-only by construction (it always calls
// http.NewRequestWithContext(ctx, http.MethodGet, ...)), so a method body
// that decodes through getLarge counts as GET without spelling the constant
// out itself.
func methodTokenPresent(body, method string) bool {
	switch method {
	case "GET":
		return strings.Contains(body, "http.MethodGet") || strings.Contains(body, ".getLarge(")
	case "POST":
		return strings.Contains(body, "http.MethodPost")
	case "PUT":
		return strings.Contains(body, "http.MethodPut")
	case "DELETE":
		return strings.Contains(body, "http.MethodDelete")
	case "PATCH":
		return strings.Contains(body, "http.MethodPatch")
	default:
		return false
	}
}

var toolContractWildcardRe = regexp.MustCompile(`\{[^}]*\}`)

// literalFragmentsPresentInOrder reports whether every literal fragment of
// path (the parts outside {wildcard} segments) appears in body, in order. The
// Hub client builds a path like "/api/v1/assets/" + url.PathEscape(assetID) +
// "/shots" by string concatenation, so an exact-string match against
// "/api/v1/assets/{id}/shots" is never expected to succeed; this checks that
// the surrounding literal pieces are the ones the code actually concatenates.
func literalFragmentsPresentInOrder(body, path string) bool {
	fragments := toolContractWildcardRe.Split(path, -1)
	pos := 0
	for _, frag := range fragments {
		if frag == "" {
			continue
		}
		idx := strings.Index(body[pos:], frag)
		if idx < 0 {
			return false
		}
		pos += idx + len(frag)
	}
	return true
}

func diffToolNameSets(a, b []string) (onlyInA, onlyInB []string) {
	setA := make(map[string]bool, len(a))
	for _, v := range a {
		setA[v] = true
	}
	setB := make(map[string]bool, len(b))
	for _, v := range b {
		setB[v] = true
	}
	for v := range setA {
		if !setB[v] {
			onlyInA = append(onlyInA, v)
		}
	}
	for v := range setB {
		if !setA[v] {
			onlyInB = append(onlyInB, v)
		}
	}
	sort.Strings(onlyInA)
	sort.Strings(onlyInB)
	return onlyInA, onlyInB
}

// TestMCPToolsMatchUsageDoc pins the registered tool set against
// mcp-usage.md's table (the same set TestToolNamesRegistered pins against a
// hardcoded literal — this is the doc-drift half of that same guarantee), and
// checks that each tool's documented Hub endpoint names a path the tool's own
// hubClient method actually requests with the documented HTTP method.
func TestMCPToolsMatchUsageDoc(t *testing.T) {
	root := repoRootFromMCPTest(t)

	mainGoPath := filepath.Join(root, "cmd/nexusgate-mcp/main.go")
	src, err := os.ReadFile(mainGoPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainGoPath, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainGoPath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mainGoPath, err)
	}
	toolMethods := parseToolClientMethods(t, file)

	t.Setenv("NEXUSGATE_BASE_URL", "http://127.0.0.1:8787")
	client, err := newHubClient()
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewMCPServer("nexusgate", "v0.1", server.WithToolCapabilities(true))
	registerTools(srv, client)
	var registered []string
	for name := range srv.ListTools() {
		registered = append(registered, name)
	}
	sort.Strings(registered)

	docPath := filepath.Join(root, "skills/nexusgate/references/mcp-usage.md")
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	docTools, docEndpoints := parseMCPUsageTable(t, string(docBytes))
	sort.Strings(docTools)

	if onlyRegistered, onlyDocumented := diffToolNameSets(registered, docTools); len(onlyRegistered) > 0 || len(onlyDocumented) > 0 {
		t.Fatalf("registered MCP tool names differ from mcp-usage.md's Tools table: only registered=%v only documented=%v", onlyRegistered, onlyDocumented)
	}

	for _, name := range registered {
		endpoints := docEndpoints[name]
		methodName, ok := toolMethods[name]
		if !ok {
			t.Errorf("registerTools: could not determine which hubClient method backs tool %q (its handler calls no client.<Method>(...))", name)
			continue
		}
		body := methodBodySource(t, fset, file, src, methodName)
		for _, ep := range endpoints {
			if !methodTokenPresent(body, ep.method) {
				t.Errorf("tool %q: hubClient.%s does not appear to issue %s, which mcp-usage.md documents as its Hub endpoint (%s %s)", name, methodName, ep.method, ep.method, ep.path)
			}
			if !literalFragmentsPresentInOrder(body, ep.path) {
				t.Errorf("tool %q: hubClient.%s's source does not reference the path %s documented in mcp-usage.md as its Hub endpoint", name, methodName, ep.path)
			}
		}
	}
}
