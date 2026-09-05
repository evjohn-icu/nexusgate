package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestListAssetCardsFilteredByCaptureFacets(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asset-browse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('browse-root','/library',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id, camera, region, session string
		captured                    time.Time
	}{
		{id: "sz-fx3", camera: "Sony FX3", region: "中国 · 深圳 · 南山", session: "session-sz", captured: now},
		{id: "hk-dji", camera: "DJI Mavic 3", region: "中国 · 香港", session: "session-hk", captured: now.Add(24 * time.Hour)},
	} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, fixture.id, fixture.id, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?, 'browse-root', ?, ?, 1,1,1,?)`, "loc-"+fixture.id, fixture.id, fixture.id+".mov", "/library/"+fixture.id+".mov", stamp); err != nil {
			t.Fatal(err)
		}
		captured := fixture.captured
		if err := repo.SaveMediaMetadata(ctx, fixture.id, domain.MediaMetadata{CapturedAt: &captured, CameraModel: fixture.camera}, "browse-fixture", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `UPDATE capture_metadata SET region_label=? WHERE asset_id=?`, fixture.region, fixture.id); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO shoot_sessions(id,root_id,title,state,created_at,updated_at) VALUES(?, 'browse-root', ?, 'manual', ?, ?)`, fixture.session, fixture.session, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_shoot_sessions(asset_id,session_id,is_primary,created_at) VALUES(?,?,1,?)`, fixture.id, fixture.session, stamp); err != nil {
			t.Fatal(err)
		}
	}

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)
	cards, err := repo.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{
		Limit: 20, CapturedFrom: &from, CapturedTo: &to,
		RegionLabel: "中国 · 深圳 · 南山", CameraModel: "Sony FX3", SessionID: "session-sz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].ID != "sz-fx3" {
		t.Fatalf("filtered cards=%+v, want only sz-fx3", cards)
	}
}

// TestListAssetCardsFilteredBatchesArtifactURLs exercises the thumbnail/proxy
// lookup that used to call GetArtifact once per card per artifact type
// (1+2n queries for a page). It checks correctness across every combination
// of artifact presence for a handful of assets, which the batched
// assetArtifactPresence query must still get right per (asset, type) pair.
func TestListAssetCardsFilteredBatchesArtifactURLs(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "asset-browse-artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('artifact-root','/library',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		id                 string
		hasThumb, hasProxy bool
	}
	fixtures := []fixture{
		{id: "asset-both", hasThumb: true, hasProxy: true},
		{id: "asset-thumb-only", hasThumb: true, hasProxy: false},
		{id: "asset-proxy-only", hasThumb: false, hasProxy: true},
		{id: "asset-neither", hasThumb: false, hasProxy: false},
	}
	for _, fx := range fixtures {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, fx.id, fx.id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,'artifact-root',?,?,1,1,1,?)`, "loc-"+fx.id, fx.id, fx.id+".mov", "/library/"+fx.id+".mov", now); err != nil {
			t.Fatal(err)
		}
		// Raw SQL, not repo.SaveArtifact: these fixtures have no job behind them,
		// and SaveArtifact now requires one to own -- see the ownership
		// predicate added in jobs_lease_ownership_test.go.
		if fx.hasThumb {
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, "thumb-"+fx.id, fx.id, "thumbnail", "sw", "/cache/"+fx.id+"-thumb.jpg", 10, now); err != nil {
				t.Fatal(err)
			}
		}
		if fx.hasProxy {
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, "proxy-"+fx.id, fx.id, "proxy", "sw", "/cache/"+fx.id+"-proxy.mp4", 20, now); err != nil {
				t.Fatal(err)
			}
		}
	}

	cards, err := repo.ListAssetCardsFiltered(ctx, domain.AssetCardFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]domain.AssetCard, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}
	if len(byID) != len(fixtures) {
		t.Fatalf("cards=%+v, want %d distinct assets", cards, len(fixtures))
	}
	for _, fx := range fixtures {
		card, ok := byID[fx.id]
		if !ok {
			t.Fatalf("missing card for %s", fx.id)
		}
		wantThumb := ""
		if fx.hasThumb {
			wantThumb = "/api/v1/assets/" + fx.id + "/thumbnail"
		}
		wantProxy := ""
		if fx.hasProxy {
			wantProxy = "/api/v1/assets/" + fx.id + "/proxy"
		}
		if card.ThumbnailURL != wantThumb {
			t.Fatalf("asset %s thumbnail=%q want %q", fx.id, card.ThumbnailURL, wantThumb)
		}
		if card.ProxyURL != wantProxy {
			t.Fatalf("asset %s proxy=%q want %q", fx.id, card.ProxyURL, wantProxy)
		}
	}
}
