// Package support assembles the `timingdex support bundle`: one zip of Hub
// diagnostics — the structured doctor report, the queue summary, worker
// versions, recent failure codes, the sanitized configuration and the
// human-readable doctor text — meant to be handed to an operator or upstream
// support.
//
// Safety is one-sided: anything that is not provably diagnostic is excluded.
// The bundle never reads provider-secrets/ and never contains API keys, admin
// or agent tokens, provider secrets, source media, transcript content, raw
// model responses, or absolute source paths. Every secret-bearing
// configuration value is replaced by "***", and every absolute path —
// configuration fields and any absolute path appearing inside the report or
// the doctor text — is reduced to its basename before anything reaches the
// archive.
package support

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
)

const (
	maxBundleEntryBytes    = 4 << 20
	maxBundleTotalBytes    = 8 << 20
	bundleTruncationMarker = "\n[TRUNCATED: support bundle limit reached]\n"
)

// Bundle is the digest written as bundle.json inside the archive. The full
// detail lives in Report (itself path-sanitized); the top-level fields are
// the one-glance summary an operator sees before opening the report.
type Bundle struct {
	Report           domain.DoctorReport `json:"report"`
	Version          string              `json:"version"`
	OS               string              `json:"os"`
	Arch             string              `json:"arch"`
	FFmpegVersion    string              `json:"ffmpeg_version,omitempty"`
	ConfigSanitized  map[string]any      `json:"config_sanitized"`
	SchemaVersion    string              `json:"schema_version"`
	Queue            domain.JobSummary   `json:"queue"`
	WorkerVersions   []string            `json:"worker_versions,omitempty"`
	RecentErrorCodes []string            `json:"recent_error_codes,omitempty"`
	GeneratedAt      time.Time           `json:"generated_at"`
}

// BundleSource is the slice of the Hub service the bundle reads by itself. It
// is declared here, at the consumer — the same convention the app layer uses
// for its repository interfaces — so tests can drive Generate with a fake
// instead of a live database.
type BundleSource interface {
	// Doctor renders the human-readable doctor text, included in the archive
	// after path sanitization.
	Doctor(context.Context, io.Writer) error
	// JobSummary is the whole-queue count. JobIssues is the failure backlog
	// grouped by category — the distinct last_error_code values currently
	// present in the queue, deduped by the database.
	JobSummary(context.Context) (domain.JobSummary, error)
	JobIssues(context.Context) ([]domain.JobIssue, error)
}

// Generate assembles the support bundle for report and cfg and writes it as a
// zip to outPath. outPath is explicit: the caller (the CLI command) owns the
// default naming. The archive contains exactly three entries: bundle.json,
// doctor.txt and config.sanitized.json.
func Generate(ctx context.Context, report domain.DoctorReport, cfg config.Config, source BundleSource, outPath string) error {
	summary, err := source.JobSummary(ctx)
	if err != nil {
		return fmt.Errorf("read queue summary: %w", err)
	}
	issues, err := source.JobIssues(ctx)
	if err != nil {
		return fmt.Errorf("read failure categories: %w", err)
	}
	cfgSanitized, err := SanitizeConfig(cfg)
	if err != nil {
		return fmt.Errorf("sanitize config: %w", err)
	}

	var doctorText limitedBuffer
	if err := source.Doctor(ctx, &doctorText); err != nil {
		return fmt.Errorf("run doctor: %w", err)
	}

	bundle := Bundle{
		Report:           sanitizeReportPaths(report),
		Version:          report.System.Version,
		OS:               report.System.GoOS,
		Arch:             report.System.GoArch,
		FFmpegVersion:    report.FFmpeg.Version,
		ConfigSanitized:  cfgSanitized,
		SchemaVersion:    report.DB.SchemaVersion,
		Queue:            summary,
		WorkerVersions:   distinctSortedWorkerVersions(report.Workers),
		RecentErrorCodes: recentErrorCodes(issues),
		GeneratedAt:      time.Now().UTC(),
	}
	bundleJSON, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bundle: %w", err)
	}
	configJSON, err := json.MarshalIndent(cfgSanitized, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sanitized config: %w", err)
	}
	doctor := SanitizePathText(doctorText.String())
	if doctorText.truncated {
		doctor += bundleTruncationMarker
	}
	return writeZip(outPath, []zipEntry{
		{name: "bundle.json", data: bundleJSON},
		{name: "doctor.txt", data: boundEntry([]byte(doctor))},
		{name: "config.sanitized.json", data: boundEntry(configJSON)},
	})
}

// recentErrorCodes flattens the failure backlog into its category codes — the
// last_error_code values the database aggregated, deduped — capped at 20 and
// sorted so the archive is byte-stable across runs.
func recentErrorCodes(issues []domain.JobIssue) []string {
	seen := make(map[string]bool, len(issues))
	var codes []string
	for _, issue := range issues {
		code := string(issue.Category)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		codes = append(codes, code)
		if len(codes) >= 20 {
			break
		}
	}
	sort.Strings(codes)
	return codes
}

// distinctSortedWorkerVersions is the fleet's reported binary versions
// (remote.WorkerRegistration.Version), deduped and sorted.
func distinctSortedWorkerVersions(workers []domain.WorkerInfo) []string {
	seen := make(map[string]bool, len(workers))
	var versions []string
	for _, worker := range workers {
		version := strings.TrimSpace(worker.Version)
		if version == "" || seen[version] {
			continue
		}
		seen[version] = true
		versions = append(versions, version)
	}
	sort.Strings(versions)
	return versions
}

// sanitizeReportPaths returns a copy of the report whose absolute paths have
// been reduced to basenames. The copy is made through a JSON round-trip so
// the walk cannot miss a field M1a or a future track adds to the report:
// every string value is run through SanitizePathText, which strips absolute
// paths out of whole-value paths and prose alike.
func sanitizeReportPaths(report domain.DoctorReport) domain.DoctorReport {
	raw, err := json.Marshal(report)
	if err != nil {
		return report
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return report
	}
	sanitizeReportValue(tree)
	sanitized, err := json.Marshal(tree)
	if err != nil {
		return report
	}
	var out domain.DoctorReport
	if err := json.Unmarshal(sanitized, &out); err != nil {
		return report
	}
	return out
}

func sanitizeReportValue(node any) {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if text, ok := child.(string); ok {
				v[key] = SanitizePathText(text)
				continue
			}
			sanitizeReportValue(child)
		}
	case []any:
		for i := range v {
			sanitizeReportValue(v[i])
		}
	}
}

// sensitiveConfigKey matches any configuration key that can carry a secret:
// key material, tokens, passwords, credentials, auth headers, and the
// extra_headers map whose values can hold a key in header form. api_key_env
// matches through "key" and is redacted too: it holds an environment
// variable name rather than a secret, but the name is not diagnostic enough
// to justify keeping a key-looking key in the archive. Losing a key name is
// safe; keeping a value is not, so the line is drawn on the safe side.
var sensitiveConfigKey = regexp.MustCompile(`(?i)key|secret|token|password|credential|auth|header`)

// pathConfigKey names the configuration fields that hold absolute paths.
// Values under these keys are reduced to their basename. hub_tls.key_file is
// deliberately absent: it matches sensitiveConfigKey and is redacted
// entirely, which is the right answer for a private-key file name.
// command fields are included because they point at Hub-box binaries
// (alignment, shot detection) whose install layout must not leave the box in
// a support archive.
var pathConfigKey = map[string]bool{
	"data_dir":         true,
	"cache_dir":        true,
	"database_path":    true,
	"certificate_file": true,
	"device":           true,
	"command":          true,
}

// SanitizeConfig renders the configuration as a JSON tree with every
// secret-bearing value replaced by "***" and every absolute path reduced to
// its basename. The tree is a copy: the caller's config is never mutated.
// provider-secrets/ is never read — provider-channel keys live in the
// encrypted store, and the legacy api_key values config.Load resolves from
// the environment are caught by the key match.
func SanitizeConfig(cfg config.Config) (map[string]any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		// A Config that cannot marshal cannot be sanitized by walking it;
		// fail the bundle rather than ship a half-censored config.
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	sanitizeConfigValue(tree)
	return tree, nil
}

func sanitizeConfigValue(node any) {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if sensitiveConfigKey.MatchString(key) {
				v[key] = "***"
				continue
			}
			if pathConfigKey[strings.ToLower(key)] {
				if text, ok := child.(string); ok && text != "" && filepath.IsAbs(text) {
					v[key] = filepath.Base(text)
				}
				continue
			}
			sanitizeConfigValue(child)
		}
	case []any:
		for i := range v {
			// A bare absolute path inside a list (e.g. an ffmpeg args array
			// carrying an input path) leaks the Hub box's layout the same way
			// a command field does. filepath.IsAbs keeps URLs out — nothing
			// with a scheme matches.
			if text, ok := v[i].(string); ok && filepath.IsAbs(text) {
				v[i] = filepath.Base(text)
				continue
			}
			sanitizeConfigValue(v[i])
		}
	}
}

// absPathToken matches a whole absolute path inside prose: a POSIX path
// (leading slash) or a Windows drive path (letter, colon, backslash). The
// match is anchored to start-of-text or a non-path character, which is what
// keeps URLs out: "https://…" has a colon before the slash, and a colon is
// never the start of a POSIX path token here. The path itself runs to the
// next whitespace or prose punctuation, so "/mnt/nas/root: cifs" keeps its
// colon.
var absPathToken = regexp.MustCompile(`(^|[\s(\[])(/[^ \t\n,;:)\]]+|[A-Za-z]:[\\/][^ \t\n,;)\]]+)`)

// SanitizePathText reduces every absolute path appearing in prose to its
// basename: "/mnt/nas/footage: cifs" becomes "footage: cifs" and "ffmpeg:
// /usr/bin/ffmpeg" becomes "ffmpeg: ffmpeg". Everything that is not an
// absolute path passes through untouched. The Windows-drive alternative is
// only ever rewritten on a host whose filepath splits backslashes, which is
// the only place such a path can exist; a Unix Hub leaves it alone.
func SanitizePathText(text string) string {
	return absPathToken.ReplaceAllStringFunc(text, func(match string) string {
		sub := absPathToken.FindStringSubmatch(match)
		prefix, path := sub[1], sub[2]
		if filepath.IsAbs(path) || isWindowsAbsolute(path) {
			return prefix + crossPlatformBase(path)
		}
		return match
	})
}

func isWindowsAbsolute(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}

func crossPlatformBase(value string) string {
	return filepath.Base(strings.ReplaceAll(value, "\\", "/"))
}

type limitedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := maxBundleEntryBytes - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func boundEntry(data []byte) []byte {
	if len(data) <= maxBundleEntryBytes {
		return data
	}
	limit := maxBundleEntryBytes - len(bundleTruncationMarker)
	return append(append([]byte(nil), data[:limit]...), []byte(bundleTruncationMarker)...)
}

type zipEntry struct {
	name string
	data []byte
}

// writeZip writes entries to a zip at outPath with 0600 permissions and a
// deterministic entry order (sorted by name) so two runs against the same
// state produce byte-identical archives.
func writeZip(outPath string, entries []zipEntry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var total int
	for i := range entries {
		entries[i].data = boundEntry(entries[i].data)
		total += len(entries[i].data)
	}
	if total > maxBundleTotalBytes {
		return fmt.Errorf("support bundle exceeds %d-byte entry limit", maxBundleTotalBytes)
	}
	file, err := os.CreateTemp(filepath.Dir(outPath), ".timingdex-support-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		w, err := writer.Create(entry.name)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("create %s in archive: %w", entry.name, err)
		}
		if _, err := w.Write(entry.data); err != nil {
			_ = file.Close()
			return fmt.Errorf("write %s in archive: %w", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return fmt.Errorf("finalize archive: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if info.Size() > maxBundleTotalBytes {
		return fmt.Errorf("support bundle exceeds %d-byte archive limit", maxBundleTotalBytes)
	}
	return os.Rename(tmpPath, outPath)
}
