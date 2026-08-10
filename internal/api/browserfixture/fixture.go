//go:build playwrightfixture

// Package browserfixture provides a deterministic, offline browser fixture.
package browserfixture

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/evjohn-icu/timingdex/internal/api"
	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

const AdminToken = "pw-admin-fixture"

type Fixture struct {
	Repo    *sqlite.Repository
	Service *app.Service
	Handler http.Handler
	Dir     string
}

func New(ctx context.Context) (*Fixture, error) {
	dir, err := os.MkdirTemp("", "timingdex-playwright-")
	if err != nil {
		return nil, err
	}
	repo, err := sqlite.Open(filepath.Join(dir, "timingdex.db"))
	if err != nil {
		return nil, err
	}
	if err := repo.Migrate(ctx); err != nil {
		repo.Close()
		return nil, err
	}
	service, err := app.NewService(repo, config.Config{DataDir: dir, HubSecurity: config.HubSecurityConfig{AdminToken: AdminToken}, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		repo.Close()
		return nil, err
	}
	if err := seed(ctx, repo, dir); err != nil {
		repo.Close()
		return nil, err
	}
	return &Fixture{Repo: repo, Service: service, Handler: api.NewServer("", service).Handler(), Dir: dir}, nil
}

func (f *Fixture) Close() error {
	if f.Repo != nil {
		return f.Repo.Close()
	}
	return nil
}

func seed(ctx context.Context, repo *sqlite.Repository, rootPath string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	db := repo.DB()
	root, err := repo.CreateLibraryRoot(ctx, rootPath)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`, "asset-fixture", "fixture-fingerprint", 1234, "discovered", now, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "location-fixture", "asset-fixture", root.ID, `"><img src=x onerror=window.__xss=1>.mp4`, `/tmp/fixture.mp4`, now); err != nil {
		return err
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-fixture", "", []domain.AssetShot{{ID: "shot-fixture", AssetID: "asset-fixture", Ordinal: 0, StartMS: 0, EndMS: 5000, Description: `"><img src=x onerror=window.__xss=1>`, Tags: []string{`"><img src=x onerror=window.__xss=1>`}, Objects: []string{"camera"}, Confidence: .9, CreatedAt: time.Now().UTC()}}); err != nil {
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
	return nil
}
