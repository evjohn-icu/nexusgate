package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ev/timingdex/internal/domain"
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
		if err := repo.SaveMediaMetadata(ctx, fixture.id, domain.MediaMetadata{CapturedAt: &captured, CameraModel: fixture.camera}, "browse-fixture"); err != nil {
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
