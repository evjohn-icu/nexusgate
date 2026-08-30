package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/hubtls"
)

type workerMount struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
}

var workerPlatforms = map[string]struct {
	Filename string
	Shell    string
}{
	"windows-amd64": {"timingdex-windows-amd64.exe", "powershell"},
	"linux-amd64":   {"timingdex-linux-amd64", "posix"},
	"linux-arm64":   {"timingdex-linux-arm64", "posix"},
}

func (s *Server) workerSetupRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("page-worker-setup", "GET /worker-setup", routeAuthBrowserPage, http.HandlerFunc(s.workerSetupPage)),
		newRouteSpec("worker-setup-context", "GET /api/v1/hub/worker-setup/context", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.workerSetupContext))),
		newRouteSpec("worker-setup-library-roots", "GET /api/v1/admin/hub/worker-setup/library-roots", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.workerSetupLibraryRoots))),
		newRouteSpec("worker-binary", "GET /api/v1/hub/worker-binaries/{platform}", routeAuthWorkerBootstrap, http.HandlerFunc(s.routeAuthWorkerBootstrap(s.workerServeBinary))),
		newRouteSpec("worker-setup-script", "POST /api/v1/hub/worker-setup/script", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.workerGenerateScript))),
	}
}

func (s *Server) workerSetupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.serveLocalizedPage(w, r, "/worker-setup", workerSetupPageHTML)
}

func (s *Server) workerSetupContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	hubURL := scheme + "://" + r.Host

	tlsActive := s.tlsCert != ""
	var fingerprint, spkiPin string
	if tlsActive {
		fingerprint, spkiPin, _ = s.hubtlsIdentity()
	}
	roots, err := s.service.ListLibraryRoots(r.Context())
	if err != nil {
		roots = []domain.LibraryRoot{}
	}

	binariesDir := filepath.Join(s.service.DataDir(), "worker-binaries")
	availableBinaries := make(map[string]map[string]any, len(workerPlatforms))
	for key, info := range workerPlatforms {
		binPath := filepath.Join(binariesDir, info.Filename)
		fi, statErr := os.Stat(binPath)
		entry := map[string]any{"exists": false, "size_bytes": 0}
		if statErr == nil && !fi.IsDir() {
			entry["exists"] = true
			entry["size_bytes"] = fi.Size()
		}
		availableBinaries[key] = entry
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hub_url":            hubURL,
		"fingerprint":        fingerprint,
		"spki_pin":           spkiPin,
		"tls":                tlsActive,
		"library_roots":      rootReferences(roots),
		"available_binaries": availableBinaries,
	})
}

func rootReferences(roots []domain.LibraryRoot) []workerSetupRootReference {
	refs := make([]workerSetupRootReference, 0, len(roots))
	for _, root := range roots {
		refs = append(refs, workerSetupRootReference{ID: root.ID})
	}
	return refs
}

type workerSetupRootReference struct {
	ID string `json:"id"`
}

type workerSetupRootDetail struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type workerSetupLibraryRootsResponse struct {
	LibraryRoots []workerSetupRootDetail `json:"library_roots"`
}

func (s *Server) workerSetupLibraryRoots(w http.ResponseWriter, r *http.Request) {
	roots, err := s.service.ListLibraryRoots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	details := make([]workerSetupRootDetail, 0, len(roots))
	for _, root := range roots {
		details = append(details, workerSetupRootDetail{ID: root.ID, Path: root.Path})
	}
	writeJSON(w, http.StatusOK, workerSetupLibraryRootsResponse{LibraryRoots: details})
}

func (s *Server) workerServeBinary(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	info, ok := workerPlatforms[platform]
	if !ok {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "unknown platform: " + platform})
		return
	}
	binPath := filepath.Join(s.service.DataDir(), "worker-binaries", info.Filename)
	if _, err := os.Stat(binPath); err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "binary not available for platform: " + platform})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, binPath)
}

func (s *Server) workerGenerateScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Platform     string        `json:"platform"`
		Name         string        `json:"name"`
		PairingToken string        `json:"pairing_token"`
		Mounts       []workerMount `json:"mounts"`
		CacheDir     string        `json:"cache_dir"`
	}
	if !decodeStrictJSON(w, r, &req, 1<<20) {
		return
	}

	info, ok := workerPlatforms[req.Platform]
	if !ok {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "unknown platform: " + req.Platform})
		return
	}

	if strings.TrimSpace(req.PairingToken) == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "pairing_token is required"})
		return
	}

	hubURL := "http"
	if r.TLS != nil {
		hubURL = "https"
	}
	hubURL += "://" + r.Host

	// The generated script pins the Hub certificate and verifies the binary's
	// SHA-256 digest, so generation requires the platform artifact and an
	// HTTPS request whose identity the script can embed. Failing either
	// before any pairing token is minted keeps a wasted credential out of the
	// page's hands.
	binPath := filepath.Join(s.service.DataDir(), "worker-binaries", info.Filename)
	raw, err := os.ReadFile(binPath)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: "worker_binary_unavailable", Message: "worker binary is not available for platform: " + req.Platform, Action: "upload_worker_binary"})
		return
	}
	sum := sha256.Sum256(raw)
	binaryDigest := hex.EncodeToString(sum[:])

	if r.TLS == nil {
		writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: "worker_tls_required", Message: "generating a Worker install script requires HTTPS so the script can pin the Hub certificate", Action: "enable_hub_tls"})
		return
	}
	fingerprint, spkiPin, err := s.hubtlsIdentity()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, APIError{Code: "worker_tls_required", Message: "the Hub TLS certificate could not be read; cannot generate a pinned install script", Action: "enable_hub_tls"})
		return
	}

	// The Worker CLI has separate --config and --cache flags. Deriving the
	// config path from cache_dir also pushed a request value through
	// filepath.Join, which rewrites separators using the Hub's OS and so
	// mangles a Windows path whenever the Hub is not Windows. The install
	// scripts keep the config inside a private .timingdex-worker directory
	// that only the enrolled Worker touches.
	const configPath = "./.timingdex-worker/worker.json"

	switch info.Shell {
	case "powershell":
		s.writeWorkerSetupScript(w, s.generatePowerShell(hubURL, info.Filename, req.Platform, req.PairingToken, req.Name, req.Mounts, configPath, req.CacheDir, fingerprint, spkiPin, binaryDigest))
	default:
		s.writeWorkerSetupScript(w, s.generatePOSIX(hubURL, info.Filename, req.Platform, req.PairingToken, req.Name, req.Mounts, configPath, req.CacheDir, fingerprint, spkiPin, binaryDigest))
	}
}

// hubtlsIdentity derives the leaf certificate fingerprint and the
// curl-compatible `sha256//<base64>` SPKI pin from the Hub's TLS certificate
// file. A script must embed both or neither: the SPKI pin authenticates the
// bootstrap download while the fingerprint is what the Worker itself pins.
func (s *Server) hubtlsIdentity() (fingerprint, spkiPin string, err error) {
	if s.tlsCert == "" {
		return "", "", fmt.Errorf("Hub TLS is not configured")
	}
	fingerprint, err = hubtls.FingerprintCertificate(s.tlsCert)
	if err != nil {
		return "", "", err
	}
	leaf, err := hubtls.LoadCertificate(s.tlsCert)
	if err != nil {
		return "", "", err
	}
	spkiPin = hubtls.CertificateSPKIPin(leaf)
	if spkiPin == "" {
		return "", "", fmt.Errorf("could not derive the certificate SPKI pin")
	}
	return fingerprint, spkiPin, nil
}

func (s *Server) generatePOSIX(hubURL, filename, platform, pairingToken, name string, mounts []workerMount, configPath, cacheDir, fingerprint, spkiPin, binaryDigest string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("umask 077\n")
	b.WriteString("\n")
	b.WriteString("# WARNING: This script contains a single-use credential (one-time pairing token).\n")
	b.WriteString("# Do not commit or share this file.\n")
	b.WriteString("\n")
	b.WriteString("BINARY_URL=")
	b.WriteString(quotePOSIX(hubURL + "/api/v1/hub/worker-binaries/" + platform))
	b.WriteString("\n")
	b.WriteString("BINARY_OUT=")
	b.WriteString(quotePOSIX(filename))
	b.WriteString("\n")
	b.WriteString("BINARY_TMP=")
	b.WriteString(quotePOSIX(filename + ".download"))
	b.WriteString("\n")
	b.WriteString("CONFIG_DIR=")
	b.WriteString(quotePOSIX(filepath.Dir(configPath)))
	b.WriteString("\n")
	b.WriteString("PAIRING_TOKEN=")
	b.WriteString(quotePOSIX(pairingToken))
	b.WriteString("\n")
	b.WriteString("SPKI_PIN=")
	b.WriteString(quotePOSIX(spkiPin))
	b.WriteString("\n")
	b.WriteString("BINARY_DIGEST=")
	b.WriteString(quotePOSIX(binaryDigest))
	b.WriteString("\n")
	b.WriteString("FINGERPRINT=")
	b.WriteString(quotePOSIX(fingerprint))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("mkdir -p \"$CONFIG_DIR\"\n")
	b.WriteString("chmod 0700 \"$CONFIG_DIR\"\n")
	b.WriteString("\n")
	b.WriteString("echo 'Downloading Timingdex Worker binary...'\n")
	// --insecure is required for the Hub's self-signed certificate; the SPKI
	// pin is the trust that replaces host verification. The one-time pairing
	// token travels in a request header so it never appears in a URL.
	b.WriteString("curl --fail --location --insecure --pinnedpubkey \"$SPKI_PIN\" -H \"X-Timingdex-Pairing-Token: $PAIRING_TOKEN\" \"$BINARY_URL\" -o \"$BINARY_TMP\" || { rm -f \"$BINARY_TMP\"; exit 1; }\n")
	b.WriteString("echo \"$BINARY_DIGEST  $BINARY_TMP\" | sha256sum -c - || { rm -f \"$BINARY_TMP\"; exit 1; }\n")
	b.WriteString("mv \"$BINARY_TMP\" \"$BINARY_OUT\"\n")
	b.WriteString("chmod 0755 \"$BINARY_OUT\"\n")
	b.WriteString("\n")
	b.WriteString("echo 'Enrolling Worker...'\n")

	enrollArgs := []string{
		"./" + filename, "worker", "enroll",
		"--hub", hubURL,
		"--fingerprint", fingerprint,
		"--pairing", pairingToken,
	}
	if strings.TrimSpace(name) != "" {
		enrollArgs = append(enrollArgs, "--name", strings.TrimSpace(name))
	}
	for _, m := range mounts {
		enrollArgs = append(enrollArgs, "--mount", m.RootID+"="+m.Path)
	}
	if strings.TrimSpace(cacheDir) != "" {
		enrollArgs = append(enrollArgs, "--cache", strings.TrimSpace(cacheDir))
	}
	enrollArgs = append(enrollArgs, "--config", configPath)

	// Each argument is quoted separately. Quoting the joined string instead
	// produces one enormous quoted word, which the shell reads as the name of a
	// command to execute rather than as a command plus arguments -- safe, but the
	// script cannot run at all.
	quoted := make([]string, 0, len(enrollArgs))
	for _, arg := range enrollArgs {
		quoted = append(quoted, quotePOSIX(arg))
	}
	b.WriteString(strings.Join(quoted, " "))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("echo ''\n")
	b.WriteString("echo 'Enrolled successfully. Run the Worker with:'\n")
	// Single quotes, not double: inside double quotes $(...) and backticks still
	// execute, so an interpolated request value would run as a command on the
	// operator's machine even though it is only being printed.
	b.WriteString("echo ")
	b.WriteString(quotePOSIX("  ./" + filename + " worker run --config " + configPath))
	b.WriteString("\n")
	b.WriteString("echo ")
	b.WriteString(quotePOSIX("  ./" + filename + " worker doctor --config " + configPath))
	b.WriteString("\n")

	return b.String()
}

func (s *Server) generatePowerShell(hubURL, filename, platform, pairingToken, name string, mounts []workerMount, configPath, cacheDir, fingerprint, spkiPin, binaryDigest string) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString("\n")
	b.WriteString("# WARNING: This script contains a single-use credential (one-time pairing token).\n")
	b.WriteString("# Do not commit or share this file.\n")
	b.WriteString("\n")
	b.WriteString("$binaryUrl = ")
	b.WriteString(quotePowerShell(hubURL + "/api/v1/hub/worker-binaries/" + platform))
	b.WriteString("\n")
	b.WriteString("$binaryOut = ")
	b.WriteString(quotePowerShell(filename))
	b.WriteString("\n")
	b.WriteString("$binaryTmp = ")
	b.WriteString(quotePowerShell(filename + ".download"))
	b.WriteString("\n")
	b.WriteString("$configDir = ")
	b.WriteString(quotePowerShell(filepath.Dir(configPath)))
	b.WriteString("\n")
	b.WriteString("$fingerprint = ")
	b.WriteString(quotePowerShell(fingerprint))
	b.WriteString("\n")
	b.WriteString("$binaryDigest = ")
	b.WriteString(quotePowerShell(binaryDigest))
	b.WriteString("\n")
	b.WriteString("$pairingToken = ")
	b.WriteString(quotePowerShell(pairingToken))
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("New-Item -ItemType Directory -Force -Path $configDir | Out-Null\n")
	b.WriteString("\n")
	b.WriteString("Write-Host 'Downloading Timingdex Worker binary...'\n")
	// The self-signed Hub certificate is trusted by exact SHA-256 fingerprint
	// instead of a CA chain; the one-time pairing token travels in a request
	// header so it never appears in a URL. The file is written to a .download
	// sibling and moved only after Get-FileHash matches the Hub-provided
	// digest, so a partial or tampered download is never executed.
	b.WriteString("$handler = [System.Net.Http.HttpClientHandler]::new()\n")
	b.WriteString("$handler.ServerCertificateCustomValidationCallback = {\n")
	b.WriteString("  param($sender, $certificate, $chain, $sslPolicyErrors)\n")
	b.WriteString("  if ($null -eq $certificate) { return $false }\n")
	b.WriteString("  $sha = [System.Security.Cryptography.SHA256]::Create()\n")
	b.WriteString("  try {\n")
	b.WriteString("    $bytes = $sha.ComputeHash($certificate.RawData)\n")
	b.WriteString("    $hex = [System.BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()\n")
	b.WriteString("    return ($hex -eq $fingerprint)\n")
	b.WriteString("  } finally { $sha.Dispose() }\n")
	b.WriteString("}\n")
	b.WriteString("$client = [System.Net.Http.HttpClient]::new($handler)\n")
	b.WriteString("$response = $null\n")
	b.WriteString("try {\n")
	b.WriteString("  $client.DefaultRequestHeaders.Add('X-Timingdex-Pairing-Token', $pairingToken)\n")
	b.WriteString("  $response = $client.GetAsync($binaryUrl).GetAwaiter().GetResult()\n")
	b.WriteString("  if (-not $response.IsSuccessStatusCode) { throw ('download failed with HTTP ' + [int]$response.StatusCode) }\n")
	b.WriteString("  $bytes = $response.Content.ReadAsByteArrayAsync().GetAwaiter().GetResult()\n")
	b.WriteString("  [System.IO.File]::WriteAllBytes($binaryTmp, $bytes)\n")
	b.WriteString("} catch {\n")
	b.WriteString("  if (Test-Path -Path $binaryTmp) { Remove-Item -Force -Path $binaryTmp }\n")
	b.WriteString("  throw\n")
	b.WriteString("} finally {\n")
	b.WriteString("  if ($null -ne $response) { $response.Dispose() }\n")
	b.WriteString("  $client.Dispose()\n")
	b.WriteString("}\n")
	b.WriteString("$actual = (Get-FileHash -Path $binaryTmp -Algorithm SHA256).Hash.ToLowerInvariant()\n")
	b.WriteString("if ($actual -ne $binaryDigest) {\n")
	b.WriteString("  Remove-Item -Force -Path $binaryTmp\n")
	b.WriteString("  throw 'downloaded binary checksum does not match the Hub-provided digest'\n")
	b.WriteString("}\n")
	b.WriteString("Move-Item -Force -Path $binaryTmp -Destination $binaryOut\n")
	b.WriteString("\n")
	b.WriteString("Write-Host 'Enrolling Worker...'\n")

	b.WriteString("& ")
	b.WriteString(quotePowerShell("./" + filename))
	b.WriteString(" worker enroll")
	writePSArg(&b, "--hub", hubURL)
	writePSArg(&b, "--fingerprint", fingerprint)
	writePSArg(&b, "--pairing", pairingToken)
	if strings.TrimSpace(name) != "" {
		writePSArg(&b, "--name", strings.TrimSpace(name))
	}
	for _, m := range mounts {
		writePSArg(&b, "--mount", m.RootID+"="+m.Path)
	}
	if strings.TrimSpace(cacheDir) != "" {
		writePSArg(&b, "--cache", strings.TrimSpace(cacheDir))
	}
	writePSArg(&b, "--config", configPath)
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString("Write-Host ''\n")
	b.WriteString("Write-Host 'Enrolled successfully. Run the Worker with:'\n")
	b.WriteString("Write-Host ")
	b.WriteString(quotePowerShell("  .\\" + filename + " worker run --config " + configPath))
	b.WriteString("\n")
	b.WriteString("Write-Host ")
	b.WriteString(quotePowerShell("  .\\" + filename + " worker doctor --config " + configPath))
	b.WriteString("\n")

	return b.String()
}

func writePSArg(b *strings.Builder, flag, value string) {
	b.WriteString(" `\n")
	b.WriteString("  ")
	b.WriteString(flag)
	b.WriteString(" ")
	b.WriteString(quotePowerShell(value))
}

func (s *Server) writeWorkerSetupScript(w http.ResponseWriter, script string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func quotePowerShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

const workerSetupPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Timingdex · [[i18n:workerSetup.title]]</title><style>

.step-panel{display:none}
.step-panel.active{display:block}
.panel{padding:20px;margin-bottom:16px}
.panel h2{margin:0 0 14px}
.field{margin:12px 0}
.row{display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.binary-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:10px;margin:10px 0}
.binary-cell{padding:12px;text-align:center}
.binary-grid .binary-cell{margin-bottom:0}
.binary-cell .avail{color:var(--ev-confirmed);font-weight:800}
.binary-cell .unavail{color:var(--ev-contradicted)}
.binary-cell .size{color:var(--text-muted);font-size:12px}
.fingerprint{font-family:var(--font-data);font-size:13px;color:var(--text-muted);word-break:break-all;background:var(--inset);border-radius:8px;padding:8px;margin:6px 0}
.mount-row{display:grid;grid-template-columns:1fr 1fr;gap:8px;align-items:end;margin:8px 0;padding:8px;background:var(--inset);border-radius:8px}
.mount-row .path{font-size:13px;color:var(--text-muted);margin-bottom:4px}
.script-box{background:var(--inset);border:1px solid var(--rule-strong);border-radius:10px;padding:12px;max-height:420px;overflow:auto;white-space:pre-wrap;font-family:var(--font-data);font-size:13px;line-height:1.5;color:var(--text-muted);margin:10px 0}
.center{text-align:center;margin:18px 0}
.step-actions{display:flex;gap:10px;margin-top:16px}
.tls-on{color:var(--ev-confirmed)}
.tls-off{color:var(--ev-contradicted)}
.back-row{margin-bottom:12px}
@media(max-width:600px){.binary-grid{grid-template-columns:1fr}
.mount-row{grid-template-columns:1fr}
}

</style></head><body data-worker-setup-wizard>
<!--SHELL_HEADER-->
<div class="wrap"><div class="back-row"><a class="btn btn--ghost" href="/workers">← [[i18n:common.back]]</a></div>
<nav class="steps" id="step-nav" aria-label="[[i18n:workerSetup.wizardNavLabel]]"><span class="is-active" data-step="1" aria-current="step">1. [[i18n:workerSetup.environmentOverview]]</span><span data-step="2">2. [[i18n:workerSetup.configureNode]]</span><span data-step="3">3. [[i18n:workerSetup.generateScript]]</span><span data-step="4">4. [[i18n:workerSetup.startNode]]</span></nav>

<div class="panel step-panel active" id="step-1" role="group" aria-label="[[i18n:workerSetup.environmentOverview]]">
<div class="panel"><h2>[[i18n:workerSetup.hubConnectionInfo]]</h2><div id="hub-info" role="status" aria-live="polite"><div class="muted">[[i18n:workerSetup.loadingEnvironment]]</div></div></div>
<div class="panel"><h2>[[i18n:workerSetup.binaries]]</h2><p class="muted">[[i18n:workerSetup.binariesHint]]</p><div class="binary-grid" id="binary-grid" role="status" aria-live="polite"></div><div class="hint">💡 [[i18n:workerSetup.crossCompileLead]]<code>GOOS=windows GOARCH=amd64 go build -o timingdex-windows-amd64.exe ./cmd/timingdex</code><br>[[i18n:workerSetup.placeFilesLead]]<code>&lt;DataDir&gt;/worker-binaries/</code>[[i18n:workerSetup.placeFilesTrail]]</div></div>
<div class="step-actions"><button class="btn btn--primary" onclick="goStep(2)">[[i18n:common.next]] →</button></div>
</div>

<div class="panel step-panel" id="step-2" role="group" aria-label="[[i18n:workerSetup.configureNode]]">
<div class="panel"><h2>[[i18n:workerSetup.configuration]]</h2>
<div class="field"><label for="platform">[[i18n:workerSetup.targetPlatform]]</label><select id="platform"><option value="linux-amd64">Linux AMD64</option><option value="linux-arm64">Linux ARM64</option><option value="windows-amd64">Windows AMD64</option></select></div>
<div class="field"><label for="worker-name">[[i18n:workerSetup.workerNameOptional]]</label><input id="worker-name" type="text" placeholder="[[i18n:workerSetup.workerNamePlaceholder]]"></div>
<div class="field"><label for="cache-dir">[[i18n:workerSetup.cacheDirOptional]]</label><input id="cache-dir" type="text" placeholder="[[i18n:workerSetup.cacheDirPlaceholder]]"></div>
</div>
<div class="panel"><h2>[[i18n:workerSetup.mountMapping]]</h2><p class="muted">[[i18n:workerSetup.mountMappingHint]]</p><div id="mounts-panel" role="status" aria-live="polite"></div></div>
<div class="step-actions"><button class="btn btn--ghost" onclick="goStep(1)">← [[i18n:workerSetup.previous]]</button><button class="btn btn--primary" onclick="goStep(3)">[[i18n:common.next]] →</button></div>
</div>

<div class="panel step-panel" id="step-3" role="group" aria-label="[[i18n:workerSetup.generateScript]]">
<div class="panel"><h2>[[i18n:workerSetup.generateScript]]</h2>
<p class="muted">[[i18n:workerSetup.scriptWarning]]</p>
<div id="script-area" role="status" aria-live="polite"><div class="center"><button class="primary" onclick="generateScript()" id="gen-btn">[[i18n:workerSetup.generateButton]]</button></div></div>
</div>
<div class="step-actions"><button class="btn btn--ghost" onclick="goStep(2)">← [[i18n:workerSetup.previous]]</button></div>
</div>

<div class="panel step-panel" id="step-4" role="group" aria-label="[[i18n:workerSetup.startNode]]">
<div class="panel"><h2>[[i18n:workerSetup.startNode]]</h2>
<p class="muted">[[i18n:workerSetup.runCommandHint]]</p>
<div class="script-box">timingdex worker run --config ./.timingdex-worker/worker.json</div>
<p class="muted">[[i18n:workerSetup.doctorHintLead]]<code>timingdex worker doctor --config ./.timingdex-worker/worker.json</code>[[i18n:workerSetup.doctorHintTrail]]</p>
<p class="muted">[[i18n:workerSetup.workersStatusHintLead]]<a href="/workers">[[i18n:workers.title]]</a>[[i18n:workerSetup.workersStatusHintTrail]]</p>
</div>
<div class="step-actions">
<a class="btn btn--primary" href="/workers">[[i18n:common.back]]</a>
</div>
</div>
</div>

<script>
const esc=function(v){return String(v??'').replace(/[&<>"']/g,function(c){return {'&':'&amp;','>':'&gt;','<':'&lt;','"':'&quot;',"'":'&#39;'}[c]})};
var ctx=null,pairingToken=null;
 function csrfToken(){var prefix='__Host-timingdex_csrf=';var item=document.cookie.split('; ').find(function(x){return x.indexOf(prefix)===0});return item?decodeURIComponent(item.slice(prefix.length)):''}
  function authHeaders(base){var headers=new Headers(base||{});var csrf=csrfToken();if(csrf)headers.set('X-CSRF-Token',csrf);return headers}
async function json(url,opt){opt=opt||{};var r=await fetch(url,{...opt,headers:authHeaders(opt.headers)});if(!r.ok)throw new Error(await tdApiErrorMessage(r));return r.json()}
function goStep(n){for(var i=1;i<=4;i++){document.getElementById('step-'+i).className='panel step-panel'+(i===n?' active':'');var marker=document.querySelectorAll('.steps span')[i-1];marker.className=(i===n?'is-active':(i<n?'is-done':''));if(i===n){marker.setAttribute('aria-current','step')}else{marker.removeAttribute('aria-current')}}}
function renderBinaries(){var html=[],bins=ctx.available_binaries||{},order=['linux-amd64','linux-arm64','windows-amd64'];for(var i=0;i<order.length;i++){var key=order[i],bin=bins[key]||{},name=key,exists=!!bin.exists,size=bin.size_bytes||0;html.push('<div class="panel binary-cell"><div class="'+(exists?'avail':'unavail')+'">'+(exists?'\u2713 '+tdT('workerSetup.binaryAvailable'):'&times; '+tdT('workerSetup.binaryUnavailable'))+'</div><div>'+esc(name)+'</div><div class="size">'+(exists?formatSize(size):tdT('workerSetup.binaryNotUploaded'))+'</div></div>')}document.getElementById('binary-grid').innerHTML=html.join('')}
function formatSize(b){if(b<1024)return b+' B';if(b<1048576)return(b/1024).toFixed(1)+' KB';return(b/1048576).toFixed(1)+' MB'}
 function renderMounts(){var panel=document.getElementById('mounts-panel'),roots=ctx.library_roots||[];if(!roots.length){panel.innerHTML='<div class="muted">'+esc(tdT('workerSetup.noRoots'))+'</div>';return}var html=[];for(var i=0;i<roots.length;i++){var r=roots[i],label=r.path?esc(r.path):esc(tdT('workerSetup.pathRequiresAdmin'));html.push('<div class="mount-row"><div><div class="path">'+esc(r.id)+'<br>'+label+'</div></div><div><label style="font-size:12px;color:var(--text-muted);font-weight:800;display:block;margin-bottom:3px">'+esc(tdT('workerSetup.localPath'))+'</label><input aria-label="'+esc(tdT('workerSetup.localPath'))+'" class="mount-path" data-root-id="'+esc(r.id)+'" type="text" placeholder="/mnt/footage/'+esc(r.id)+'"></div></div>')}panel.innerHTML=html.join('')}
async function loadContext(){try{var contextResponse=await fetch('/api/v1/hub/worker-setup/context',{credentials:'same-origin'});if(!contextResponse.ok)throw new Error(await tdApiErrorMessage(contextResponse));ctx=await contextResponse.json();var detailWarning='';try{var rootsResponse=await fetch('/api/v1/admin/hub/worker-setup/library-roots',{credentials:'same-origin',headers:authHeaders()});if(!rootsResponse.ok)throw new Error('admin detail request failed');var details=await rootsResponse.json();ctx.library_roots=details.library_roots||[]}catch(_){detailWarning='<div class="muted">'+esc(tdT('workerSetup.adminDetailsFailed'))+'</div>'}var hubInfo=document.getElementById('hub-info');hubInfo.innerHTML='<div class="kv"><span class="k">'+esc(tdT('workerSetup.hubUrl'))+'</span><span class="v">'+esc(ctx.hub_url)+'</span></div><div class="kv"><span class="k">'+esc(tdT('workerSetup.tls'))+'</span><span class="v '+(ctx.tls?'tls-on':'tls-off')+'">'+(ctx.tls?tdT('workerSetup.tlsEnabled'):tdT('workerSetup.tlsNotEnabled'))+'</span></div>'+(ctx.fingerprint?'<div class="kv"><span class="k">'+esc(tdT('workerSetup.fingerprint'))+'</span><span class="fingerprint">'+esc(ctx.fingerprint)+'</span></div>':'')+'<div class="kv"><span class="k">'+esc(tdT('workerSetup.mediaFolders'))+'</span><span class="v">'+esc(tdPlural('workerSetup.rootCount',(ctx.library_roots||[]).length))+'</span></div>'+detailWarning;renderBinaries();renderMounts()}catch(e){document.getElementById('hub-info').innerHTML='<div class="muted">'+esc(tdT('workerSetup.environmentLoadError',{message:e.message}))+'</div>'}}
  // A successful admin login is what lets an off-trusted-network operator read
  // the admin-only library root details the first (pre-login) context load
  // could not fetch. Reload the whole context then, so the URL, fingerprint,
  // binaries and paths all agree instead of stitching a partial view together.
  window.addEventListener('timingdex:admin-auth-changed',function(event){if(event.detail&&event.detail.authenticated){loadContext()}else{ctx=ctx||{};ctx.library_roots=(ctx.library_roots||[]).map(function(root){return{id:root.id}});renderMounts()}});
  async function init(){return loadContext()}
 async function generateScript(){var btn=document.getElementById('gen-btn');btn.disabled=true;btn.textContent=tdT('workerSetup.generating');try{var platform=document.getElementById('platform').value;var bins=ctx&&ctx.available_binaries||{};var bin=bins[platform]||{};if(!bin.exists)throw new Error(tdT('workerSetup.binaryRequired'));if(!ctx||!ctx.tls||!ctx.fingerprint||!ctx.spki_pin)throw new Error(tdT('workerSetup.tlsRequired'));var pairing=await json('/api/v1/hub/worker-pairings',{method:'POST'});pairingToken=pairing.token;var name=document.getElementById('worker-name').value.trim();var cacheDir=document.getElementById('cache-dir').value.trim();var mountInputs=document.querySelectorAll('.mount-path');var mounts=[];for(var i=0;i<mountInputs.length;i++){var path=mountInputs[i].value.trim();if(path){mounts.push({root_id:mountInputs[i].getAttribute('data-root-id'),path:path})}}var body=JSON.stringify({platform:platform,name:name,pairing_token:pairingToken,mounts:mounts,cache_dir:cacheDir});var script=await fetch('/api/v1/hub/worker-setup/script',{method:'POST',headers:authHeaders({'Content-Type':'application/json'}),body:body});if(!script.ok)throw new Error(await tdApiErrorMessage(script));var scriptText=await script.text();document.getElementById('script-area').innerHTML='<button class="btn" onclick="copyScript()" id="copy-btn">'+esc(tdT('workerSetup.copyScript'))+'</button><div class="script-box" id="script-output">'+esc(scriptText)+'</div><div class="hint">'+esc(tdT('workerSetup.scriptOneTimeNote'))+'</div><div class="step-actions"><button class="btn btn--primary" onclick="goStep(4)">'+esc(tdT('workerSetup.viewStartupGuide'))+'</button></div>'}catch(e){document.getElementById('script-area').innerHTML='<div class="callout callout--contradicted" role="alert"><span>'+esc(tdT('workerSetup.generateFailed',{message:e.message}))+'</span></div><div class="center"><button class="primary" onclick="generateScript()">'+esc(tdT('common.retry'))+'</button></div>'}finally{btn.disabled=false}}
function copyScript(){var el=document.getElementById('script-output');if(!el)return;var range=document.createRange();range.selectNode(el);window.getSelection().removeAllRanges();window.getSelection().addRange(range);try{document.execCommand('copy');var btn=document.getElementById('copy-btn');btn.textContent=tdT('common.copied')}catch(e){}}init();
</script></body></html>`
