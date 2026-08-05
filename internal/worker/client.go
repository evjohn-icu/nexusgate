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
		"stage": stage, "progress": progress, "event": event, "message": message,
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
	bearerPattern     = regexp.MustCompile(`(?i)Bearer\s+[^\s\x00-\x1f]+`)
	openAIKeyPattern  = regexp.MustCompile(`sk-[A-Za-z0-9_-]{32,}`)
	apiKeyHeaderValue = regexp.MustCompile(`(?i)(X-Api-Key|api[_\-]?key|apikey|auth["']?\s*[:=])\s*[:=]\s*[^\s,;]+`)
)

// redactSecrets replaces common secret patterns (Bearer tokens, OpenAI-style
// keys, API key header values) with [redacted] so that error text sent to the
// Hub never contains provider credentials. It follows the same bounded-length
// philosophy as common.ReadError: the text may still contain the structure of
// the error, but no usable key material.
func redactSecrets(s string) string {
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = openAIKeyPattern.ReplaceAllString(s, "[redacted]")
	s = apiKeyHeaderValue.ReplaceAllString(s, "$1: [redacted]")
	return s
}

func normalizeFingerprint(value string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "sha256", "").Replace(value))
}
