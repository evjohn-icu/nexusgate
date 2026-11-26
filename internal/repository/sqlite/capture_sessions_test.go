package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func seedSessionAsset(t *testing.T, repo *Repository, ctx context.Context, assetID, rootID string, capturedAt time.Time, make, model, serial string, durationMS int64) {
	t.Helper()
	stamp := formatTime(capturedAt)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, assetID, assetID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,? ,?,1,1,1,?)`, "loc-"+assetID, assetID, rootID, assetID+".mov", "/footage/"+assetID+".mov", stamp); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{CapturedAt: &capturedAt, CaptureTimeSource: "embedded_exif", CaptureTimeConfidence: .95, CameraMake: make, CameraModel: model, CameraSerial: serial, DurationMS: durationMS}, "session-fixture", "", ""); err != nil {
		t.Fatal(err)
	}
}

// TestRebuildAutomaticShootSessionsGroupsSameCameraWithinThirtyMinutes pins
// the automatic join rule and its hysteresis: a weak re-probe that does not
// change the aggregated grouping must not change persisted membership.
func TestRebuildAutomaticShootSessionsGroupsSameCameraWithinThirtyMinutes(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-session','/footage',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	seedSessionAsset(t, repo, ctx, "session-a", "root-session", now, "Sony", "FX3", "serial-1", 5000)
	seedSessionAsset(t, repo, ctx, "session-b", "root-session", now.Add(20*time.Minute), "Sony", "FX3", "serial-1", 5000)
	seedSessionAsset(t, repo, ctx, "session-c", "root-session", now.Add(55*time.Minute), "Sony", "FX3", "serial-1", 5000)
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-session"); err != nil {
		t.Fatal(err)
	}
	var sessions, mappings int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shoot_sessions WHERE root_id='root-session' AND state='automatic'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shoot_sessions`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 || mappings != 3 {
		t.Fatalf("sessions=%d mappings=%d, want 2/3", sessions, mappings)
	}
	snapshotMembership := func() []string {
		rows, err := repo.db.QueryContext(ctx, `SELECT ass.session_id,ass.asset_id FROM asset_shoot_sessions ass JOIN shoot_sessions ss ON ss.id=ass.session_id WHERE ss.root_id='root-session' AND ss.state='automatic' ORDER BY ass.session_id,ass.asset_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		groups := map[string][]string{}
		for rows.Next() {
			var sessionID, assetID string
			if err := rows.Scan(&sessionID, &assetID); err != nil {
				t.Fatal(err)
			}
			groups[sessionID] = append(groups[sessionID], assetID)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		membership := make([]string, 0, len(groups))
		for _, assets := range groups {
			sort.Strings(assets)
			membership = append(membership, strings.Join(assets, ","))
		}
		sort.Strings(membership)
		return membership
	}
	before := snapshotMembership()
	weak := now.Add(24 * time.Hour)
	if err := repo.SaveMediaMetadata(ctx, "session-a", domain.MediaMetadata{CapturedAt: &weak, CaptureTimeSource: "filesystem", CaptureTimeConfidence: .25, CameraMake: "Sony", CameraModel: "FX3", CameraSerial: "serial-1", DurationMS: 5000}, "weak-reprobe", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-session"); err != nil {
		t.Fatal(err)
	}
	after := snapshotMembership()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("weak re-probe changed session membership: before=%v after=%v", before, after)
	}
}

// TestRebuildAutomaticShootSessionsDeduplicatesMultiLocationAssets pins the
// UNIQUE(asset_id, session_id) boundary: one file linked from two locations
// under a root (mirrored folder, hardlinked copy) is one capture, so the
// rebuild must not insert the same (asset, session) pair twice. Before the
// GROUP BY dedup this failed with a constraint error and aborted the whole
// pipeline pass.
func TestRebuildAutomaticShootSessionsDeduplicatesMultiLocationAssets(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "sessions-multi-location.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-multi','/footage',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	// multi-a lives at two paths under the same root: two location rows, one
	// asset. multi-b is the single-location control.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('multi-a','multi-a',1,'discovered',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"clips/a.mov", "mirror/a.mov"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?, 'multi-a','root-multi',?,?,1,1,?,?)`, "loc-a-"+path, path, "/footage/"+path, formatTime(now.Add(time.Duration(i)*time.Hour)), stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SaveMediaMetadata(ctx, "multi-a", domain.MediaMetadata{CapturedAt: &now, CameraMake: "Sony", CameraModel: "FX3", CameraSerial: "serial-m", DurationMS: 5000}, "session-fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	seedSessionAsset(t, repo, ctx, "multi-b", "root-multi", now.Add(10*time.Minute), "Sony", "FX3", "serial-m", 5000)
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-multi"); err != nil {
		t.Fatalf("rebuild must not violate UNIQUE(asset_id, session_id): %v", err)
	}
	var mappings int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asset_shoot_sessions`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if mappings != 2 {
		t.Fatalf("mappings=%d, want 2 (one per asset, multi-a once despite two locations)", mappings)
	}
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-multi"); err != nil {
		t.Fatalf("second rebuild must be idempotent: %v", err)
	}
}

// TestRebuildAutomaticShootSessionsPreservesIDsAndCollectionFilters pins the
// stable-id contract: an unchanged rebuild reuses every persisted session id
// (collection filters and deep links reference them), and a saved collection
// filtered on a session still returns that session's assets afterwards.
func TestRebuildAutomaticShootSessionsPreservesIDsAndCollectionFilters(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "sessions-stable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-stable','/footage',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	seedSessionAsset(t, repo, ctx, "stable-a", "root-stable", now, "Sony", "FX3", "serial-1", 5000)
	seedSessionAsset(t, repo, ctx, "stable-b", "root-stable", now.Add(20*time.Minute), "Sony", "FX3", "serial-1", 5000)
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-stable"); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := repo.db.QueryRowContext(ctx, `SELECT id FROM shoot_sessions WHERE root_id='root-stable' AND state='automatic'`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	created, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{
		Name:   "that morning",
		Filter: domain.AssetCollectionFilter{SessionID: sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-stable"); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shoot_sessions WHERE root_id='root-stable' AND state='automatic'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("sessions=%d, want 1 after unchanged rebuild", sessions)
	}
	var keptID string
	if err := repo.db.QueryRowContext(ctx, `SELECT id FROM shoot_sessions WHERE root_id='root-stable' AND state='automatic'`).Scan(&keptID); err != nil {
		t.Fatal(err)
	}
	if keptID != sessionID {
		t.Fatalf("unchanged rebuild rotated session id: before=%s after=%s", sessionID, keptID)
	}
	cards, err := repo.ListAssetCardsInCollection(ctx, created.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("collection cards=%d, want both session assets after rebuild", len(cards))
	}
}

// TestRebuildAutomaticShootSessionsDropsObsoleteAndKeepsOverlapping pins the
// matching rule on a changed grouping: an asset that left its old group keeps
// that group's id only for the surviving overlap, the emptied group is
// removed, and the split-off group gets a new id.
func TestRebuildAutomaticShootSessionsDropsObsoleteAndKeepsOverlapping(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "sessions-regroup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-regroup','/footage',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	seedSessionAsset(t, repo, ctx, "regroup-a", "root-regroup", now, "Sony", "FX3", "serial-1", 5000)
	seedSessionAsset(t, repo, ctx, "regroup-b", "root-regroup", now.Add(10*time.Minute), "Sony", "FX3", "serial-1", 5000)
	seedSessionAsset(t, repo, ctx, "regroup-c", "root-regroup", now.Add(55*time.Minute), "Sony", "FX3", "serial-1", 5000)
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-regroup"); err != nil {
		t.Fatal(err)
	}
	var pairSession string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT ass.session_id FROM asset_shoot_sessions ass JOIN shoot_sessions ss ON ss.id=ass.session_id WHERE ss.root_id='root-regroup' AND ss.state='automatic' AND ass.asset_id='regroup-a'`).Scan(&pairSession); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO shoot_sessions(id,root_id,title,state,created_at,updated_at) VALUES('empty-automatic','root-regroup','Empty','automatic',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	// regroup-c moves 23 hours later: out of the pair's neighborhood. The pair
	// keeps its session id; the old one-asset session is obsolete; the moved
	// asset lands in a new session.
	moved := now.Add(23 * time.Hour)
	if err := repo.SaveMediaMetadata(ctx, "regroup-c", domain.MediaMetadata{CapturedAt: &moved, CaptureTimeSource: "embedded_exif", CaptureTimeConfidence: .95, CameraMake: "Sony", CameraModel: "FX3", CameraSerial: "serial-1", DurationMS: 5000}, "session-fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-regroup"); err != nil {
		t.Fatal(err)
	}
	var keptForA string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT ass.session_id FROM asset_shoot_sessions ass JOIN shoot_sessions ss ON ss.id=ass.session_id WHERE ss.root_id='root-regroup' AND ss.state='automatic' AND ass.asset_id='regroup-a'`).Scan(&keptForA); err != nil {
		t.Fatal(err)
	}
	if keptForA != pairSession {
		t.Fatalf("overlapping group rotated id: before=%s after=%s", pairSession, keptForA)
	}
	var sessions int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shoot_sessions WHERE root_id='root-regroup' AND state='automatic'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 {
		t.Fatalf("sessions=%d, want the pair plus one new session for the moved asset", sessions)
	}
	var cSessions int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM asset_shoot_sessions ass JOIN shoot_sessions ss ON ss.id=ass.session_id WHERE ss.root_id='root-regroup' AND ss.state='automatic' AND ass.asset_id='regroup-c'`).Scan(&cSessions); err != nil {
		t.Fatal(err)
	}
	if cSessions != 1 {
		t.Fatalf("regroup-c automatic links=%d, want exactly one", cSessions)
	}
	var cInOld int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM asset_shoot_sessions WHERE session_id=? AND asset_id='regroup-c'`, pairSession).Scan(&cInOld); err != nil {
		t.Fatal(err)
	}
	if cInOld != 0 {
		t.Fatalf("moved asset still linked to the old session id")
	}
}

// TestRebuildAutomaticShootSessionsNoInventedDevice pins the device-identity
// boundary: captures whose metadata carries neither vendor nor model must not
// merge through an invented device key just because they are close in time.
func TestRebuildAutomaticShootSessionsNoInventedDevice(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "sessions-nodevice.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stamp := formatTime(now)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-nodevice','/footage',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	seedSessionAsset(t, repo, ctx, "nodevice-a", "root-nodevice", now, "", "", "", 5000)
	seedSessionAsset(t, repo, ctx, "nodevice-b", "root-nodevice", now.Add(5*time.Minute), "", "", "", 5000)
	if err := repo.RebuildAutomaticShootSessions(ctx, "root-nodevice"); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shoot_sessions WHERE root_id='root-nodevice' AND state='automatic'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 {
		t.Fatalf("sessions=%d, want 2: vendor/model-free captures share no real device and must not merge", sessions)
	}
}
