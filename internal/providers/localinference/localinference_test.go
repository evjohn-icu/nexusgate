//go:build linux

package localinference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

const testOperationID = "op-1"

func TestExecuteHappyMultiFrameAndSanitizedEnvironment(t *testing.T) {
	request := testRequest()
	first := resultPart(request, []domain.SoundEvent{testEvent(0, 400)}, "fake")
	second := map[string]any{"events": []domain.SoundEvent{testEvent(400, 1000)}}
	frames := resultFrames(t, request.OperationID, []json.RawMessage{marshalJSON(t, first), marshalJSON(t, second)})
	script := readOnceScript(t, "if [ \"${PARENT_SECRET+x}\" = x ]; then printf '%s\\n' '{\"type\":\"error\",\"operation_id\":\"op-1\",\"code\":\"invalid_output\",\"message\":\"parent environment leaked\"}'; exit 0; fi\n"+frames)
	t.Setenv("PARENT_SECRET", "host-token-should-not-reach-child")
	executor, scratch := testExecutor(t, script, time.Second, nil)
	got, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(got.Events) != 2 || got.Events[0].StartMS != 0 || got.Events[1].StartMS != 400 {
		t.Fatalf("multi-frame events not concatenated in order: %+v", got.Events)
	}
	assertScratchEmpty(t, scratch)
}

func TestExecuteSerializesDeviceUse(t *testing.T) {
	request := testRequest()
	frames := resultFrames(t, request.OperationID, []json.RawMessage{marshalJSON(t, resultPart(request, []domain.SoundEvent{testEvent(0, 1000)}, "fake"))})
	activeDir := filepath.Join(t.TempDir(), "active")
	overlapFile := filepath.Join(t.TempDir(), "overlap")
	script := readOnceScript(t, "if /bin/mkdir \"$ACTIVE_DIR\" 2>/dev/null; then /bin/sleep 0.5; /bin/rmdir \"$ACTIVE_DIR\"; else : > \"$OVERLAP_FILE\"; fi\n"+frames)
	executor, scratch := testExecutor(t, script, 3*time.Second, []string{"ACTIVE_DIR=" + activeDir, "OVERLAP_FILE=" + overlapFile})
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := executor.Execute(context.Background(), request)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Execute() error = %v", err)
		}
	}
	if _, err := os.Stat(overlapFile); !os.IsNotExist(err) {
		t.Fatalf("subprocesses overlapped; marker stat error = %v", err)
	}
	assertScratchEmpty(t, scratch)
}

func TestExecuteRejectsDigestMismatch(t *testing.T) {
	request := testRequest()
	payload := marshalJSON(t, resultPart(request, []domain.SoundEvent{testEvent(0, 1000)}, "fake"))
	frame := resultFrames(t, request.OperationID, []json.RawMessage{payload})
	encodedFrame := []byte(frame)
	prefix := `"digest":"sha256:`
	digestStart := strings.Index(frame, prefix) + len(prefix)
	if digestStart < len(prefix) {
		t.Fatal("frame has no digest")
	}
	if encodedFrame[digestStart] == '0' {
		encodedFrame[digestStart] = '1'
	} else {
		encodedFrame[digestStart] = '0'
	}
	frame = string(encodedFrame)
	executor, scratch := testExecutor(t, readOnceScript(t, frame), time.Second, nil)
	_, err := executor.Execute(context.Background(), request)
	assertFailure(t, err, remote.FailureInvalidOutput)
	assertScratchEmpty(t, scratch)
}

func TestExecuteRejectsSequenceGap(t *testing.T) {
	request := testRequest()
	first := resultPart(request, []domain.SoundEvent{testEvent(0, 400)}, "fake")
	second := map[string]any{"events": []domain.SoundEvent{testEvent(400, 1000)}}
	digest := "sha256:" + strings.Repeat("ab", 32)
	frames := []map[string]any{
		{"type": "result", "operation_id": request.OperationID, "sequence": 0, "count": 2, "final": false, "digest": digest, "result": first},
		// Sequence 2 with count 2 is the gap: the second frame never arrives.
		{"type": "result", "operation_id": request.OperationID, "sequence": 2, "count": 2, "final": true, "digest": digest, "result": second},
	}
	executor, scratch := testExecutor(t, readOnceScript(t, frameScript(t, frames)), time.Second, nil)
	_, err := executor.Execute(context.Background(), request)
	assertFailure(t, err, remote.FailureInvalidOutput)
	assertScratchEmpty(t, scratch)
}

func TestExecuteErrorFrameWinsOverExitStatus(t *testing.T) {
	request := testRequest()
	script := readOnceScript(t, "printf '%s\\n' '{\"type\":\"error\",\"operation_id\":\"op-1\",\"code\":\"unsupported_input\",\"message\":\"unsupported fixture\",\"retryable\":false}'\nexit 17\n")
	executor, scratch := testExecutor(t, script, time.Second, nil)
	_, err := executor.Execute(context.Background(), request)
	assertFailure(t, err, remote.FailureUnsupportedInput)
	assertScratchEmpty(t, scratch)
}

func TestExecuteDeadlineKillsProcessTree(t *testing.T) {
	request := testRequest()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s\\n' \"$!\" > \"$CHILD_PID_FILE\"\nIFS= read -r request || exit 0\nwait\n"
	executor, scratch := testExecutor(t, script, 150*time.Millisecond, []string{"CHILD_PID_FILE=" + pidFile})
	_, err := executor.Execute(context.Background(), request)
	assertFailure(t, err, remote.FailureDeadlineExceeded)
	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("sleeping child did not record its pid: %v", readErr)
	}
	var pid int
	if _, scanErr := fmt.Sscan(string(pidBytes), &pid); scanErr != nil || pid <= 0 {
		t.Fatalf("invalid child pid %q: %v", pidBytes, scanErr)
	}
	waitProcessGone(t, pid)
	assertScratchEmpty(t, scratch)
}

func TestExecuteContextCancellationKillsProcess(t *testing.T) {
	request := testRequest()
	startFile := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\nIFS= read -r request || exit 0\n: > \"$START_FILE\"\n/bin/sleep 30\n"
	executor, scratch := testExecutor(t, script, 5*time.Second, []string{"START_FILE=" + startFile})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, request)
		resultCh <- err
	}()
	waitForFile(t, startFile)
	cancel()
	select {
	case err := <-resultCh:
		assertFailure(t, err, remote.FailureCancelled)
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not return after context cancellation")
	}
	assertScratchEmpty(t, scratch)
}

func TestExecuteRedactsStderrTokensAndOutsidePaths(t *testing.T) {
	request := testRequest()
	script := readOnceScript(t, "printf '%s\\n' 'Traceback (most recent call last): File \"/home/private/model.py\" token hf_abcdefgh1234567890 Bearer abcdefghijklmnopqrstuvwxyz1234567890' >&2\nexit 2\n")
	executor, _ := testExecutor(t, script, time.Second, nil)
	_, err := executor.Execute(context.Background(), request)
	assertFailure(t, err, remote.FailureInvalidOutput)
	message := err.Error()
	for _, secret := range []string{"/home/private/model.py", "hf_abcdefgh1234567890", "abcdefghijklmnopqrstuvwxyz1234567890"} {
		if strings.Contains(message, secret) {
			t.Errorf("failure message leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "[PATH]") || !strings.Contains(message, "[REDACTED]") {
		t.Errorf("stderr was not visibly redacted: %s", message)
	}
}

func TestCapabilitiesCachesProbe(t *testing.T) {
	probeCount := filepath.Join(t.TempDir(), "probe-count")
	capability := remote.InferenceCapability{
		Operation: domain.OpSoundEvents, SchemaVersion: domain.InferenceSchemaVersion,
		ModelID: "fake-model", ModelRevision: "rev-1", ConfigHash: "cfg-1",
		Backend: "fake", Available: true,
	}
	frame := marshalJSON(t, map[string]any{"type": "capabilities", "capabilities": []remote.InferenceCapability{capability}})
	script := "#!/bin/sh\nif [ \"$9\" = \"--probe\" ]; then printf x >> \"$PROBE_COUNT_FILE\"; printf '%s\\n' '" + string(frame) + "'; exit 0; fi\nexit 9\n"
	executor, scratch := testExecutor(t, script, time.Second, []string{"PROBE_COUNT_FILE=" + probeCount})
	first, err := executor.Capabilities(context.Background())
	if err != nil || len(first) != 1 || first[0] != capability {
		t.Fatalf("first Capabilities() = %+v, %v", first, err)
	}
	second, err := executor.Capabilities(context.Background())
	if err != nil || len(second) != 1 || second[0] != capability {
		t.Fatalf("cached Capabilities() = %+v, %v", second, err)
	}
	count, err := os.ReadFile(probeCount)
	if err != nil || string(count) != "x" {
		t.Fatalf("probe ran more than once: count=%q err=%v", count, err)
	}
	assertScratchEmpty(t, scratch)
}

func TestExecuteRemovesScratchOnFailure(t *testing.T) {
	request := testRequest()
	executor, scratch := testExecutor(t, readOnceScript(t, "printf '%s\\n' '{\"type\":\"unknown\"}'\n"), time.Second, nil)
	_, err := executor.Execute(context.Background(), request)
	assertFailure(t, err, remote.FailureInvalidOutput)
	assertScratchEmpty(t, scratch)
}

func TestWireRequestResolvesAssetAndOmitsCallerPaths(t *testing.T) {
	executor, _ := testExecutor(t, "#!/bin/sh\nexit 0\n", time.Second, nil)
	request := testRequest()
	request.Input.RootID = "root-1"
	request.Input.RelativePath = "nested/clip.mp4"
	request.Input.Interval = &domain.TimeInterval{StartMS: 100, EndMS: 900}
	encoded, err := executor.wireRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["space_id"]) != `""` {
		t.Fatalf("non-vector request invented a space id: %s", wire["space_id"])
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(wire["input"], &input); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"asset_id", "root_id", "relative_path", "reference_id"} {
		if _, exists := input[forbidden]; exists {
			t.Errorf("wire asset input contains forbidden %q", forbidden)
		}
	}
	if len(input) != 4 || input["kind"] == nil || input["path"] == nil || input["duration_ms"] == nil || input["interval"] == nil {
		t.Fatalf("wire asset input has unexpected shape: %+v", input)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(wire["settings"], &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1 || settings["sound_events"] == nil {
		t.Fatalf("wire settings have unexpected shape: %+v", settings)
	}
}

func TestExecuteWithoutAssetResolverReturnsUnsupportedInput(t *testing.T) {
	executor, scratch := testExecutor(t, "#!/bin/sh\nexit 0\n", time.Second, nil)
	executor.cfg.ResolveAssetPath = nil
	_, err := executor.Execute(context.Background(), testRequest())
	assertFailure(t, err, remote.FailureUnsupportedInput)
	assertScratchEmpty(t, scratch)
}

func TestWireRequestResolvesImageReference(t *testing.T) {
	executor, _ := testExecutor(t, "#!/bin/sh\nexit 0\n", time.Second, nil)
	imagePath := filepath.Join(t.TempDir(), "reference.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor.cfg.ResolveImagePath = func(referenceID, digest string) (string, error) {
		if referenceID != "ref-1" || digest != "sha256:abc" {
			t.Fatalf("unexpected image resolver identity %q %q", referenceID, digest)
		}
		return imagePath, nil
	}
	request := testRequest()
	request.Operation = domain.OpVisualEmbedding
	request.SpaceID = "space-1"
	request.SoundEvent = nil
	request.Visual = &domain.VisualSettings{}
	request.Input = domain.InferenceInput{Kind: domain.InputKindImage, ReferenceID: "ref-1", Digest: "sha256:abc"}
	encoded, err := executor.wireRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(wire["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 3 || input["path"] == nil || input["digest"] == nil || input["reference_id"] != nil {
		t.Fatalf("wire image input has unexpected shape: %+v", input)
	}
}

func testExecutor(t *testing.T, script string, timeout time.Duration, extraEnv []string) (*Executor, string) {
	t.Helper()
	base := t.TempDir()
	python := filepath.Join(base, "fake-python")
	if err := os.WriteFile(python, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(python, 0o700); err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Join(base, "module")
	modelDir := filepath.Join(base, "models")
	scratch := filepath.Join(base, "scratch")
	for _, dir := range []string{moduleDir, modelDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(base, "media.bin")
	if err := os.WriteFile(source, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		PythonPath: python, ModuleDir: moduleDir, ModelDir: modelDir,
		Device: "cpu", Dtype: "float32", ScratchDir: scratch,
		Timeout: timeout, Env: extraEnv,
		ResolveAssetPath: func(domain.InferenceInput) (string, error) { return source, nil },
	}
	executor, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return executor, scratch
}

func testRequest() domain.InferenceRequest {
	return domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion,
		OperationID:   testOperationID, Operation: domain.OpSoundEvents,
		ModelID: "fake-model", ModelRevision: "rev-1", InputHash: "input-1",
		ConfigHash: "cfg-1", TimeoutMS: 2000,
		Input:      domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "asset-1", DurationMS: 1000},
		SoundEvent: &domain.SoundEventSettings{},
	}
}

func testEvent(start, end int64) domain.SoundEvent {
	return domain.SoundEvent{Label: "rain", StartMS: start, EndMS: end, RawScore: .8, EvidenceStatus: "possible", BoundaryResolutionMS: 100}
}

func resultPart(request domain.InferenceRequest, events []domain.SoundEvent, backend string) map[string]any {
	return map[string]any{
		"schema_version": request.SchemaVersion, "operation_id": request.OperationID,
		"operation": request.Operation, "model_id": request.ModelID,
		"model_revision": request.ModelRevision, "input_hash": request.InputHash,
		"config_hash": request.ConfigHash, "backend": backend,
		"events": events,
	}
}

func resultFrames(t *testing.T, operationID string, results []json.RawMessage) string {
	t.Helper()
	var joined []byte
	for _, result := range results {
		joined = append(joined, result...)
	}
	digestBytes := sha256.Sum256(joined)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	var output strings.Builder
	for sequence, result := range results {
		frame := struct {
			Type        string          `json:"type"`
			OperationID string          `json:"operation_id"`
			Sequence    int             `json:"sequence"`
			Count       int             `json:"count"`
			Final       bool            `json:"final"`
			Digest      string          `json:"digest"`
			Result      json.RawMessage `json:"result"`
		}{"result", operationID, sequence, len(results), sequence == len(results)-1, digest, result}
		line := marshalJSON(t, frame)
		// The fake interpreter is a shell script with a sanitized PATH, so each
		// frame is emitted with the shell builtin and single-quoted as data —
		// not pasted as a command line and not via an external binary.
		quoted := strings.ReplaceAll(string(line), "'", "'\\''")
		output.WriteString("printf '%s\\n' '" + quoted + "'\n")
	}
	return output.String()
}

// frameScript renders frame maps as shell printf commands for the fake
// interpreter. It uses the shell builtin because the child environment is
// sanitized and has no external binaries on PATH.
func frameScript(t *testing.T, frames []map[string]any) string {
	t.Helper()
	var output strings.Builder
	for _, frame := range frames {
		line := string(marshalJSON(t, frame))
		quoted := strings.ReplaceAll(line, "'", "'\\''")
		output.WriteString("printf '%s\\n' '" + quoted + "'\n")
	}
	return output.String()
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func readOnceScript(t *testing.T, output string) string {
	t.Helper()
	var script strings.Builder
	script.WriteString("#!/bin/sh\nIFS= read -r request || exit 0\n")
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			script.WriteString("printf '%s\\n' ")
			script.WriteString(shellQuote(strings.TrimSpace(line)))
		} else {
			script.WriteString(line)
		}
		script.WriteByte('\n')
	}
	return script.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func assertFailure(t *testing.T, err error, code remote.InferenceFailureCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s failure, got nil", code)
	}
	var failure *remote.InferenceFailure
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error = %T %v, want InferenceFailure(%s)", err, err, code)
	}
}

func assertScratchEmpty(t *testing.T, scratch string) {
	t.Helper()
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("operation scratch remains: %v", entries)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q was not created", path)
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if stat, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); statErr == nil {
			if end := strings.LastIndex(string(stat), ")"); end >= 0 {
				fields := strings.Fields(string(stat[end+1:]))
				if len(fields) > 0 && fields[0] == "Z" {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived process-tree termination", pid)
}

func TestWireRequestSendsSpeakerSegmentsNotProfileIdentity(t *testing.T) {
	executor, _ := testExecutor(t, "#!/bin/sh\nexit 0\n", time.Second, nil)
	request := testRequest()
	request.Operation = domain.OpSpeakerEmbedding
	request.SoundEvent = nil
	request.Speaker = &domain.SpeakerSettings{
		ProfileID: "profile-secret",
		Interval:  &domain.TimeInterval{StartMS: 1000, EndMS: 4500},
	}
	encoded, err := executor.wireRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Settings map[string]struct {
			Segments []map[string]int64 `json:"segments"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	segments := wire.Settings["speaker"].Segments
	if len(segments) != 1 || segments[0]["start_ms"] != 1000 || segments[0]["end_ms"] != 4500 {
		t.Fatalf("speaker wire settings = %+v", wire.Settings)
	}
	if strings.Contains(string(encoded), "profile-secret") {
		t.Fatal("profile identity must not cross the model process boundary")
	}
}
