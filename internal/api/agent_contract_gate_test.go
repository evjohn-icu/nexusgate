package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// This gate exists because the agent capability contract has drifted from its
// own documentation before without anyone noticing until an audit: SKILL.md
// and api-contract.md restate agentCapabilities' allowed_actions,
// denied_actions and allowed_write_routes by hand, in prose and bullets, and
// nothing previously checked that the restatement still matched the handler.
// A route renamed, an action added to one list and not the other, or
// approval_mode drifting from "human_required" would all build and pass every
// existing test while silently breaking the "agents draft, humans approve"
// contract this Skill depends on.

// agentCapabilitiesContract is a decode target for GET
// /api/v1/agent/capabilities. agentCapabilities (server.go) writes an inline
// map[string]any with no named response type, so this mirrors
// TestHandlerDeclaresBoundedAgentCapabilities's local decode struct
// (server_test.go) rather than introducing a shared type the handler itself
// does not use.
type agentCapabilitiesContract struct {
	ApprovalMode       string   `json:"approval_mode"`
	AllowedActions     []string `json:"allowed_actions"`
	AllowedWriteRoutes []string `json:"allowed_write_routes"`
	DeniedActions      []string `json:"denied_actions"`
}

// agentContractGateRepoRoot locates the repository root from this test file's
// own path, the same runtime.Caller technique
// search_relevance_eval_test.go uses, so the gate resolves skills/timingdex/
// regardless of the working directory `go test` was invoked from.
func agentContractGateRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

// fetchAgentCapabilities serves GET /api/v1/agent/capabilities in-process and
// decodes it, exactly as TestHandlerDeclaresBoundedAgentCapabilities does.
func fetchAgentCapabilities(t *testing.T) agentCapabilitiesContract {
	t.Helper()
	service := newErrorEnvelopeTestService(t, "agent-contract-gate")
	handler := NewServer("", service).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/agent/capabilities", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/agent/capabilities status=%d body=%s", response.Code, response.Body.String())
	}
	var capabilities agentCapabilitiesContract
	if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
		t.Fatalf("decode /api/v1/agent/capabilities: %v", err)
	}
	return capabilities
}

// diffStringSets reports the symmetric difference between a and b treated as
// sets, sorted so a failure message names exactly which item is missing on
// which side instead of "sets differ".
func diffStringSets(a, b []string) (onlyInA, onlyInB []string) {
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

var backtickTokenRe = regexp.MustCompile("`([^`]+)`")

// backtickTokens extracts every `...`-quoted token from s, in order.
func backtickTokens(s string) []string {
	matches := backtickTokenRe.FindAllStringSubmatch(s, -1)
	tokens := make([]string, 0, len(matches))
	for _, m := range matches {
		tokens = append(tokens, m[1])
	}
	return tokens
}

// skillMDBulletItems returns every backtick-quoted token in the SKILL.md
// bullet whose line starts with marker, plus its indented continuation lines.
// SKILL.md wraps each of the three allowlist bullets across one to four lines
// with a two-space continuation indent (a list gaining or losing an item
// reflows the wrap), so this walks from the marker to the first line that is
// no longer a continuation rather than trusting a fixed line range.
func skillMDBulletItems(t *testing.T, lines []string, marker string) []string {
	t.Helper()
	for i, line := range lines {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		var block strings.Builder
		block.WriteString(strings.TrimPrefix(line, marker))
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if next == "" || !strings.HasPrefix(next, "  ") {
				break
			}
			block.WriteString(" ")
			block.WriteString(next)
		}
		return backtickTokens(block.String())
	}
	t.Fatalf("SKILL.md: no bullet starting with %q", marker)
	return nil
}

// skillMDApprovalMode extracts the value out of SKILL.md's single
// `approval_mode: human_required` token (Preconditions step 4).
func skillMDApprovalMode(t *testing.T, content string) string {
	t.Helper()
	re := regexp.MustCompile("`approval_mode:\\s*([a-z_]+)`")
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatal("SKILL.md: could not find an `approval_mode: ...` token")
	}
	return m[1]
}

// proseMarkerPattern turns a plain-English marker into a regexp that tolerates
// the marker's own words being wrapped across a line break in the source
// markdown, without hardcoding where that wrap happens. api-contract.md
// states allowed_actions/denied_actions as prose sentences, not bullets, so a
// literal substring marker would break on the next incidental rewrap.
func proseMarkerPattern(marker string) string {
	fields := strings.Fields(marker)
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = regexp.QuoteMeta(f)
	}
	return strings.Join(parts, `\s+`)
}

// proseBacktickList extracts the backtick-quoted tokens in api-contract.md's
// prose between marker and the sentence-ending period that follows it.
func proseBacktickList(t *testing.T, content, marker string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`(?s)` + proseMarkerPattern(marker) + `\s+(.*?)\.`)
	m := pattern.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("api-contract.md: could not find prose matching %q", marker)
	}
	return backtickTokens(m[1])
}

// apiContractApprovalMode extracts the value api-contract.md declares in
// "Require `approval_mode` to be `human_required`."
func apiContractApprovalMode(t *testing.T, content string) string {
	t.Helper()
	re := regexp.MustCompile(proseMarkerPattern("Require `approval_mode` to be") + "\\s+`([^`]+)`")
	m := re.FindStringSubmatch(content)
	if m == nil {
		t.Fatal("api-contract.md: could not find \"Require `approval_mode` to be `...`\"")
	}
	return m[1]
}

// TestAgentCapabilitiesMatchesSkillMD is the drift item this gate exists for:
// SKILL.md ~34-42 restates all three agentCapabilities lists exhaustively, by
// hand, as markdown bullets. A route renamed or an action added to one side
// and not the other used to only surface at the next manual audit.
func TestAgentCapabilitiesMatchesSkillMD(t *testing.T) {
	capabilities := fetchAgentCapabilities(t)

	root := agentContractGateRepoRoot(t)
	skillPath := filepath.Join(root, "skills/timingdex/SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	content := string(raw)
	lines := strings.Split(content, "\n")

	skillAllowed := skillMDBulletItems(t, lines, "- `allowed_actions`:")
	skillDenied := skillMDBulletItems(t, lines, "- `denied_actions`:")
	skillWriteRoutes := skillMDBulletItems(t, lines, "- Agent-token write routes:")

	if onlyCode, onlyDoc := diffStringSets(capabilities.AllowedActions, skillAllowed); len(onlyCode) > 0 || len(onlyDoc) > 0 {
		t.Errorf("allowed_actions differ between GET /api/v1/agent/capabilities and SKILL.md: only in code=%v only in SKILL.md=%v", onlyCode, onlyDoc)
	}
	if onlyCode, onlyDoc := diffStringSets(capabilities.DeniedActions, skillDenied); len(onlyCode) > 0 || len(onlyDoc) > 0 {
		t.Errorf("denied_actions differ between GET /api/v1/agent/capabilities and SKILL.md: only in code=%v only in SKILL.md=%v", onlyCode, onlyDoc)
	}
	if onlyCode, onlyDoc := diffStringSets(capabilities.AllowedWriteRoutes, skillWriteRoutes); len(onlyCode) > 0 || len(onlyDoc) > 0 {
		t.Errorf("allowed_write_routes differ between GET /api/v1/agent/capabilities and SKILL.md: only in code=%v only in SKILL.md=%v", onlyCode, onlyDoc)
	}

	if capabilities.ApprovalMode != "human_required" {
		t.Errorf("GET /api/v1/agent/capabilities approval_mode = %q, want human_required", capabilities.ApprovalMode)
	}
	if skillApproval := skillMDApprovalMode(t, content); skillApproval != "human_required" {
		t.Errorf("SKILL.md declares approval_mode %q, want human_required", skillApproval)
	}
}

// TestAgentCapabilitiesMatchesAPIContractDoc covers api-contract.md, which
// states the same lists as prose rather than bullets. denied_actions there is
// deliberately partial ("must include" 5 of the 14 denied actions, not all of
// them) so this asserts subset, not equality — do not "fix" this into
// equality later; that would force the doc to duplicate SKILL.md's exhaustive
// list for no reason. allowed_actions is exhaustive there and is asserted
// as equality, matching the doc's own "currently contains" wording.
func TestAgentCapabilitiesMatchesAPIContractDoc(t *testing.T) {
	capabilities := fetchAgentCapabilities(t)

	root := agentContractGateRepoRoot(t)
	docPath := filepath.Join(root, "skills/timingdex/references/api-contract.md")
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	content := string(raw)

	docAllowed := proseBacktickList(t, content, "`allowed_actions` currently contains")
	if onlyCode, onlyDoc := diffStringSets(capabilities.AllowedActions, docAllowed); len(onlyCode) > 0 || len(onlyDoc) > 0 {
		t.Errorf("allowed_actions differ between GET /api/v1/agent/capabilities and api-contract.md: only in code=%v only in doc=%v", onlyCode, onlyDoc)
	}

	deniedSet := make(map[string]bool, len(capabilities.DeniedActions))
	for _, v := range capabilities.DeniedActions {
		deniedSet[v] = true
	}
	docDenied := proseBacktickList(t, content, "`denied_actions` must include")
	var missing []string
	for _, v := range docDenied {
		if !deniedSet[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("api-contract.md claims denied_actions must include %v, but GET /api/v1/agent/capabilities denied_actions=%v does not contain them", missing, capabilities.DeniedActions)
	}

	if capabilities.ApprovalMode != "human_required" {
		t.Errorf("GET /api/v1/agent/capabilities approval_mode = %q, want human_required", capabilities.ApprovalMode)
	}
	if docApproval := apiContractApprovalMode(t, content); docApproval != "human_required" {
		t.Errorf("api-contract.md declares approval_mode %q, want human_required", docApproval)
	}
}
