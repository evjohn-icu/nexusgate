//go:build playwrightfixture

// Package browserfixture provides a deterministic, offline browser fixture.
package browserfixture

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/api"
	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/hubtls"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

const AdminToken = "pw-admin-fixture"

type Fixture struct {
	Repo           *sqlite.Repository
	Service        *app.Service
	Handler        http.Handler
	Dir            string
	TLSCertificate string
	TLSKey         string
	TLSFingerprint string
}

func New(ctx context.Context) (*Fixture, error) {
	dir, err := os.MkdirTemp("", "nexusgate-playwright-")
	if err != nil {
		return nil, err
	}
	keepDir := false
	defer func() {
		if !keepDir {
			_ = os.RemoveAll(dir)
		}
	}()
	repo, err := sqlite.Open(filepath.Join(dir, "nexusgate.db"))
	if err != nil {
		return nil, err
	}
	if err := repo.Migrate(ctx); err != nil {
		repo.Close()
		return nil, err
	}
	service, err := app.NewService(repo, config.Config{DataDir: dir, HubSecurity: config.HubSecurityConfig{AdminToken: AdminToken, AdminAuth: "required"}, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		repo.Close()
		return nil, err
	}
	if err := seed(ctx, repo, dir); err != nil {
		repo.Close()
		return nil, err
	}
	certificate, key, fingerprint, err := hubtls.EnsureSelfSigned(dir)
	if err != nil {
		repo.Close()
		return nil, err
	}
	fixture := &Fixture{
		Repo:           repo,
		Service:        service,
		Handler:        api.NewTLSServer("127.0.0.1:4173", service, certificate, key).Handler(),
		Dir:            dir,
		TLSCertificate: certificate,
		TLSKey:         key,
		TLSFingerprint: fingerprint,
	}
	keepDir = true
	return fixture, nil
}

func (f *Fixture) Close() error {
	var closeErr error
	if f.Repo != nil {
		closeErr = f.Repo.Close()
		f.Repo = nil
	}
	removeErr := os.RemoveAll(f.Dir)
	return errors.Join(closeErr, removeErr)
}

func seed(ctx context.Context, repo *sqlite.Repository, rootPath string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	db := repo.DB()
	// The library root is a dedicated media subdirectory, not the fixture temp
	// dir itself, so a scan of it is deterministic: the seeded asset's source
	// file is absent (thumbnail/proxy requests 404, exactly like a real
	// missing-mount test), and the only regular files present are the skipped
	// non-video ones below.
	mediaDir := filepath.Join(rootPath, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return err
	}
	// A supported-format fixture (referenced by the seeded asset location) and
	// the unsupported files a scan must report as skipped: an .mkv and a
	// sidecar, so POST /roots/{id}/scan returns skipped_files>0 with named
	// extensions and the zero-discovery warning path is exercisable.
	for _, name := range []string{"fixture.mp4", "skipped.mkv", "note.txt"} {
		if err := os.WriteFile(filepath.Join(mediaDir, name), []byte("fixture bytes"), 0o600); err != nil {
			return err
		}
	}
	root, err := repo.CreateLibraryRoot(ctx, mediaDir)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`, "asset-fixture", "fixture-fingerprint", 1234, "discovered", now, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "location-fixture", "asset-fixture", root.ID, `"><img src=x onerror=window.__xss=1>.mp4`, filepath.Join(mediaDir, "fixture.mp4"), now); err != nil {
		return err
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-fixture", "", []domain.AssetShot{{ID: "shot-fixture", AssetID: "asset-fixture", Ordinal: 0, StartMS: 0, EndMS: 5000, Description: `"><img src=x onerror=window.__xss=1>`, Tags: []string{`"><img src=x onerror=window.__xss=1>`}, Objects: []string{"camera"}, Confidence: .9, CreatedAt: time.Now().UTC()}}, "", ""); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO asset_tag_links(asset_id,raw_tag,normalized_tag,tag_type,source,created_at,updated_at) VALUES(?,?,?,?,'ai',?,?)`, "asset-fixture", "city", "city", "topic", now, now); err != nil {
		return err
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{ID: "collection-fixture", Name: `"><img src=x onerror=window.__xss=1>`, Description: `"><img src=x onerror=window.__xss=1>`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-fixture"); err != nil {
		return err
	}
	if _, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{ID: "channel-fixture", Capability: "video_analysis", Label: "Fixture channel", ProviderName: "fixture", Endpoint: "http://127.0.0.1:1", Model: "offline", Enabled: true, Members: []domain.ProviderChannelMember{{ID: "member-fixture", Label: "redacted-key", SecretRef: "fixture/secret", SecretReady: false, Enabled: true, Weight: 1, MaxInflight: 1}}}); err != nil {
		return err
	}
	for i, typ := range []domain.JobType{domain.JobProbe, domain.JobDerive, domain.JobAnalyze} {
		if err := repo.EnqueueJob(ctx, "asset-fixture", typ, "fixture-hash-"+string(rune('0'+i)), i); err != nil {
			return err
		}
	}
	if err := repo.EnqueueJob(ctx, "asset-fixture", domain.JobIndex, "fixture-hash-failed", 1); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET state='failed',terminal=1,last_error_code='configuration',last_error_message=? WHERE asset_id=? AND job_type=? AND input_hash=?`, "fixture provider failure: offline provider is intentionally unavailable", "asset-fixture", string(domain.JobIndex), "fixture-hash-failed"); err != nil {
		return err
	}
	return nil
}
