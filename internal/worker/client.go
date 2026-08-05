// Package worker is the small, persistent runtime used by Windows and Linux
// compute nodes. It talks to the Hub over HTTPS and never opens Hub storage.
package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/credentials"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/remote"
)

type Enrollment struct {
	Worker remote.Worker `json:"worker"`
	Token  string        `json:"token"`
}

type Client struct {
	baseURL string
	http    *http.Client
}

type ArtifactUpload struct {
	Type        string
	ProfileHash string
	Path        string
}

// ProviderProxyResponse contains a Provider's JSON response as relayed by the
// Hub. It intentionally carries no credential fields: keys never leave the
// Hub when a Worker selects proxy mode.
type ProviderProxyResponse struct {
	Provider   string          `json:"provider"`
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body"`
}

// NewClient pins a SHA-256 certificate fingerprint for HTTPS Hubs. HTTP is
// accepted only for tests/local development; the CLI rejects it for production
// enrollment unless explicitly requested by a future dev option.
func NewClient(baseURL, fingerprint string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(fingerprint) != "" {
		expected := normalizeFingerprint(fingerprint)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("Hub did not present a certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if normalizeFingerprint(hex.EncodeToString(sum[:])) != expected {
				return fmt.Errorf("Hub certificate fingerprint mismatch")
			}
			return nil
		}}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (c *Client) Enroll(ctx context.Context, pairingToken string, registration remote.WorkerRegistration) (Enrollment, error) {
	body := struct {
		PairingToken string `json:"pairing_token"`
		remote.WorkerRegistration
	}{PairingToken: pairingToken, WorkerRegistration: registration}
	var out Enrollment
	if err := c.postJSON(ctx, "/api/v1/worker/enroll", "", body, &out, http.StatusCreated); err != nil {
		return Enrollment{}, err
	}
	if out.Worker.ID == "" || out.Token == "" {
		return Enrollment{}, fmt.Errorf("Hub returned incomplete worker enrollment")
	}
	return out, nil
}

func (c *Client) Heartbeat(ctx context.Context, token string, capabilities remote.WorkerCapabilities) error {
	return c.postJSON(ctx, "/api/v1/worker/heartbeat", token, map[string]any{"capabilities": capabilities}, nil, http.StatusNoContent)
}

func (c *Client) Lease(ctx context.Context, token string) (*remote.WorkerJob, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/worker/lease", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("Hub %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var job remote.WorkerJob
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (c *Client) Complete(ctx context.Context, token, jobID string, state domain.JobState, message string) error {
	return c.postJSON(ctx, "/api/v1/worker/jobs/"+jobID+"/complete", token, map[string]any{"state": state, "message": redactSecrets(message)}, nil, http.StatusNoContent)
}

// Progress records a bounded, lease-owned stage update. It is safe to call
// opportunistically; a stale lease is rejected by the Hub rather than being
// allowed to overwrite the current workflow state.
func (c *Client) Progress(ctx context.Context, token, jobID, stage string, progress float64, event, message string) error {
	return c.postJSON(ctx, "/api/v1/worker/jobs/"+jobID+"/progress", token, map[string]any{
		"stage": stage, "progress": progress, "event": event, "message": redactSecrets(message),
	}, nil, http.StatusNoContent)
}

// Credential fetches a short-lived task credential. Callers must keep the
// returned API key in memory and discard it when the task exits.
func (c *Client) Credential(ctx context.Context, token, jobID string, operation credentials.Operation) (credentials.Lease, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(string(operation)) == "" {
		return credentials.Lease{}, fmt.Errorf("credential request requires job id and operation")
	}
	var lease credentials.Lease
	if err := c.postJSON(ctx, "/api/v1/worker/jobs/"+jobID+"/credentials/"+string(operation), token, map[string]any{}, &lease, http.StatusOK); err != nil {
		return credentials.Lease{}, err
	}
	if lease.JobID != jobID || lease.Credential.APIKey == "" || lease.ExpiresAt == "" {
		return credentials.Lease{}, fmt.Errorf("Hub returned incomplete credential lease")
	}
	return lease, nil
}

// ProxyProviderJSON sends a small JSON-only Provider request through the Hub.
// Use Credential for explicitly trusted Worker direct mode; use this method
// when the Provider key must remain Hub-side. Media uploads are deliberately
// not accepted here, so NAS video never traverses this proxy path.
func (c *Client) ProxyProviderJSON(ctx context.Context, token, jobID string, operation credentials.Operation, body json.RawMessage) (ProviderProxyResponse, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(string(operation)) == "" || !json.Valid(body) {
		return ProviderProxyResponse{}, fmt.Errorf("provider proxy requires job id, operation and valid JSON")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/worker/jobs/"+jobID+"/provider/"+string(operation), bytes.NewReader(body))
	if err != nil {
		return ProviderProxyResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return ProviderProxyResponse{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil {
		return ProviderProxyResponse{}, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ProviderProxyResponse{}, fmt.Errorf("Hub %s: %s", response.Status, strings.TrimSpace(string(raw)))
	}
	if !json.Valid(raw) {
		return ProviderProxyResponse{}, fmt.Errorf("Hub returned non-JSON provider proxy response")
	}
	return ProviderProxyResponse{StatusCode: response.StatusCode, Body: append(json.RawMessage(nil), raw...)}, nil
}

// UploadArtifact sends one derived file to the Hub. The file is streamed from
// the Worker cache and is never persisted in the Worker configuration.
// Contract: POST /api/v1/worker/jobs/{jobID}/artifacts multipart/form-data
// with fields type, profile_hash and file field artifact. Hub may return 200
// for an idempotent reuse, or 201, 202 or 204 for an accepted upload.
func (c *Client) UploadArtifact(ctx context.Context, token, jobID string, artifact ArtifactUpload) error {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(artifact.Type) == "" || strings.TrimSpace(artifact.ProfileHash) == "" {
		return fmt.Errorf("artifact upload requires job id, type and profile hash")
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return err
	}
	defer file.Close()

	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	writeDone := make(chan error, 1)
	go func() {
		var writeErr error
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = writer.CloseWithError(ctx.Err())
			case <-done:
			}
		}()
		defer func() {
			if writeErr != nil {
				_ = writer.CloseWithError(writeErr)
			} else {
				_ = writer.Close()
			}
			writeDone <- writeErr
		}()
		if writeErr = multipartWriter.WriteField("type", artifact.Type); writeErr != nil {
			return
		}
		if writeErr = multipartWriter.WriteField("profile_hash", artifact.ProfileHash); writeErr != nil {
			return
		}
		filename := filepath.Base(artifact.Path)
		disposition := mime.FormatMediaType("form-data", map[string]string{"name": "artifact", "filename": filename})
		partHeader := make(textproto.MIMEHeader)
		partHeader.Set("Content-Disposition", disposition)
		partHeader.Set("Content-Type", artifactContentType(artifact))
		var part io.Writer
		part, writeErr = multipartWriter.CreatePart(partHeader)
		if writeErr != nil {
			return
		}
		_, writeErr = io.Copy(part, file)
		if writeErr == nil {
			writeErr = multipartWriter.Close()
		}
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/worker/jobs/"+jobID+"/artifacts", reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response, err := c.http.Do(request)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return err
	}
	defer response.Body.Close()
	if writeErr := <-writeDone; writeErr != nil {
		return writeErr
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Hub %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

func artifactContentType(artifact ArtifactUpload) string {
	if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(artifact.Path))); contentType != "" {
		return contentType
	}
	switch artifact.Type {
	case "thumbnail":
		return "image/jpeg"
	case "audio":
		return "audio/mp4"
	default:
		return "video/mp4"
	}
}

func (c *Client) postJSON(ctx context.Context, path, token string, input, output any, accepted int) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != accepted {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Hub %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output)
	}
	return nil
}

var (
	bearerPattern    = regexp.MustCompile(`(?i)Bearer\s+[^\s\x00-\x1f"]+`)
	openAIKeyPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{32,}`)
	// apiKeyHeaderValue matches HTTP-header-style credential keys
	// (X-Api-Key, api_key, apikey, auth) with : or = separator.
	apiKeyHeaderValue = regexp.MustCompile(`(?i)(X-Api-Key|api[_\-]?key|apikey|auth["']?\s*[:=])\s*[:=]\s*[^\s,;]+`)
	// credentialKeyValuePattern matches known credential key names
	// followed by = or : and a value in free-form text. This catches
	// forms like token=abc123, secret: xyz, access_token=... that
	// are not in JSON quotes.
	credentialKeyValuePattern = regexp.MustCompile(`(?i)(api[_\-]?key|apikey|token|secret|password|access[_\-]?token|api[_\-]?key[_\-]?id|credential|authorization|key)\s*[:=]\s*([^\s,;]+)`)
	// jsonKeyPattern is a regex fallback for unstructured text that
	// contains JSON-like key:value fragments. The primary redaction
	// path is jsonAwareRedact (recursive JSON walk).
	jsonKeyPattern = regexp.MustCompile(`"(?i)(api[_\-]?key|apikey|authorization|token|secret|password|access[_\-]?token|api[_\-]?key[_\-]?id|credential|key)"\s*:\s*("[^"]*"|[^\s,;}\]"]+)`)
	// uuidPattern matches UUIDs (8-4-4-4-12 hex with dashes). These
	// must never be redacted by the long-token safety net.
	uuidPattern = regexp.MustCompile(`^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`)
	// longTokenPattern is a safety net for non-JSON text. It matches
	// standalone runs (delimited by whitespace or string boundaries)
	// starting with a letter, ≥20 chars, base64-like alphabet.
	// UUIDs are excluded via ReplaceAllStringFunc.
	longTokenPattern = regexp.MustCompile(`(^|\s)([A-Za-z][A-Za-z0-9_\-+=/]{19,})(\s|$)`)
)

// sensitiveJSONKeys lists JSON object keys whose values are redacted
// during recursive walking (case-insensitive comparison).
var sensitiveJSONKeys = map[string]bool{
	"api_key":       true,
	"apikey":        true,
	"authorization": true,
	"token":         true,
	"secret":        true,
	"password":      true,
	"key":           true,
	"api_key_id":    true,
	"access_token":  true,
	"credential":    true,
}

// redactSecrets replaces common secret patterns with [redacted] so that
// error text sent to the Hub never contains provider credentials.
//
// Strategy (two-phase):
//  1. JSON-aware recursive redaction: try json.Unmarshal on the whole
//     string; if valid JSON, recursively walk maps redacting sensitive
//     keys and re-marshal. Also scan for JSON-like segments embedded in
//     free-form text (with bracket-balanced scanning for arbitrary
//     nesting depth) and redact those individually.
//  2. Regex fallback for non-JSON text: Bearer tokens, OpenAI keys,
//     credential key=value patterns, JSON-key patterns (skipped when
//     phase 1 handled the top level), and long base64-like tokens
//     (UUIDs excluded).
func redactSecrets(s string) string {
	if s == "" {
		return s
	}

	// Phase 1: JSON-aware recursive redaction. Try top-level JSON parse
	// first; if that fails, scan for embedded JSON segments using
	// bracket-balanced detection (supports arbitrary nesting depth).
	jsonHandled := false
	if redacted := jsonAwareRedact(s); redacted != "" {
		s = redacted
		jsonHandled = true
	} else {
		segments := findJSONSegments(s)
		if len(segments) > 0 {
			result := s
			anyRedacted := false
			// Process from end to start to preserve indices.
			for i := len(segments) - 1; i >= 0; i-- {
				seg := segments[i]
				segment := result[seg[0]:seg[1]]
				if redacted := jsonAwareRedact(segment); redacted != "" {
					result = result[:seg[0]] + redacted + result[seg[1]:]
					anyRedacted = true
				}
			}
			s = result
			if anyRedacted {
				jsonHandled = true
			}
		}
	}

	// Phase 2: regex fallback. Always apply Bearer, OpenAI key,
	// credential key=value, and API-key header patterns.
	// jsonKeyPattern is skipped when top-level/segments were already
	// JSON-processed; also skip already-redacted values to prevent
	// "token":"[redacted]" → "token":[redacted] corruption.
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = openAIKeyPattern.ReplaceAllString(s, "[redacted]")
	s = credentialKeyValuePattern.ReplaceAllString(s, "$1: [redacted]")
	if !jsonHandled {
		s = jsonKeyPattern.ReplaceAllStringFunc(s, func(match string) string {
			if strings.Contains(match, "[redacted]") {
				return match
			}
			// Preserve quoting: if the original value was
			// quoted, [redacted] is quoted; if unquoted,
			// [redacted] is unquoted.
			parts := jsonKeyPattern.FindStringSubmatch(match)
			if len(parts) >= 3 && strings.HasPrefix(parts[2], "\"") {
				return `"` + parts[1] + `":"[redacted]"`
			}
			return `"` + parts[1] + `":[redacted]`
		})
	}
	s = apiKeyHeaderValue.ReplaceAllString(s, "$1: [redacted]")
	s = longTokenPattern.ReplaceAllStringFunc(s, func(match string) string {
		parts := longTokenPattern.FindStringSubmatch(match)
		if len(parts) >= 4 {
			token := parts[2]
			if uuidPattern.MatchString(token) {
				return match
			}
			return parts[1] + "[redacted]" + parts[3]
		}
		return match
	})
	return s
}

// jsonAwareRedact attempts to parse raw as JSON. If successful, it
// recursively walks the value redacting sensitive keys and re-parsing
// nested JSON strings. Returns the re-marshalled JSON string, or "" if
// raw is not valid JSON.
func jsonAwareRedact(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return ""
	}
	walkAndRedact(v)
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// walkAndRedact recursively walks a JSON value. For maps, it redacts
// values under sensitive keys. For slices, it recurses into each element.
// For string values, it first attempts to parse the string as nested JSON;
// if that fails, it scans for embedded JSON fragments (e.g.
// {"body":"prefix {\"token\":\"k\"} suffix"}) and redacts those.
func walkAndRedact(v any) {
	switch val := v.(type) {
	case map[string]any:
		for k, sub := range val {
			if sensitiveJSONKeys[strings.ToLower(k)] {
				val[k] = "[redacted]"
			} else if s, ok := sub.(string); ok {
				// If the string value is itself valid JSON, recursively
				// redact it and store the result as a string. This
				// handles the escaped-nested-JSON bypass: the outer JSON
				// parses, the string field is extracted, and we re-parse
				// it here.  Keeping it as a string preserves the original
				// schema (e.g. {"body":"..."} stays a string field).
				if redacted := jsonAwareRedact(s); redacted != "" {
					val[k] = redacted
				} else {
					// String is not valid JSON overall, but may contain
					// embedded JSON fragments.
					val[k] = redactEmbeddedJSONInString(s)
				}
			} else {
				walkAndRedact(sub)
			}
		}
	case []any:
		for i, item := range val {
			if s, ok := item.(string); ok {
				if redacted := jsonAwareRedact(s); redacted != "" {
					val[i] = redacted
				} else {
					val[i] = redactEmbeddedJSONInString(s)
				}
			} else {
				walkAndRedact(item)
			}
		}
	}
}

// findJSONSegments finds balanced JSON object literals ({...}) embedded in
// free-form text. It counts brackets and is aware of Go/JSON string escaping
// so that braces inside quoted strings do not confuse the counter. Supports
// arbitrary nesting depth.
func findJSONSegments(s string) [][2]int {
	var segments [][2]int
	inString := false
	escape := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			if ch == '\\' {
				escape = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == '{' {
			depth := 1
			start := i
			innerString := false
			innerEscape := false
			j := i + 1
			for ; j < len(s) && depth > 0; j++ {
				c := s[j]
				if innerEscape {
					innerEscape = false
					continue
				}
				if innerString {
					if c == '\\' {
						innerEscape = true
					} else if c == '"' {
						innerString = false
					}
					continue
				}
				if c == '"' {
					innerString = true
					continue
				}
				switch c {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			if depth == 0 {
				segments = append(segments, [2]int{start, j})
			}
			i = j - 1
		}
	}
	return segments
}

// redactEmbeddedJSONInString scans a string for embedded JSON object
// segments and redacts sensitive keys within each found segment.
func redactEmbeddedJSONInString(s string) string {
	segments := findJSONSegments(s)
	if len(segments) == 0 {
		return s
	}
	result := s
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		segment := result[seg[0]:seg[1]]
		if redacted := jsonAwareRedact(segment); redacted != "" {
			result = result[:seg[0]] + redacted + result[seg[1]:]
		}
	}
	return result
}

func normalizeFingerprint(value string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "sha256", "").Replace(value))
}
