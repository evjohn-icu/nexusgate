//go:build linux

package localinference

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

// jobContextKey is the test stand-in for the per-job context value a lease-bound
// resolver publishes. Execute must forward the caller's context to the image
// resolver unchanged.
type jobContextKey struct{}

func probeCapability(available bool, code remote.InferenceFailureCode) remote.InferenceCapability {
	capability := remote.InferenceCapability{
		Operation: domain.OpVisualEmbedding, SchemaVersion: domain.InferenceSchemaVersion,
		ModelID: "fake-model", ModelRevision: "rev-1", ConfigHash: "cfg-1",
		Backend: "fake", Available: available,
	}
	if !available {
		capability.UnavailableCode = code
	}
	return capability
}

func probeFrame(t *testing.T, capability remote.InferenceCapability) string {
	t.Helper()
	line := string(marshalJSON(t, map[string]any{
		"type": "capabilities", "capabilities": []remote.InferenceCapability{capability},
	}))
	return shellQuote(line)
}

// probeScript records the dispatch arguments and branches between the probe
// frame and the ordinary execute result stream. POSIX sh only expands
// single-digit positionals directly, so two-digit arguments need braces.
func probeScript(t *testing.T, capability remote.InferenceCapability, request *domain.InferenceRequest) string {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$9|${10}|${11}|${12}\" > \"$ARGS_FILE\"\n"
	script += "if [ \"$9\" = \"--probe-model\" ]; then printf '%s\\n' " + probeFrame(t, capability) + "\nexit 0\nfi\n"
	if request != nil {
		script += "IFS= read -r request || exit 0\n"
		script += resultFrames(t, request.OperationID, []json.RawMessage{
			marshalJSON(t, resultPart(*request, []domain.SoundEvent{testEvent(0, 1000)}, "fake")),
		})
	}
	return script
}

func TestProbeModelSucceedsOnExactAvailableIdentity(t *testing.T) {
	request := testRequest()
	argsFile := filepath.Join(t.TempDir(), "args")
	executor, scratch := testExecutor(t, probeScript(t, probeCapability(true, ""), &request), time.Second, []string{"ARGS_FILE=" + argsFile})
	if err := executor.ProbeModel(context.Background(), "fake-model", "rev-1"); err != nil {
		t.Fatalf("ProbeModel() error = %v", err)
	}
	assertArgs(t, argsFile, "--probe-model|fake-model|--probe-revision|rev-1")
	assertScratchEmpty(t, scratch)
}

func TestProbeModelReportsTypedUnavailableCode(t *testing.T) {
	executor, scratch := testExecutor(t, probeScript(t, probeCapability(false, remote.FailureUnsupportedInput), nil), time.Second, nil)
	err := executor.ProbeModel(context.Background(), "fake-model", "rev-1")
	assertFailure(t, err, remote.FailureUnsupportedInput)
	assertScratchEmpty(t, scratch)
}

func TestProbeModelRejectsUnknownIdentity(t *testing.T) {
	executor, scratch := testExecutor(t, probeScript(t, probeCapability(true, ""), nil), time.Second, nil)
	err := executor.ProbeModel(context.Background(), "unknown-model", "rev-1")
	assertFailure(t, err, remote.FailureModelUnavailable)
	assertScratchEmpty(t, scratch)
}

func TestProbeModelRejectsWrongRevision(t *testing.T) {
	executor, scratch := testExecutor(t, probeScript(t, probeCapability(true, ""), nil), time.Second, nil)
	err := executor.ProbeModel(context.Background(), "fake-model", "rev-2")
	assertFailure(t, err, remote.FailureModelUnavailable)
	assertScratchEmpty(t, scratch)
}

func TestProbeModelRejectsInvalidReport(t *testing.T) {
	executor, scratch := testExecutor(t, "#!/bin/sh\nprintf '%s\\n' 'not json'\n", time.Second, nil)
	err := executor.ProbeModel(context.Background(), "fake-model", "rev-1")
	assertFailure(t, err, remote.FailureModelUnavailable)
	assertScratchEmpty(t, scratch)
}

func TestProbeModelRejectsEmptyIdentity(t *testing.T) {
	executor, scratch := testExecutor(t, probeScript(t, probeCapability(true, ""), nil), time.Second, nil)
	for _, call := range []struct{ modelID, revision string }{
		{"", "rev-1"}, {"fake-model", ""}, {"  ", "rev-1"},
	} {
		err := executor.ProbeModel(context.Background(), call.modelID, call.revision)
		assertFailure(t, err, remote.FailureUnsupportedInput)
	}
	assertScratchEmpty(t, scratch)
}

func TestProbeModelDeadlineReturnsDeadlineExceeded(t *testing.T) {
	executor, scratch := testExecutor(t, "#!/bin/sh\n/bin/sleep 30\n", 150*time.Millisecond, nil)
	err := executor.ProbeModel(context.Background(), "fake-model", "rev-1")
	assertFailure(t, err, remote.FailureDeadlineExceeded)
	assertScratchEmpty(t, scratch)
}

func TestProbeModelCancellationReturnsCancelled(t *testing.T) {
	startFile := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\n: > \"$START_FILE\"\n/bin/sleep 30\n"
	executor, scratch := testExecutor(t, script, 5*time.Second, []string{"START_FILE=" + startFile})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- executor.ProbeModel(ctx, "fake-model", "rev-1") }()
	waitForFile(t, startFile)
	cancel()
	select {
	case err := <-errCh:
		assertFailure(t, err, remote.FailureCancelled)
	case <-time.After(3 * time.Second):
		t.Fatal("ProbeModel did not return after context cancellation")
	}
	assertScratchEmpty(t, scratch)
}

// TestProbeModelSerializesDeviceUse pins the contract that a load-only probe
// occupies the same device slot as an executing job: neither may overlap.
func TestProbeModelSerializesDeviceUse(t *testing.T) {
	request := testRequest()
	execActive := filepath.Join(t.TempDir(), "exec-active")
	execOverlap := filepath.Join(t.TempDir(), "exec-overlap")
	probeActive := filepath.Join(t.TempDir(), "probe-active")
	probeOverlap := filepath.Join(t.TempDir(), "probe-overlap")
	env := []string{
		"EXEC_ACTIVE_DIR=" + execActive, "EXEC_OVERLAP_FILE=" + execOverlap,
		"PROBE_ACTIVE_DIR=" + probeActive, "PROBE_OVERLAP_FILE=" + probeOverlap,
	}
	script := "#!/bin/sh\n" +
		"if [ \"$9\" = \"--probe-model\" ]; then\n" +
		"  if /bin/mkdir \"$PROBE_ACTIVE_DIR\" 2>/dev/null; then /bin/sleep 0.3; /bin/rmdir \"$PROBE_ACTIVE_DIR\"; " +
		"printf '%s\\n' " + probeFrame(t, probeCapability(true, "")) + "; exit 0; else : > \"$PROBE_OVERLAP_FILE\"; exit 9; fi\n" +
		"fi\n" +
		"IFS= read -r request || exit 0\n" +
		"if /bin/mkdir \"$EXEC_ACTIVE_DIR\" 2>/dev/null; then /bin/sleep 0.3; /bin/rmdir \"$EXEC_ACTIVE_DIR\"; else : > \"$EXEC_OVERLAP_FILE\"; fi\n" +
		resultFrames(t, request.OperationID, []json.RawMessage{
			marshalJSON(t, resultPart(request, []domain.SoundEvent{testEvent(0, 1000)}, "fake")),
		})
	executor, scratch := testExecutor(t, script, 3*time.Second, env)
	results := make(chan error, 2)
	go func() { _, err := executor.Execute(context.Background(), request); results <- err }()
	go func() { results <- executor.ProbeModel(context.Background(), "fake-model", "rev-1") }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Execute/ProbeModel error = %v", err)
		}
	}
	for _, marker := range []string{execOverlap, probeOverlap} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("Execute and ProbeModel overlapped; marker stat error = %v", err)
		}
	}
	assertScratchEmpty(t, scratch)
}

func TestInvalidateCapabilitiesForcesFreshProbe(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	capability := probeCapability(true, "")
	frame := marshalJSON(t, map[string]any{"type": "capabilities", "capabilities": []remote.InferenceCapability{capability}})
	script := "#!/bin/sh\nif [ \"$9\" = \"--probe\" ]; then printf x >> \"$PROBE_COUNT_FILE\"; printf '%s\\n' '" + string(frame) + "'; exit 0; fi\nexit 9\n"
	executor, scratch := testExecutor(t, script, time.Second, []string{"PROBE_COUNT_FILE=" + probeCount})
	if _, err := executor.Capabilities(context.Background()); err != nil {
		t.Fatalf("first Capabilities() error = %v", err)
	}
	executor.InvalidateCapabilities()
	if _, err := executor.Capabilities(context.Background()); err != nil {
		t.Fatalf("post-invalidation Capabilities() error = %v", err)
	}
	count, err := os.ReadFile(probeCount)
	if err != nil || string(count) != "xx" {
		t.Fatalf("invalidation must force a fresh probe: count=%q err=%v", count, err)
	}
	assertScratchEmpty(t, scratch)
}

func TestNewCreatesMissingModelDirectory(t *testing.T) {
	base := t.TempDir()
	cfg := Config{
		PythonPath: writeFakePython(t, base, "#!/bin/sh\nexit 0\n"),
		ModuleDir:  mustDir(t, base, "module"),
		ModelDir:   filepath.Join(base, "models", "nested"),
		Device:     "cpu", Dtype: "float32",
		ScratchDir: filepath.Join(base, "scratch"),
		Timeout:    time.Second,
	}
	executor, err := New(cfg)
	if err != nil {
		t.Fatalf("New() with a missing model directory error = %v", err)
	}
	info, err := os.Stat(cfg.ModelDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("model directory was not bootstrapped: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("model directory mode = %o, want 700", mode)
	}
	_ = executor
}

func TestNewSecuresExistingModelDirectory(t *testing.T) {
	base := t.TempDir()
	modelDir := mustDir(t, base, "models")
	if err := os.Chmod(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		PythonPath: writeFakePython(t, base, "#!/bin/sh\nexit 0\n"),
		ModuleDir:  mustDir(t, base, "module"), ModelDir: modelDir,
		Device: "cpu", Dtype: "float32",
		ScratchDir: filepath.Join(base, "scratch"), Timeout: time.Second,
	}
	if _, err := New(cfg); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := os.Stat(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("existing model directory mode = %o, want 700", mode)
	}
}

func TestNewRejectsModelDirectoryFile(t *testing.T) {
	base := t.TempDir()
	modelFile := filepath.Join(base, "models")
	if err := os.WriteFile(modelFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		PythonPath: writeFakePython(t, base, "#!/bin/sh\nexit 0\n"),
		ModuleDir:  mustDir(t, base, "module"), ModelDir: modelFile,
		Device: "cpu", Dtype: "float32",
		ScratchDir: filepath.Join(base, "scratch"), Timeout: time.Second,
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New() must reject a model directory that is a regular file")
	}
}

func TestCapabilitiesRecreatesScratchRoot(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	capability := probeCapability(true, "")
	frame := marshalJSON(t, map[string]any{"type": "capabilities", "capabilities": []remote.InferenceCapability{capability}})
	script := "#!/bin/sh\nif [ \"$9\" = \"--probe\" ]; then printf x >> \"$PROBE_COUNT_FILE\"; printf '%s\\n' '" + string(frame) + "'; exit 0; fi\nexit 9\n"
	executor, scratch := testExecutor(t, script, time.Second, []string{"PROBE_COUNT_FILE=" + probeCount})
	if err := os.RemoveAll(scratch); err != nil {
		t.Fatal(err)
	}
	caps, err := executor.Capabilities(context.Background())
	if err != nil || len(caps) != 1 || !caps[0].Available {
		t.Fatalf("Capabilities() after scratch removal = %+v, %v", caps, err)
	}
	info, err := os.Stat(scratch)
	if err != nil || !info.IsDir() {
		t.Fatalf("scratch root was not recreated: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("recreated scratch root mode = %o, want 700", mode)
	}
}

func TestExecutePassesJobContextToImageResolver(t *testing.T) {
	executor, _ := testExecutor(t, "#!/bin/sh\nIFS= read -r request || exit 0\nexit 3\n", time.Second, nil)
	imagePath := filepath.Join(t.TempDir(), "reference.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := make(chan context.Context, 1)
	executor.cfg.ResolveImagePath = func(ctx context.Context, referenceID, digest string) (string, error) {
		seen <- ctx
		return imagePath, nil
	}
	request := testRequest()
	request.Operation = domain.OpVisualEmbedding
	request.SpaceID = "space-1"
	request.SoundEvent = nil
	request.Visual = &domain.VisualSettings{}
	request.Input = domain.InferenceInput{Kind: domain.InputKindImage, ReferenceID: "ref-1", Digest: "sha256:abc"}
	jobCtx := context.WithValue(context.Background(), jobContextKey{}, "job-1")
	if _, err := executor.Execute(jobCtx, request); err == nil {
		t.Fatal("expected the fixture process failure, not success")
	}
	select {
	case got := <-seen:
		if got.Value(jobContextKey{}) != "job-1" {
			t.Fatalf("Execute did not forward the per-job context: %v", got)
		}
	default:
		t.Fatal("Execute never called the image resolver")
	}
}

func assertArgs(t *testing.T, argsFile, want string) {
	t.Helper()
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("dispatch arguments were not recorded: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Fatalf("dispatch arguments = %q, want %q", got, want)
	}
}

func writeFakePython(t *testing.T, base, script string) string {
	t.Helper()
	python := filepath.Join(base, "fake-python")
	if err := os.WriteFile(python, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return python
}

func mustDir(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
