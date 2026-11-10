// Package localinference runs the locked Python inference dispatcher as an
// isolated, short-lived subprocess. It deliberately depends only on the pure
// domain and remote inference contracts.
package localinference

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

const (
	defaultTimeout    = 10 * time.Minute
	defaultFrameBytes = int64(32 << 20)
	maxStderrBytes    = 8 << 10
	maxProgressFrames = 1024
	capabilityTTL     = 30 * time.Second
)

// Config is the fixed runtime identity for one local inference executor.
// Paths must be absolute. Env is an explicit allowlisted-by-caller overlay,
// never a supplement to the parent process environment.
type Config struct {
	PythonPath    string
	ModuleDir     string
	ModelDir      string
	Device        string
	Dtype         string
	ScratchDir    string
	Timeout       time.Duration
	MaxFrameBytes int64
	Env           []string
	// moduleRoot is the derived sys.path entry: either ModuleDir itself (when
	// it contains the `inference` package) or its parent (when ModuleDir is the
	// package directory). It is never caller-controlled.
	moduleRoot string
	// ResolveAssetPath and ResolveImagePath return the already-authorized local
	// staged file. The executor never guesses a path from an asset or reference ID.
	ResolveAssetPath func(input domain.InferenceInput) (string, error)
	ResolveImagePath func(referenceID, digest string) (string, error)
}

// Executor is safe for concurrent calls. Each operation owns a private
// directory and process; capability probes are shared for 30 seconds.
type Executor struct {
	cfg Config

	// runMu serialises device use: one operation at a time, per the configured
	// GPU concurrency of 1, for Hub-local and Worker execution alike.
	runMu sync.Mutex

	probeMu   sync.Mutex
	probeAt   time.Time
	probeCaps []remote.InferenceCapability
}

// New validates and normalizes the runtime configuration, creating the
// private managed scratch root if necessary.
func New(cfg Config) (*Executor, error) {
	// "auto" is a deployment preference, not a device name the dispatcher
	// understands. Resolve it once here: a visible NVIDIA device selects CUDA,
	// anything else stays on CPU. An overridden value is validated below.
	if strings.EqualFold(strings.TrimSpace(cfg.Device), "auto") || strings.TrimSpace(cfg.Device) == "" {
		cfg.Device = resolveAutoDevice()
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Dtype), "auto") || strings.TrimSpace(cfg.Dtype) == "" {
		// FP16 on CUDA is the measured profile default; CPU stays FP32.
		if cfg.Device == "cuda" {
			cfg.Dtype = "float16"
		} else {
			cfg.Dtype = "float32"
		}
	}
	for name, path := range map[string]string{
		"PythonPath": cfg.PythonPath,
		"ModuleDir":  cfg.ModuleDir,
		"ModelDir":   cfg.ModelDir,
		"ScratchDir": cfg.ScratchDir,
	} {
		if path == "" || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if cfg.Device != "cpu" && cfg.Device != "cuda" {
		return nil, fmt.Errorf("device must be cpu or cuda")
	}
	if cfg.Dtype != "float32" && cfg.Dtype != "float16" {
		return nil, fmt.Errorf("dtype must be float32 or float16")
	}
	for _, item := range cfg.Env {
		key, _, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.ContainsAny(key, "\x00=\r\n") || strings.ContainsAny(item, "\x00\r\n") {
			return nil, fmt.Errorf("extra environment entries must be valid KEY=VALUE pairs")
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxFrameBytes <= 0 {
		cfg.MaxFrameBytes = defaultFrameBytes
	}
	for name, path := range map[string]string{"PythonPath": cfg.PythonPath, "ModuleDir": cfg.ModuleDir, "ModelDir": cfg.ModelDir} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("%s is unavailable: %w", name, err)
		}
		if name == "PythonPath" {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("PythonPath must be a regular file")
			}
		} else if !info.IsDir() {
			return nil, fmt.Errorf("%s must be a directory", name)
		}
	}
	// Accept both documented layouts: ModuleDir may be the parent that holds
	// `inference/`, or the package directory itself.
	if _, err := os.Stat(filepath.Join(cfg.ModuleDir, "inference", "__init__.py")); err == nil {
		cfg.moduleRoot = cfg.ModuleDir
	} else {
		cfg.moduleRoot = filepath.Dir(cfg.ModuleDir)
	}
	if err := os.MkdirAll(cfg.ScratchDir, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch root: %w", err)
	}
	if err := os.Chmod(cfg.ScratchDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure scratch root: %w", err)
	}
	return &Executor{cfg: cfg}, nil
}

// Execute runs exactly one inference request and returns only after the full
// framed stream, digest, process exit and domain payload have all validated.
func (e *Executor) Execute(ctx context.Context, req domain.InferenceRequest) (result domain.InferenceResult, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := req.Validate(); err != nil {
		return domain.InferenceResult{}, failure(remote.FailureUnsupportedInput, "invalid inference request")
	}
	if err := ctx.Err(); err != nil {
		return domain.InferenceResult{}, contextFailure(err)
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.InferenceResult{}, contextFailure(err)
	}

	opDir, err := e.newOperationDir()
	if err != nil {
		return domain.InferenceResult{}, failure(remote.FailureResourceExhausted, "cannot create operation directory")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(opDir); cleanupErr != nil && retErr == nil {
			retErr = failure(remote.FailureResourceExhausted, "cannot remove operation directory")
			result = domain.InferenceResult{}
		}
	}()
	requestJSON, err := e.wireRequest(req)
	if err != nil {
		return domain.InferenceResult{}, err
	}
	if int64(len(requestJSON)) > e.cfg.MaxFrameBytes {
		return domain.InferenceResult{}, failure(remote.FailureUnsupportedInput, "inference request exceeds the frame limit")
	}
	requestJSON = append(requestJSON, '\n')

	var limit time.Duration
	if req.TimeoutMS > int64(^uint64(0)>>1)/int64(time.Millisecond) {
		limit = time.Duration(int64(^uint64(0) >> 1))
	} else {
		limit = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if limit > e.cfg.Timeout {
		limit = e.cfg.Timeout
	}
	opCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	parser := newResultParser(req)
	process, err := e.runProcess(opCtx, opDir, e.dispatchArgs(), requestJSON, parser.accept)
	if err != nil {
		return domain.InferenceResult{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return domain.InferenceResult{}, contextFailure(ctxErr)
	}
	if process.ctxErr != nil {
		return domain.InferenceResult{}, contextFailure(process.ctxErr)
	}
	if opErr := opCtx.Err(); opErr != nil {
		return domain.InferenceResult{}, contextFailure(opErr)
	}
	if process.handlerErr != nil {
		var typed *remote.InferenceFailure
		if errors.As(process.handlerErr, &typed) {
			return domain.InferenceResult{}, typed
		}
		return domain.InferenceResult{}, failure(remote.FailureInvalidOutput, "invalid inference output")
	}
	if process.writeErr != nil {
		return domain.InferenceResult{}, failure(remote.FailureInvalidOutput, "inference process did not accept its request")
	}
	if strings.Contains(strings.ToLower(process.stderr), "traceback (most recent call last):") {
		return domain.InferenceResult{}, failure(remote.FailureInvalidOutput, "python inference traceback: "+process.stderr)
	}
	if process.waitErr != nil {
		return domain.InferenceResult{}, failureForExit(process.stderr, opDir)
	}
	if err := parser.finish(); err != nil {
		return domain.InferenceResult{}, failure(remote.FailureInvalidOutput, "incomplete inference result stream")
	}
	result = parser.result
	if err := result.ValidateFor(req, req.Input.DurationMS, 0); err != nil {
		return domain.InferenceResult{}, failure(remote.FailureInvalidOutput, "inference result failed validation")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return domain.InferenceResult{}, contextFailure(ctxErr)
	}
	return result, nil
}

// Capabilities probes the same interpreter and caches successful or failed
// probes for 30 seconds. A probe failure degrades to no advertised capabilities.
func (e *Executor) Capabilities(ctx context.Context) ([]remote.InferenceCapability, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return []remote.InferenceCapability{}, nil
	}
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	if ctx.Err() != nil {
		return []remote.InferenceCapability{}, nil
	}
	if time.Since(e.probeAt) < capabilityTTL {
		return cloneCapabilities(e.probeCaps), nil
	}

	opDir, err := e.newOperationDir()
	if err != nil {
		e.cacheProbe(nil)
		return []remote.InferenceCapability{}, nil
	}
	defer os.RemoveAll(opDir)
	probeCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	var frames int
	var caps []remote.InferenceCapability
	parseErr := error(nil)
	handler := func(line []byte) error {
		frames++
		if frames != 1 {
			parseErr = fmt.Errorf("probe must emit one frame")
			return parseErr
		}
		var frame struct {
			Type         string                       `json:"type"`
			Capabilities []remote.InferenceCapability `json:"capabilities"`
		}
		if err := json.Unmarshal(line, &frame); err != nil || frame.Type != "capabilities" {
			parseErr = fmt.Errorf("invalid capability frame")
			return parseErr
		}
		for _, capability := range frame.Capabilities {
			if capability.Validate() == nil {
				caps = append(caps, capability)
			}
		}
		return nil
	}
	process, runErr := e.runProcess(probeCtx, opDir, append(e.dispatchArgs(), "--probe"), nil, handler)
	if runErr != nil || process.ctxErr != nil || process.handlerErr != nil || process.writeErr != nil || process.waitErr != nil || parseErr != nil || frames != 1 {
		caps = nil
	}
	caps = cloneCapabilities(caps)
	e.cacheProbe(caps)
	return cloneCapabilities(caps), nil
}

func (e *Executor) cacheProbe(caps []remote.InferenceCapability) {
	e.probeCaps = cloneCapabilities(caps)
	e.probeAt = time.Now()
}

func (e *Executor) newOperationDir() (string, error) {
	dir, err := os.MkdirTemp(e.cfg.ScratchDir, "inference-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func (e *Executor) dispatchArgs() []string {
	return []string{"-m", "inference.dispatch", "--model-dir", e.cfg.ModelDir,
		"--device", e.cfg.Device, "--dtype", e.cfg.Dtype}
}

type pythonRequest struct {
	SchemaVersion string                    `json:"schema_version"`
	OperationID   string                    `json:"operation_id"`
	Operation     domain.InferenceOperation `json:"operation"`
	ModelID       string                    `json:"model_id"`
	ModelRevision string                    `json:"model_revision"`
	SpaceID       string                    `json:"space_id"`
	InputHash     string                    `json:"input_hash"`
	ConfigHash    string                    `json:"config_hash"`
	TimeoutMS     int64                     `json:"timeout_ms"`
	Input         pythonInput               `json:"input"`
	Settings      map[string]any            `json:"settings"`
}

type pythonInput struct {
	Kind       domain.InputKind     `json:"kind"`
	Path       string               `json:"path,omitempty"`
	DurationMS *int64               `json:"duration_ms,omitempty"`
	Interval   *domain.TimeInterval `json:"interval,omitempty"`
	Digest     string               `json:"digest,omitempty"`
	Text       string               `json:"text,omitempty"`
}

func (e *Executor) wireRequest(req domain.InferenceRequest) ([]byte, error) {
	var settings map[string]any
	switch req.Operation {
	case domain.OpVisualEmbedding:
		settings = map[string]any{"visual": req.Visual}
	case domain.OpSoundEvents:
		settings = map[string]any{"sound_events": req.SoundEvent}
	case domain.OpAudioEmbedding:
		settings = map[string]any{"audio": req.Audio}
	case domain.OpDiarization:
		settings = map[string]any{"diarization": req.Diarization}
	case domain.OpSpeakerEmbedding:
		// The Python protocol names the eligible clean-speech regions
		// "segments"; the domain keeps the request interval plus profile
		// identity. Send only what the model process is allowed to see.
		if req.Speaker == nil || req.Speaker.Interval == nil {
			return nil, failure(remote.FailureUnsupportedInput, "speaker embedding request has no interval")
		}
		settings = map[string]any{"speaker": map[string]any{
			"segments": []map[string]int64{{
				"start_ms": req.Speaker.Interval.StartMS,
				"end_ms":   req.Speaker.Interval.EndMS,
			}},
		}}
	default:
		return nil, failure(remote.FailureUnsupportedOperation, "operation is not supported by the Python dispatcher")
	}
	input := pythonInput{Kind: req.Input.Kind}
	switch req.Input.Kind {
	case domain.InputKindAsset:
		if e.cfg.ResolveAssetPath == nil {
			return nil, failure(remote.FailureUnsupportedInput, "asset input has no local path resolver")
		}
		path, err := e.cfg.ResolveAssetPath(req.Input)
		if err != nil || !filepath.IsAbs(path) || !isRegularFile(path) {
			return nil, failure(remote.FailureUnsupportedInput, "asset input path is unavailable")
		}
		duration := req.Input.DurationMS
		input.Path = filepath.Clean(path)
		input.DurationMS = &duration
		input.Interval = req.Input.Interval
	case domain.InputKindImage:
		if e.cfg.ResolveImagePath == nil {
			return nil, failure(remote.FailureUnsupportedInput, "image input has no local path resolver")
		}
		path, err := e.cfg.ResolveImagePath(req.Input.ReferenceID, req.Input.Digest)
		if err != nil || !filepath.IsAbs(path) || !isRegularFile(path) {
			return nil, failure(remote.FailureUnsupportedInput, "image input path is unavailable")
		}
		input.Path = filepath.Clean(path)
		input.Digest = req.Input.Digest
	case domain.InputKindText:
		input.Text = req.Input.Text
	default:
		return nil, failure(remote.FailureUnsupportedInput, "unsupported inference input")
	}

	return json.Marshal(pythonRequest{
		SchemaVersion: req.SchemaVersion,
		OperationID:   req.OperationID,
		Operation:     req.Operation,
		ModelID:       req.ModelID,
		ModelRevision: req.ModelRevision,
		SpaceID:       req.SpaceID,
		InputHash:     req.InputHash,
		ConfigHash:    req.ConfigHash,
		TimeoutMS:     req.TimeoutMS,
		Input:         input,
		Settings:      settings,
	})
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (e *Executor) environment(operationDir string) []string {
	values := map[string]string{
		"PATH":             filepath.Dir(e.cfg.PythonPath) + ":/usr/local/bin:/usr/bin:/bin",
		"HOME":             operationDir,
		"LANG":             "C.UTF-8",
		"PYTHONUNBUFFERED": "1",
		"PYTHONNOUSERSITE": "1",
		"PYTHONPATH":       e.cfg.moduleRoot,
	}
	if e.cfg.Device == "cuda" {
		values["CUDA_VISIBLE_DEVICES"] = "0"
	}
	for _, item := range e.cfg.Env {
		key, value, _ := strings.Cut(item, "=")
		switch key {
		case "PATH", "HOME", "LANG", "PYTHONUNBUFFERED", "PYTHONNOUSERSITE", "PYTHONPATH", "CUDA_VISIBLE_DEVICES":
			continue
		default:
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func cloneCapabilities(in []remote.InferenceCapability) []remote.InferenceCapability {
	result := make([]remote.InferenceCapability, len(in))
	copy(result, in)
	return result
}

func failure(code remote.InferenceFailureCode, message string) error {
	return &remote.InferenceFailure{Code: code, Message: message}
}

func contextFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return failure(remote.FailureCancelled, "inference was cancelled")
	}
	return failure(remote.FailureDeadlineExceeded, "inference deadline exceeded")
}

type processResult struct {
	stderr     string
	waitErr    error
	handlerErr error
	writeErr   error
	ctxErr     error
}

type frameHandler func([]byte) error

// runProcess owns all process I/O and kills the process group on cancellation,
// protocol failure, or return from a valid parent process (to catch children
// that outlive it). Explicit os.Pipe files let Wait run concurrently with reads.
func (e *Executor) runProcess(ctx context.Context, operationDir string, args []string, request []byte, handler frameHandler) (processResult, error) {
	cmd := exec.Command(e.cfg.PythonPath, args...)
	cmd.Dir = operationDir
	cmd.Env = e.environment(operationDir)
	configureProcessTree(cmd)

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return processResult{}, failure(remote.FailureResourceExhausted, "cannot create subprocess pipes")
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return processResult{}, failure(remote.FailureResourceExhausted, "cannot create subprocess pipes")
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return processResult{}, failure(remote.FailureResourceExhausted, "cannot create subprocess pipes")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return processResult{}, failure(remote.FailureModelUnavailable, "cannot start inference interpreter")
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	stdoutCh := make(chan error, 1)
	go func() { stdoutCh <- readLines(stdoutR, e.cfg.MaxFrameBytes, handler); _ = stdoutR.Close() }()
	stderrCh := make(chan string, 1)
	go func() { stderrCh <- readBounded(stderrR, maxStderrBytes); _ = stderrR.Close() }()
	writeCh := make(chan error, 1)
	go func() {
		var writeErr error
		if len(request) != 0 {
			writeErr = writeAll(stdinW, request)
		}
		closeErr := stdinW.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		writeCh <- writeErr
	}()

	var out processResult
	var waitDone, stdoutDone, stderrDone, writeDone bool
	var stdoutErr error
	var stderrText string
	var waitErr error
	for !(waitDone && stdoutDone && stderrDone && writeDone) {
		select {
		case <-ctx.Done():
			out.ctxErr = ctx.Err()
			killProcessTree(cmd)
			waitDone = awaitWait(waitCh, &waitErr, waitDone)
			if !stdoutDone {
				stdoutErr = awaitStdout(stdoutCh)
				stdoutDone = true
			}
			if !writeDone {
				<-writeCh
				writeDone = true
			}
			if !stderrDone {
				stderrText = <-stderrCh
				stderrDone = true
			}
		case err := <-stdoutCh:
			stdoutErr = err
			stdoutDone = true
			if err != nil {
				killProcessTree(cmd)
			}
		case err := <-writeCh:
			if err != nil {
				out.writeErr = err
				killProcessTree(cmd)
			}
			writeDone = true
		case text := <-stderrCh:
			stderrText = text
			stderrDone = true
		case err := <-waitCh:
			waitErr = err
			waitDone = true
		}
		if stdoutDone && stdoutErr != nil {
			out.handlerErr = stdoutErr
			if !waitDone {
				killProcessTree(cmd)
				waitErr = <-waitCh
				waitDone = true
			}
			if !writeDone {
				<-writeCh
				writeDone = true
			}
			if !stderrDone {
				stderrText = <-stderrCh
				stderrDone = true
			}
			break
		}
		if out.writeErr != nil && !waitDone {
			waitErr = <-waitCh
			waitDone = true
		}
	}
	// A process may have completed while a descendant retained the pipes. Once
	// the parent exits, terminate any remaining members of its process tree.
	killProcessTree(cmd)
	out.waitErr = waitErr
	out.stderr = redactStderr(stderrText, operationDir)
	return out, nil
}

func awaitWait(ch <-chan error, value *error, done bool) bool {
	if done {
		return true
	}
	*value = <-ch
	return true
}

func awaitStdout(ch <-chan error) error { return <-ch }

func writeAll(w io.Writer, p []byte) error {
	for len(p) != 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func readLines(r io.Reader, maxBytes int64, handler frameHandler) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := readBoundedLine(reader, maxBytes)
		if len(line) != 0 {
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(bytes.TrimSpace(line)) == 0 {
				return fmt.Errorf("empty stdout frame")
			}
			if handleErr := handler(line); handleErr != nil {
				return handleErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return nil
			}
			if errors.Is(err, io.EOF) && len(line) != 0 {
				return nil
			}
			return err
		}
	}
}

func readBoundedLine(reader *bufio.Reader, maxBytes int64) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 {
			if len(part) != 0 && part[len(part)-1] == '\n' {
				part = part[:len(part)-1]
			}
			if int64(len(line))+int64(len(part)) > maxBytes {
				return nil, fmt.Errorf("stdout frame exceeds configured limit")
			}
			line = append(line, part...)
		}
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func readBounded(r io.Reader, limit int) string {
	var kept bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 && kept.Len() < limit {
			remaining := limit - kept.Len()
			if n > remaining {
				n = remaining
			}
			_, _ = kept.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return kept.String()
}

var (
	unixPathPattern    = regexp.MustCompile(`(/[^[:space:]"'<>|,)]+)`)
	windowsPathPattern = regexp.MustCompile(`(?i)([a-z]:\\[^[:space:]"'<>|,)]+)`)
	bearerPattern      = regexp.MustCompile(`(?i)\b(bearer\s+)[a-z0-9._~+/-]+=*`)
	tokenPattern       = regexp.MustCompile(`(?i)(\bsk-[a-z0-9]{8,}\b|\bhf_[a-z0-9]{8,}\b|\bgh[pousr]_[a-z0-9]{8,}\b|\bxox[baprs]-[a-z0-9-]{8,}\b|\{\{[a-z0-9_]+\}\})`)
	longSecretPattern  = regexp.MustCompile(`\b[A-Za-z0-9_+/=-]{40,}\b`)
)

func redactStderr(stderr, scratch string) string {
	text := strings.ToValidUTF8(stderr, "�")
	text = bearerPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = tokenPattern.ReplaceAllString(text, "[REDACTED]")
	text = longSecretPattern.ReplaceAllString(text, "[REDACTED]")
	text = redactPaths(text, unixPathPattern, scratch, "/")
	text = redactPaths(text, windowsPathPattern, scratch, `\`)
	text = strings.TrimSpace(text)
	if len(text) > maxStderrBytes {
		text = text[:maxStderrBytes]
	}
	return text
}

func redactPaths(text string, pattern *regexp.Regexp, scratch, separator string) string {
	return pattern.ReplaceAllStringFunc(text, func(match string) string {
		indices := pattern.FindStringSubmatchIndex(match)
		if len(indices) < 4 {
			return match
		}
		path := match[indices[2]:indices[3]]
		if pathWithinScratch(path, scratch, separator) {
			return match
		}
		return strings.Replace(match, path, "[PATH]", 1)
	})
}

func pathWithinScratch(path, scratch, separator string) bool {
	cleanPath := filepath.Clean(path)
	cleanScratch := filepath.Clean(scratch)
	if separator == `\` {
		cleanPath = strings.ToLower(strings.ReplaceAll(cleanPath, `\`, "/"))
		cleanScratch = strings.ToLower(strings.ReplaceAll(cleanScratch, `\`, "/"))
	}
	rel, err := filepath.Rel(cleanScratch, cleanPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func failureForExit(stderr, scratch string) error {
	lower := strings.ToLower(stderr)
	code := remote.FailureInvalidOutput
	if !strings.Contains(lower, "traceback (most recent call last):") &&
		(strings.Contains(lower, "out of memory") || strings.Contains(lower, "cannot allocate memory") || strings.Contains(lower, "resource exhausted")) {
		code = remote.FailureResourceExhausted
	}
	message := "inference process exited unsuccessfully"
	if cleaned := redactStderr(stderr, scratch); cleaned != "" {
		message += ": " + cleaned
	}
	return failure(code, message)
}

type resultParser struct {
	req         domain.InferenceRequest
	frames      int
	expected    int
	nextSeq     int
	digest      string
	hash        hash.Hash
	finalSeen   bool
	result      domain.InferenceResult
	initialized bool
}

func newResultParser(req domain.InferenceRequest) *resultParser {
	return &resultParser{req: req, hash: sha256.New()}
}

func (p *resultParser) accept(line []byte) error {
	var frame struct {
		Type        string          `json:"type"`
		OperationID string          `json:"operation_id"`
		Sequence    int             `json:"sequence"`
		Count       int             `json:"count"`
		Final       bool            `json:"final"`
		Digest      string          `json:"digest"`
		Code        string          `json:"code"`
		Message     string          `json:"message"`
		Retryable   *bool           `json:"retryable"`
		Result      json.RawMessage `json:"result"`
	}
	if p.finalSeen {
		return fmt.Errorf("protocol frame followed the final result")
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		return fmt.Errorf("malformed protocol frame")
	}
	switch frame.Type {
	case "progress":
		if frame.OperationID != "" && frame.OperationID != p.req.OperationID {
			return fmt.Errorf("progress operation id mismatch")
		}
		if p.frames < maxProgressFrames {
			p.frames++
		}
		return nil
	case "error":
		// A protocol-level rejection may arrive before the dispatcher has read
		// the operation id; an empty id is therefore accepted, but a different
		// non-empty id is still a contract violation.
		if frame.OperationID != "" && frame.OperationID != p.req.OperationID {
			return fmt.Errorf("error operation id mismatch")
		}
		code := remote.InferenceFailureCode(frame.Code)
		if !code.Valid() {
			return fmt.Errorf("unknown inference error code")
		}
		return &remote.InferenceFailure{Code: code, Message: redactStderr(frame.Message, "")}
	case "result":
		if p.finalSeen {
			return fmt.Errorf("result frame followed final frame")
		}
		if frame.OperationID != p.req.OperationID || frame.Sequence != p.nextSeq || frame.Count <= 0 || frame.Sequence >= frame.Count {
			return fmt.Errorf("result sequence or operation id is invalid")
		}
		if p.expected == 0 {
			p.expected = frame.Count
			p.digest = frame.Digest
		} else if p.expected != frame.Count || p.digest != frame.Digest {
			return fmt.Errorf("result frame count or digest changed")
		}
		trimmedResult := bytes.TrimSpace(frame.Result)
		if !validDigest(frame.Digest) || len(trimmedResult) == 0 || trimmedResult[0] != '{' {
			return fmt.Errorf("result frame is missing result JSON or digest")
		}
		if frame.Final != (frame.Sequence == frame.Count-1) {
			return fmt.Errorf("result final marker does not match frame count")
		}
		// Digest input is the exact UTF-8 JSON value bytes for each `result`
		// member, concatenated in ascending sequence order with no delimiter;
		// the enclosing frame, whitespace outside the value and newline are not
		// included. Python emits canonical JSON, making each value deterministic.
		_, _ = p.hash.Write(frame.Result)
		if err := p.mergeResult(frame.Result); err != nil {
			return err
		}
		p.frames++
		p.nextSeq++
		p.finalSeen = frame.Final
		return nil
	default:
		return fmt.Errorf("unknown protocol frame type")
	}
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func (p *resultParser) mergeResult(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("result payload is not an object")
	}
	// Decode the standard fields normally, then restore embedding vectors,
	// whose domain DTO intentionally hides them from ordinary JSON marshaling.
	var partial domain.InferenceResult
	if err := json.Unmarshal(raw, &partial); err != nil {
		return fmt.Errorf("result payload cannot be decoded")
	}
	if err := decodeEmbeddings(fields["embeddings"], &partial.Embeddings); err != nil {
		return err
	}
	if err := decodeArray(fields["events"], &partial.Events); err != nil {
		return err
	}
	if err := decodeArray(fields["turns"], &partial.Turns); err != nil {
		return err
	}
	if err := decodeArray(fields["segments"], &partial.Segments); err != nil {
		return err
	}
	if err := decodeArray(fields["words"], &partial.Words); err != nil {
		return err
	}
	if !p.initialized {
		p.result = partial
		p.initialized = true
		return nil
	}
	if partial.SchemaVersion != "" && p.result.SchemaVersion != "" && partial.SchemaVersion != p.result.SchemaVersion ||
		partial.OperationID != "" && p.result.OperationID != "" && partial.OperationID != p.result.OperationID ||
		partial.Operation != "" && p.result.Operation != "" && partial.Operation != p.result.Operation ||
		partial.ModelID != "" && p.result.ModelID != "" && partial.ModelID != p.result.ModelID ||
		partial.ModelRevision != "" && p.result.ModelRevision != "" && partial.ModelRevision != p.result.ModelRevision ||
		partial.SpaceID != "" && p.result.SpaceID != "" && partial.SpaceID != p.result.SpaceID ||
		partial.ConfigHash != "" && p.result.ConfigHash != "" && partial.ConfigHash != p.result.ConfigHash ||
		partial.InputHash != "" && p.result.InputHash != "" && partial.InputHash != p.result.InputHash ||
		partial.Backend != "" && p.result.Backend != "" && partial.Backend != p.result.Backend {
		return fmt.Errorf("result frame metadata changed")
	}
	if partial.SchemaVersion != "" {
		p.result.SchemaVersion = partial.SchemaVersion
	}
	if partial.OperationID != "" {
		p.result.OperationID = partial.OperationID
	}
	if partial.Operation != "" {
		p.result.Operation = partial.Operation
	}
	if partial.ModelID != "" {
		p.result.ModelID = partial.ModelID
	}
	if partial.ModelRevision != "" {
		p.result.ModelRevision = partial.ModelRevision
	}
	if partial.SpaceID != "" {
		p.result.SpaceID = partial.SpaceID
	}
	if partial.ConfigHash != "" {
		p.result.ConfigHash = partial.ConfigHash
	}
	if partial.InputHash != "" {
		p.result.InputHash = partial.InputHash
	}
	if partial.Backend != "" {
		p.result.Backend = partial.Backend
	}
	p.result.Coverage = append(p.result.Coverage, partial.Coverage...)
	p.result.Embeddings = append(p.result.Embeddings, partial.Embeddings...)
	p.result.Events = append(p.result.Events, partial.Events...)
	p.result.Turns = append(p.result.Turns, partial.Turns...)
	p.result.Segments = append(p.result.Segments, partial.Segments...)
	p.result.Words = append(p.result.Words, partial.Words...)
	return nil
}

func decodeArray[T any](raw json.RawMessage, output *[]T) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var values []T
	if err := json.Unmarshal(raw, &values); err != nil {
		return fmt.Errorf("result array cannot be decoded")
	}
	*output = values
	return nil
}

func decodeEmbeddings(raw json.RawMessage, output *[]domain.EmbeddingRecord) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var records []struct {
		ShotID      string          `json:"shot_id"`
		SampleIndex int             `json:"sample_index"`
		TimestampMS int64           `json:"timestamp_ms"`
		StartMS     int64           `json:"start_ms"`
		EndMS       int64           `json:"end_ms"`
		Strategy    string          `json:"strategy"`
		SourceHash  string          `json:"source_hash"`
		Quality     json.RawMessage `json:"quality"`
		Vector      []float32       `json:"vector"`
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		return fmt.Errorf("embedding records cannot be decoded")
	}
	// Assign rather than append: the standard unmarshal above already filled
	// the slice with vector-less records, and appending would duplicate every
	// embedding (with the first copy rejected as empty).
	decoded := make([]domain.EmbeddingRecord, 0, len(records))
	for _, record := range records {
		decoded = append(decoded, domain.EmbeddingRecord{
			ShotID: record.ShotID, SampleIndex: record.SampleIndex, TimestampMS: record.TimestampMS,
			StartMS: record.StartMS, EndMS: record.EndMS, Strategy: record.Strategy,
			SourceHash: record.SourceHash, QualityJSON: qualityJSON(record.Quality), Vector: record.Vector,
		})
	}
	*output = decoded
	return nil
}

func qualityJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	return string(trimmed)
}

func (p *resultParser) finish() error {
	if p.expected == 0 || p.nextSeq != p.expected || !p.finalSeen || !p.initialized {
		return fmt.Errorf("result stream ended before its final frame")
	}
	actual := "sha256:" + hex.EncodeToString(p.hash.Sum(nil))
	if actual != p.digest {
		return fmt.Errorf("result digest mismatch")
	}
	return nil
}

// resolveAutoDevice picks cuda only when an NVIDIA device is visible. It is a
// cheap presence check, not a compute test: a broken driver still yields a
// capability probe that reports the model unavailable.
func resolveAutoDevice() string {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return "cpu"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path, "-L").Run(); err != nil {
		return "cpu"
	}
	return "cuda"
}
