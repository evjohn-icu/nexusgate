package support

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
)

// fakeBundleSource drives Generate without a live database. Doctor writes
// known prose containing absolute paths so the test can prove doctor.txt is
// sanitized too.
type fakeBundleSource struct {
	summary domain.JobSummary
	issues  []domain.JobIssue
}

func (f fakeBundleSource) JobSummary(context.Context) (domain.JobSummary, error) {
	return f.summary, nil
}
func (f fakeBundleSource) JobIssues(context.Context) ([]domain.JobIssue, error) {
	return f.issues, nil
}
func (f fakeBundleSource) Doctor(_ context.Context, writer io.Writer) error {
	fmt.Fprintf(writer, "ffmpeg: /usr/bin/ffmpeg\n")
	fmt.Fprintf(writer, "ffprobe: /usr/bin/ffprobe\n")
	fmt.Fprintf(writer, "library roots: 1\n")
	fmt.Fprintf(writer, "  /mnt/nas/footage: cifs (cifs)\n")
	fmt.Fprintf(writer, "sqlite: ok\n")
	return nil
}

// TestSupportBundleRedactsSecretsAndBasenames is the safety floor of the
// whole feature: a config that genuinely holds a key string in memory and a
// report full of absolute paths must produce an archive in which the key is
// absent from every file, the config shows "***" for every secret-bearing
// key, and every absolute path is a basename.
func TestSupportBundleRedactsSecretsAndBasenames(t *testing.T) {
	const fakeKey = "sk-test-super-secret-0123456789abcdef"
	const fakeHeaderValue = "header-value-secret-abcdef"

	cfg := config.Config{
		DataDir:       "/srv/timingdex",
		CacheDir:      "/srv/timingdex/cache",
		DatabasePath:  "/srv/timingdex/timingdex.db",
		ListenAddress: "127.0.0.1:8787",
		Providers: config.ProvidersConfig{
			StepFun: config.ProviderConfig{
				Enabled:      true,
				BaseURL:      "https://api.stepfun.com/step_plan/v1",
				Path:         "audio/asr/sse",
				APIKey:       fakeKey,
				APIKeyEnv:    "STEP_API_KEY",
				Model:        "stepaudio-2.5-asr",
				AuthHeader:   "Authorization",
				AuthScheme:   "Bearer",
				ExtraHeaders: map[string]string{"X-Proxy-Key": fakeHeaderValue},
			},
			Alignment: config.AlignmentConfig{
				Command: "/opt/timingdex/bin/timingdex-align",
				Args:    []string{"-i", "/mnt/nas/clips", `C:\Users\ev\clips\input.mp4`, "https://example.test/media/input.mp4"},
			},
			ShotDetection: config.ShotDetectionConfig{Args: []string{`D:\Footage\nested\cut.mp4`}},
		},
		HubTLS:   config.HubTLSConfig{Mode: "files", CertificateFile: "/etc/timingdex/tls.crt", KeyFile: "/etc/timingdex/tls.key"},
		Hardware: media.HardwareConfig{Mode: "auto", Device: "/dev/dri/renderD128", AllowFallback: true, ProxyBitrateKbps: 1800},
	}

	lastSeen := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	report := domain.DoctorReport{
		System:   domain.SystemInfo{Version: "v0.29.0-test", GoOS: "linux", GoArch: "amd64", Hostname: "nas-01"},
		DB:       domain.DBInfo{Path: "/srv/timingdex/timingdex.db", IntegrityOK: true, MigrationsApplied: 29, MigrationsTotal: 29, SchemaVersion: "0029_cost_ledger.sql"},
		Storage:  domain.StorageInfo{CachePath: "/srv/timingdex/cache", FreeDiskBytes: 1 << 40, CacheBytes: 1 << 30, DBSizeBytes: 1 << 20},
		Roots:    []domain.RootInfo{{Path: "/mnt/nas/footage", State: string(domain.RootHealthHealthy), FilesystemType: "cifs", LastHealthyAt: &lastSeen}},
		FFmpeg:   domain.FFmpegInfo{Path: "/usr/bin/ffmpeg", Present: true, Version: "ffmpeg version 6.1.1-3ubuntu2"},
		FFprobe:  domain.ToolInfo{Path: "/usr/bin/ffprobe", Present: true},
		ExifTool: domain.ToolInfo{Path: "/usr/bin/exiftool", Present: true},
		GPU:      domain.GPUInfo{HardwareReport: "hardware acceleration: requested=auto selected=software fallback=true"},
		Workers:  []domain.WorkerInfo{{Name: "worker-1", Status: "online", Version: "dev", LastSeenAt: &lastSeen}},
		Queue:    domain.QueueInfo{Pending: 3, Succeeded: 10, Failed: 1, Terminal: 1, Deferred: 0},
	}
	summary := domain.JobSummary{Pending: 3, Succeeded: 10, Failed: 1, Terminal: 1, Total: 15}
	issues := []domain.JobIssue{
		{Category: domain.JobFailureCategoryProviderQuota, Count: 2},
		{Category: domain.JobFailureCategoryProviderAuth, Count: 1},
	}

	out := filepath.Join(t.TempDir(), "bundle.zip")
	if err := Generate(context.Background(), report, cfg, fakeBundleSource{summary: summary, issues: issues}, out); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	files := readZip(t, out)

	for _, name := range []string{"bundle.json", "doctor.txt", "config.sanitized.json"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("archive missing %s; have %v", name, zipEntryNames(t, out))
		}
	}

	// The fake key, the header-carried key, the key's env-var name and the
	// TLS private-key file name must not appear anywhere in the archive.
	for name, data := range files {
		for _, secret := range []string{fakeKey, fakeHeaderValue, "STEP_API_KEY", "tls.key"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s leaks %q", name, secret)
			}
		}
	}

	var sanitized map[string]any
	if err := json.Unmarshal(files["config.sanitized.json"], &sanitized); err != nil {
		t.Fatalf("parse config.sanitized.json: %v", err)
	}
	stepfun := sanitized["providers"].(map[string]any)["stepfun"].(map[string]any)
	for _, key := range []string{"api_key", "api_key_env", "auth_header", "auth_scheme", "extra_headers"} {
		if got := stepfun[key]; got != "***" {
			t.Errorf("config.sanitized.json providers.stepfun.%s = %v, want ***", key, got)
		}
	}
	if got := sanitized["data_dir"]; got != "timingdex" {
		t.Errorf("config.sanitized.json data_dir = %v, want basename timingdex", got)
	}
	if got := sanitized["cache_dir"]; got != "cache" {
		t.Errorf("config.sanitized.json cache_dir = %v, want basename cache", got)
	}
	if got := sanitized["database_path"]; got != "timingdex.db" {
		t.Errorf("config.sanitized.json database_path = %v, want basename timingdex.db", got)
	}
	if got := stepfun["base_url"]; got != "https://api.stepfun.com/step_plan/v1" {
		t.Errorf("config.sanitized.json base_url = %v, want untouched URL", got)
	}
	// The alignment command and its args carry absolute Hub-box paths (the
	// install layout and a source input) — they must be basenames, or the
	// archive leaks where the box lives.
	align := sanitized["providers"].(map[string]any)["alignment"].(map[string]any)
	if got := align["command"]; got != "timingdex-align" {
		t.Errorf("config.sanitized.json alignment.command = %v, want basename timingdex-align", got)
	}
	if args, ok := align["args"].([]any); !ok || len(args) != 4 || args[0] != "-i" || args[1] != "clips" || args[2] != "input.mp4" || args[3] != "https://example.test/media/input.mp4" {
		t.Errorf("config.sanitized.json alignment.args = %v, want cross-platform basenames with URL preserved", args)
	}
	shot := sanitized["providers"].(map[string]any)["shot_detection"].(map[string]any)
	if args, ok := shot["args"].([]any); !ok || len(args) != 1 || args[0] != "cut.mp4" {
		t.Errorf("config.sanitized.json shot_detection.args = %v, want [cut.mp4]", args)
	}
	tls := sanitized["hub_tls"].(map[string]any)
	if got := tls["certificate_file"]; got != "tls.crt" {
		t.Errorf("config.sanitized.json hub_tls.certificate_file = %v, want basename tls.crt", got)
	}
	if got := tls["key_file"]; got != "***" {
		t.Errorf("config.sanitized.json hub_tls.key_file = %v, want ***", got)
	}

	var bundle Bundle
	if err := json.Unmarshal(files["bundle.json"], &bundle); err != nil {
		t.Fatalf("parse bundle.json: %v", err)
	}
	if got := bundle.Report.DB.Path; got != "timingdex.db" {
		t.Errorf("bundle report db.path = %q, want basename timingdex.db", got)
	}
	if got := bundle.Report.Storage.CachePath; got != "cache" {
		t.Errorf("bundle report storage.cache_path = %q, want basename cache", got)
	}
	if len(bundle.Report.Roots) != 1 || bundle.Report.Roots[0].Path != "footage" {
		t.Errorf("bundle report roots[0].path = %+v, want basename footage", bundle.Report.Roots)
	}
	if got := bundle.Report.FFmpeg.Path; got != "ffmpeg" {
		t.Errorf("bundle report ffmpeg.path = %q, want basename ffmpeg", got)
	}
	if got := bundle.Report.System.Hostname; got != "nas-01" {
		t.Errorf("bundle report hostname = %q, want untouched nas-01", got)
	}
	if got := bundle.Report.System.Version; got != "v0.29.0-test" {
		t.Errorf("bundle version = %q, want v0.29.0-test", got)
	}
	if got := bundle.FFmpegVersion; got != "ffmpeg version 6.1.1-3ubuntu2" {
		t.Errorf("bundle ffmpeg_version = %q, want the report's version line", got)
	}
	if got := bundle.SchemaVersion; got != "0029_cost_ledger.sql" {
		t.Errorf("bundle schema_version = %q, want 0029_cost_ledger.sql", got)
	}
	if got := bundle.OS; got != "linux" {
		t.Errorf("bundle os = %q, want linux", got)
	}
	if got := bundle.Arch; got != "amd64" {
		t.Errorf("bundle arch = %q, want amd64", got)
	}
	if bundle.Queue != summary {
		t.Errorf("bundle queue = %+v, want %+v", bundle.Queue, summary)
	}
	if len(bundle.WorkerVersions) != 1 || bundle.WorkerVersions[0] != "dev" {
		t.Errorf("bundle worker_versions = %v, want [dev]", bundle.WorkerVersions)
	}
	wantCodes := []string{string(domain.JobFailureCategoryProviderAuth), string(domain.JobFailureCategoryProviderQuota)}
	if strings.Join(bundle.RecentErrorCodes, ",") != strings.Join(wantCodes, ",") {
		t.Errorf("bundle recent_error_codes = %v, want %v", bundle.RecentErrorCodes, wantCodes)
	}
	if bundle.GeneratedAt.IsZero() {
		t.Error("bundle generated_at is zero")
	}

	doctor := string(files["doctor.txt"])
	if strings.Contains(doctor, "/usr/bin/") || strings.Contains(doctor, "/mnt/nas/") || strings.Contains(doctor, "/srv/") {
		t.Errorf("doctor.txt leaks absolute paths:\n%s", doctor)
	}
	if !strings.Contains(doctor, "ffmpeg: ffmpeg") {
		t.Errorf("doctor.txt missing basenamed ffmpeg line:\n%s", doctor)
	}
	if !strings.Contains(doctor, "footage: cifs (cifs)") {
		t.Errorf("doctor.txt missing basenamed root line:\n%s", doctor)
	}
}

// TestSupportBundleCapsRecentErrorCodesAtTwenty proves the archive stays
// bounded no matter how many distinct failure categories the queue has.
func TestSupportBundleCapsRecentErrorCodesAtTwenty(t *testing.T) {
	var issues []domain.JobIssue
	for i := 1; i <= 30; i++ {
		issues = append(issues, domain.JobIssue{Category: domain.JobFailureCategory(fmt.Sprintf("code-%02d", i)), Count: 1})
	}
	out := filepath.Join(t.TempDir(), "bundle.zip")
	if err := Generate(context.Background(), domain.DoctorReport{}, config.Config{}, fakeBundleSource{issues: issues}, out); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(readZip(t, out)["bundle.json"], &bundle); err != nil {
		t.Fatalf("parse bundle.json: %v", err)
	}
	if len(bundle.RecentErrorCodes) != 20 {
		t.Fatalf("recent_error_codes has %d entries, want 20", len(bundle.RecentErrorCodes))
	}
	if bundle.RecentErrorCodes[0] != "code-01" || bundle.RecentErrorCodes[19] != "code-20" {
		t.Errorf("recent_error_codes not sorted-and-capped: %v", bundle.RecentErrorCodes)
	}
}

type largeBundleSource struct{ doctor string }

func (largeBundleSource) JobSummary(context.Context) (domain.JobSummary, error) {
	return domain.JobSummary{}, nil
}
func (largeBundleSource) JobIssues(context.Context) ([]domain.JobIssue, error) { return nil, nil }
func (s largeBundleSource) Doctor(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, s.doctor)
	return err
}

func TestSupportBundleBoundsOversizedEntriesAndLeavesNoPartialOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "doctor.zip")
	if err := Generate(context.Background(), domain.DoctorReport{}, config.Config{}, largeBundleSource{doctor: strings.Repeat("x", maxBundleEntryBytes+1024)}, out); err != nil {
		t.Fatal(err)
	}
	files := readZip(t, out)
	if len(files["doctor.txt"]) > maxBundleEntryBytes || !strings.Contains(string(files["doctor.txt"]), "TRUNCATED") {
		t.Fatalf("doctor entry was not bounded with marker: %d bytes", len(files["doctor.txt"]))
	}
	out = filepath.Join(dir, "config.zip")
	cfg := config.Config{Providers: config.ProvidersConfig{Alignment: config.AlignmentConfig{Args: []string{strings.Repeat("x", maxBundleEntryBytes+1024)}}}}
	if err := Generate(context.Background(), domain.DoctorReport{}, cfg, largeBundleSource{}, out); err != nil {
		t.Fatal(err)
	}
	files = readZip(t, out)
	if len(files["config.sanitized.json"]) > maxBundleEntryBytes || !strings.Contains(string(files["config.sanitized.json"]), "TRUNCATED") {
		t.Fatalf("config entry was not bounded with marker: %d bytes", len(files["config.sanitized.json"]))
	}
	bad := filepath.Join(t.TempDir(), "missing", "bundle.zip")
	if err := Generate(context.Background(), domain.DoctorReport{}, config.Config{}, largeBundleSource{doctor: strings.Repeat("x", maxBundleEntryBytes*3)}, bad); err == nil {
		t.Fatal("expected failure creating archive in missing directory")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatalf("partial output exists after failed generation: %v", err)
	}
}

func TestSanitizePathTextIsCrossPlatformAndPreservesURLs(t *testing.T) {
	got := SanitizePathText(`file C:\Users\ev\footage\clip.mp4 URL https://example.test/a/b`)
	if strings.Contains(got, `C:\Users`) || !strings.Contains(got, "clip.mp4") || !strings.Contains(got, "https://example.test/a/b") {
		t.Fatalf("sanitized text = %q", got)
	}
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		t.Fatalf("read zip %s: %v", path, err)
	}
	out := make(map[string][]byte, len(reader.File))
	for _, entry := range reader.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", entry.Name, err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("read zip entry %s: %v", entry.Name, err)
		}
		out[entry.Name] = data
	}
	return out
}

func zipEntryNames(t *testing.T, path string) []string {
	t.Helper()
	files := readZip(t, path)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}
