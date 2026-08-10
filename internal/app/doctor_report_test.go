package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/search"
)

// newDoctorHub builds a Hub on a real migrated SQLite database in a tempdir,
// because a DoctorReport is only as trustworthy as the schema it reads: the
// counts, the fts_index_state flag and the migration ledger are all facts
// only real SQL can produce (see CLAUDE.md's note on in-memory fakes).
func newDoctorHub(t *testing.T) (*Service, *sqlite.Repository, config.Config) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DataDir:      dataDir,
		CacheDir:     filepath.Join(dataDir, "cache"),
		DatabasePath: filepath.Join(dataDir, "timingdex.db"),
		Hardware:     media.HardwareConfig{Mode: "software", AllowFallback: true},
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, err := sqlite.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo, cfg
}

func TestDoctorReportFreshHub(t *testing.T) {
	service, _, cfg := newDoctorHub(t)
	ctx := context.Background()

	report, err := service.DoctorReport(ctx)
	if err != nil {
		t.Fatalf("DoctorReport on a fresh hub: %v", err)
	}
	if !report.DB.IntegrityOK {
		t.Fatal("fresh hub must pass the SQLite integrity check")
	}
	if report.DB.MigrationsApplied == 0 || report.DB.MigrationsApplied != report.DB.MigrationsTotal {
		t.Fatalf("fresh hub migrations applied=%d total=%d must be equal and non-zero", report.DB.MigrationsApplied, report.DB.MigrationsTotal)
	}
	if report.DB.SchemaVersion == "" {
		t.Fatal("fresh hub must report a schema version (last applied migration)")
	}
	if report.DB.Path != cfg.DatabasePath {
		t.Fatalf("db path = %q, want %q", report.DB.Path, cfg.DatabasePath)
	}
	if len(report.Roots) != 0 {
		t.Fatalf("fresh hub roots = %d, want 0", len(report.Roots))
	}
	if len(report.Workers) != 0 {
		t.Fatalf("fresh hub workers = %d, want 0", len(report.Workers))
	}
	if report.SearchIndex.ShotCount != 0 || report.SearchIndex.EmbeddingsCount != 0 || report.SearchIndex.EmbeddingsShotCount != 0 {
		t.Fatalf("fresh hub search index = %+v, want empty", report.SearchIndex)
	}
	if !report.SearchIndex.FTSReady {
		t.Fatalf("fresh hub FTS must be ready after Migrate, got %+v", report.SearchIndex)
	}
	if report.Queue != (domain.QueueInfo{}) {
		t.Fatalf("fresh hub queue = %+v, want all zeros", report.Queue)
	}
	if report.System.Hostname == "" || report.System.GoOS == "" || report.System.GoArch == "" {
		t.Fatalf("system section incomplete: %+v", report.System)
	}
	if report.Storage.CachePath != cfg.CacheDir {
		t.Fatalf("cache path = %q, want %q", report.Storage.CachePath, cfg.CacheDir)
	}
	if report.Storage.FreeDiskBytes < 0 {
		t.Fatalf("free disk = %d, want a measured value (tempdirs live on a real volume)", report.Storage.FreeDiskBytes)
	}
	if report.GPU.HardwareReport == "" {
		t.Fatal("GPU section must carry the rendered hardware report")
	}
}

func TestDoctorReportSeededLibrary(t *testing.T) {
	service, repo, _ := newDoctorHub(t)
	ctx := context.Background()

	rootDir := t.TempDir()
	if _, err := repo.CreateLibraryRoot(ctx, rootDir); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-seeded','fp-seeded',100,'discovered','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-seeded", "", []domain.AssetShot{
		{ID: "shot-a", AssetID: "asset-seeded", Ordinal: 0, StartMS: 0, EndMS: 1000, Description: "red car"},
		{ID: "shot-b", AssetID: "asset-seeded", Ordinal: 1, StartMS: 1000, EndMS: 2000, Description: "dog park"},
	}, "", ""); err != nil {
		t.Fatal(err)
	}
	// Embed only one of the two shots, so the distinct-shot count can be
	// told apart from the vector-row count.
	if err := repo.UpsertShotTextEmbeddings(ctx, []search.ShotEmbeddingRow{
		{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "shot-a"}}, Model: "test-embed", Vector: []float32{0.1, 0.2}, SourceTextHash: "hash-a"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-seeded", domain.JobProbe, "probe-input", 90); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-seeded", domain.JobDerive, "derive-input", 90); err != nil {
		t.Fatal(err)
	}

	report, err := service.DoctorReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Roots) != 1 {
		t.Fatalf("roots = %d, want 1", len(report.Roots))
	}
	if report.Roots[0].Path != rootDir {
		t.Fatalf("root path = %q, want %q", report.Roots[0].Path, rootDir)
	}
	if report.Roots[0].LastHealthyAt != nil {
		t.Fatalf("unscanned root must have no last-healthy stamp, got %v", report.Roots[0].LastHealthyAt)
	}
	if report.SearchIndex.ShotCount != 2 {
		t.Fatalf("shot count = %d, want 2", report.SearchIndex.ShotCount)
	}
	if report.SearchIndex.EmbeddingsCount != 1 || report.SearchIndex.EmbeddingsShotCount != 1 {
		t.Fatalf("embeddings = %+v, want 1 row / 1 distinct shot", report.SearchIndex)
	}
	if report.Queue.Pending != 2 {
		t.Fatalf("pending = %d, want 2 (both enqueued jobs)", report.Queue.Pending)
	}
}

// TestDoctorReportContainsNoSecretMaterial proves the report is safe to ship
// by construction: a channel whose member holds a real secret must still
// marshal to JSON without the key, without a secret reference, and without a
// secret-bearing field name.
func TestDoctorReportContainsNoSecretMaterial(t *testing.T) {
	service, _, _ := newDoctorHub(t)
	ctx := context.Background()

	const secret = "top-secret-provider-key-9f2b"
	channel := domain.ProviderChannel{
		ID:           "channel-doctor",
		Capability:   "video_analysis",
		Label:        "Doctor channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{ID: "member-doctor", Label: "primary", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}
	if _, err := service.SaveProviderChannel(ctx, channel, []string{secret}); err != nil {
		t.Fatal(err)
	}

	report, err := service.DoctorReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Providers.Channels != 1 {
		t.Fatalf("channels = %d, want 1", report.Providers.Channels)
	}
	if report.Providers.MembersWithSecrets != 1 {
		t.Fatalf("members with secrets = %d, want 1", report.Providers.MembersWithSecrets)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "provider-channel/", "secret_ref", "secret_ref,omitempty"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("marshaled report must not contain %q: %s", forbidden, encoded)
		}
	}
}

func TestDoctorPrintsSectionedOutput(t *testing.T) {
	service, _, _ := newDoctorHub(t)
	var out strings.Builder
	if err := service.Doctor(context.Background(), &out); err != nil {
		t.Fatalf("Doctor on a fresh hub must exit 0, got %v", err)
	}
	for _, section := range []string{"SYSTEM", "DB", "STORAGE", "MEDIA ROOTS", "FFMPEG", "FFPROBE", "EXIFTOOL", "GPU", "PROVIDERS", "SEARCH INDEX", "WORKERS", "QUEUE", "TEMP FILES"} {
		if !strings.Contains(out.String(), section) {
			t.Errorf("doctor output missing section %q", section)
		}
	}
	if !strings.Contains(out.String(), "✓ integrity ok") {
		t.Errorf("doctor output must show a passing integrity check:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no library roots configured") {
		t.Errorf("doctor output must report an empty root list:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no workers enrolled") {
		t.Errorf("doctor output must report an empty fleet:\n%s", out.String())
	}
	// The whole output must be mark-annotated: every diagnosis line starts
	// with one of the three marks. Section headers and the embedded hardware
	// report (the lines between GPU and the next header) are structural, not
	// verdicts, so they are exempt.
	headers := map[string]bool{}
	for _, section := range []string{"SYSTEM", "DB", "STORAGE", "MEDIA ROOTS", "FFMPEG", "FFPROBE", "EXIFTOOL", "GPU", "PROVIDERS", "SEARCH INDEX", "WORKERS", "QUEUE", "TEMP FILES"} {
		headers[section] = true
	}
	inGPU := false
	for _, line := range strings.Split(out.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		if headers[trimmed] {
			inGPU = trimmed == "GPU"
			continue
		}
		if inGPU || trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "✓") && !strings.HasPrefix(trimmed, "⚠") && !strings.HasPrefix(trimmed, "✗") {
			t.Errorf("doctor line missing a mark: %q", line)
		}
	}
}

// TestDoctorReportRecordsStaleScratchFiles pins the temp-file census: a
// .partial file younger than 24h is an active copy, an older one is debris.
func TestDoctorReportRecordsStaleScratchFiles(t *testing.T) {
	service, _, cfg := newDoctorHub(t)
	ctx := context.Background()

	live := filepath.Join(cfg.CacheDir, "asset-1", "v1-abc.partial")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("live copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cfg.CacheDir, "asset-2", "v1-dead.partial")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("abandoned copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	report, err := service.DoctorReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.TempFiles.Files != 1 {
		t.Fatalf("stale scratch files = %d, want 1 (only the 48h-old .partial)", report.TempFiles.Files)
	}
	if report.TempFiles.Bytes != int64(len("abandoned copy")) {
		t.Fatalf("stale scratch bytes = %d, want %d", report.TempFiles.Bytes, len("abandoned copy"))
	}
	if report.Storage.CacheBytes == 0 {
		t.Fatal("cache bytes must count the two seeded files")
	}
}
