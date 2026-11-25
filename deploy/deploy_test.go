package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the deployment artifacts that no compiler or unit test
// touches: the systemd units, the Dockerfile stage layout, the opt-in
// inference Compose override and the release packaging. They fail when a
// hardening property or a packaging contract is dropped during an edit, the
// same way internal/api/page_scripts_test.go pins page scripts.

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readRepoFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// mustContain fails when any needle is missing from content.
func mustContain(t *testing.T, what string, content string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(content, needle) {
			t.Errorf("%s is missing %q", what, needle)
		}
	}
}

// fromStages extracts every `FROM <image> AS <name>` stage name in order.
var fromStageRe = regexp.MustCompile(`(?mi)^FROM\s+\S+\s+AS\s+([A-Za-z0-9._-]+)`)

func TestHubUnitHardening(t *testing.T) {
	root := repoRoot(t)
	unit := readRepoFile(t, root, "deploy/systemd/nexusgate-hub.service")
	mustContain(t, "hub unit", unit,
		"StateDirectory=nexusgate",
		"StateDirectoryMode=0700",
		"User=nexusgate\nGroup=nexusgate",
		"ProtectSystem=strict",
		"UMask=0077",
	)
	// The Hub keeps its read-only inputs; nothing about the Worker's
	// legacy token file belongs here.
	if strings.Contains(unit, "worker.token") {
		t.Error("hub unit must not reference the worker token file")
	}
}

func TestWorkerUnitHardening(t *testing.T) {
	root := repoRoot(t)
	unit := readRepoFile(t, root, "deploy/systemd/nexusgate-worker.service")
	mustContain(t, "worker unit", unit,
		"User=nexusgate-worker\nGroup=nexusgate-worker",
		"StateDirectory=nexusgate-worker",
		"StateDirectoryMode=0700",
		"RuntimeDirectoryMode=0700",
		"InaccessiblePaths=-/var/lib/nexusgate",
		"ProtectSystem=strict",
		"UMask=0077",
	)
	// The two required read-only inputs stay required, but the optional
	// legacy token file must not make a documented worker.json install fail
	// to start: its entry is either gone or dash-prefixed.
	readonly, ok := directiveValue(unit, "ReadOnlyPaths")
	if !ok {
		t.Fatal("worker unit lost ReadOnlyPaths")
	}
	for _, entry := range strings.Fields(readonly) {
		path := strings.TrimPrefix(entry, "-")
		if path != "/srv/nexusgate/media" && path != "/etc/nexusgate/tls" {
			t.Errorf("worker unit ReadOnlyPaths has unexpected entry %q", entry)
		}
	}
	if strings.Contains(unit, "WORKER_TOKEN_FILE") {
		t.Error("worker unit sets the unused NEXUSGATE_WORKER_TOKEN_FILE environment variable")
	}
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/nexusgate worker run --config /var/lib/nexusgate-worker/worker.json") {
		t.Error("worker unit ExecStart changed")
	}
}

func directiveValue(unit, directive string) (string, bool) {
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if prefix := directive + "="; strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix)), true
		}
	}
	return "", false
}

func TestDockerfileStageLayout(t *testing.T) {
	root := repoRoot(t)
	dockerfile := readRepoFile(t, root, "Dockerfile")

	stages := fromStageRe.FindAllStringSubmatch(dockerfile, -1)
	if len(stages) == 0 {
		t.Fatal("Dockerfile has no named stages")
	}
	names := make([]string, len(stages))
	for i, stage := range stages {
		names[i] = stage[1]
	}
	// The opt-in stages sit before the final alias so an untargeted build
	// (the last stage) stays the plain CPU image.
	want := []string{"build", "runtime", "gpu", "inference-venv", "inference", "default"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Dockerfile stage order is %v, want %v", names, want)
	}
	if last := names[len(names)-1]; last != "default" {
		t.Fatalf("last Dockerfile stage is %q; the untargeted build must stay the lean image", last)
	}

	mustContain(t, "Dockerfile", dockerfile,
		"FROM python:3.12-slim-bookworm AS inference-venv",
		"--extra-index-url https://download.pytorch.org/whl/cpu",
		"-r /tmp/torch.lock",
		"COPY --from=inference-venv /opt/nexusgate-venv /opt/nexusgate-venv",
		"COPY --from=inference-venv /usr/local/bin/python3.12 /usr/local/bin/python3.12",
		"COPY --from=inference-venv /usr/local/lib/libpython3.12.so.1.0 /usr/local/lib/libpython3.12.so.1.0",
		"COPY inference/ /opt/nexusgate/inference/",
		"USER nexusgate:nexusgate",
	)
	// The final inference stage user must come after the module copy (the
	// stage must end unprivileged), and inference must stay an opt-in
	// configuration: the Dockerfile itself never enables it.
	moduleCopy := strings.Index(dockerfile, "COPY inference/ /opt/nexusgate/inference/")
	userSwitch := strings.LastIndex(dockerfile, "USER nexusgate:nexusgate")
	if moduleCopy < 0 || userSwitch < moduleCopy {
		t.Error("inference stage must copy the module before switching back to the nexusgate user")
	}
	if strings.Contains(dockerfile, "NEXUSGATE_INFERENCE_ENABLED") {
		t.Error("Dockerfile must not enable inference by default; the Compose override owns that")
	}
	// Nothing may follow the default alias: it is byte-identical to runtime
	// by construction.
	if after := dockerfile[strings.LastIndex(dockerfile, "FROM runtime AS default")+len("FROM runtime AS default"):]; strings.TrimSpace(after) != "" {
		t.Errorf("stage default must stay last, found trailing content: %q", strings.TrimSpace(after))
	}

	// The build context is pruned so `COPY . .` and the module copy never
	// pull venvs, weights or caches into a layer.
	dockerignore := readRepoFile(t, root, ".dockerignore")
	mustContain(t, ".dockerignore", dockerignore,
		"inference/.venv*",
		"inference/models",
		"**/__pycache__",
	)
}

func TestComposeInferenceOverride(t *testing.T) {
	root := repoRoot(t)
	override := readRepoFile(t, root, "deploy/docker-compose.inference.yml")
	mustContain(t, "inference compose override", override,
		"image: nexusgate:inference",
		"target: inference",
		`NEXUSGATE_INFERENCE_ENABLED: "true"`,
		"NEXUSGATE_INFERENCE_PYTHON: /opt/nexusgate-venv/bin/python",
		"NEXUSGATE_INFERENCE_MODULE_DIR: /opt/nexusgate",
		"NEXUSGATE_INFERENCE_DEVICE: cpu",
		"--inference-python",
		"--inference-module",
		"--inference-device",
		"--inference-models",
		"/var/lib/nexusgate-worker/models",
	)
	if got := strings.Count(override, "image: nexusgate:inference"); got != 2 {
		t.Errorf("override must switch both hub and worker to nexusgate:inference, found %d image lines", got)
	}

	// The default file keeps its lean image and its service commands.
	base := readRepoFile(t, root, "docker-compose.yml")
	if strings.Contains(base, "nexusgate:inference") {
		t.Error("default compose file must not use the inference image")
	}
	if strings.Contains(base, "--inference-") {
		t.Error("default compose worker command must not enable inference")
	}
	mustContain(t, "default compose file", base,
		`command: ["worker", "run", "--config", "/var/lib/nexusgate-worker/worker.json"]`,
		"nexusgate-data:",
		"nexusgate-worker-cache:",
	)
	if strings.Count(base, "nexusgate-worker-cache:") < 2 {
		t.Error("worker volume must stay separate from the hub volume")
	}
}

func TestInferenceReleasePackaging(t *testing.T) {
	root := repoRoot(t)

	// Both release entry points package the module.
	workflow := readRepoFile(t, root, ".github/workflows/release.yml")
	mustContain(t, "release workflow", workflow, "scripts/package-inference.sh dist")
	script := readRepoFile(t, root, "scripts/build-release.sh")
	mustContain(t, "build-release.sh", script, "package-inference.sh")
	packaging := readRepoFile(t, root, "scripts/package-inference.sh")
	mustContain(t, "package-inference.sh", packaging,
		"--exclude='inference/.venv'",
		"--exclude='inference/models'",
		"--exclude='__pycache__'",
		"--exclude='*.pyc'",
		"nexusgate-inference.tar.gz",
	)

	// And the artifact itself carries the module, the lock and the pinned
	// requirements, without venvs, weights or caches.
	out := t.TempDir()
	run := exec.Command("bash", filepath.Join(root, "scripts/package-inference.sh"), out)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("package-inference.sh: %v\n%s", err, output)
	}
	tarball := filepath.Join(out, "nexusgate-inference.tar.gz")
	listCmd := exec.Command("tar", "-tzf", tarball)
	listing, err := listCmd.Output()
	if err != nil {
		t.Fatalf("tar -tzf: %v\n%s", err, listing)
	}
	members := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
		members[strings.TrimSpace(line)] = true
	}
	for _, want := range []string{
		"inference/dispatch.py",
		"inference/models.lock.json",
		"inference/requirements/torch.lock",
		"inference/adapters/__init__.py",
	} {
		if !members[want] {
			t.Errorf("inference package is missing %s", want)
		}
	}
	unwanted := regexp.MustCompile(`(^|/)(__pycache__|\.venv[^/]*|\.cache|models)(/|$)|\.pyc$`)
	for member := range members {
		if unwanted.MatchString(member) {
			t.Errorf("inference package contains runtime artifact %s", member)
		}
	}
}

func TestInferenceDocsUseGeneratedCertificateTrust(t *testing.T) {
	root := repoRoot(t)
	doc := readRepoFile(t, root, "docs/v1.5/inference.md")
	if strings.Contains(doc, "http://127.0.0.1:8787") {
		t.Error("docs/v1.5/inference.md must use HTTPS, not cleartext HTTP, for Hub calls")
	}
	mustContain(t, "docs/v1.5/inference.md", doc,
		"--cacert",
		"hub-cert.pem",
		"https://127.0.0.1:8787",
	)
	// Mutations (install POST, settings PUT) are included in the HTTPS rule.
	if strings.Count(doc, "https://127.0.0.1:8787") < 3 {
		t.Error("docs/v1.5/inference.md should keep every curl, read and mutation, on HTTPS")
	}
	upgrade := readRepoFile(t, root, "docs/v0.14-deployment.md")
	mustContain(t, "docs/v0.14-deployment.md", upgrade,
		"nexusgate-worker",
		"InaccessiblePaths",
		"Upgrading an existing single-account install",
		"sudo -u nexusgate-worker /usr/local/bin/nexusgate worker enroll",
	)
}
