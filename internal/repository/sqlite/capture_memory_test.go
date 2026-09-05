package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestShootSessionPersistenceIsIdempotentAndFiltersWithoutCoordinates(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "capture-memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-memory','/footage',?,?)`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	for _, assetID := range []string{"asset-memory-1", "asset-memory-2"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, assetID, assetID+"-fp", formatTime(now), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}

	latitude, longitude := 48.8566, 2.3522
	if err := repo.SaveMediaMetadata(ctx, "asset-memory-1", domain.MediaMetadata{Latitude: &latitude, Longitude: &longitude}, "capture-memory-test", "", ""); err != nil {
		t.Fatal(err)
	}
	var storedLatitude, storedLongitude float64
	if err := repo.db.QueryRowContext(ctx, `SELECT latitude,longitude FROM capture_metadata WHERE asset_id='asset-memory-1'`).Scan(&storedLatitude, &storedLongitude); err != nil {
		t.Fatal(err)
	}
	if storedLatitude != latitude || storedLongitude != longitude {
		t.Fatalf("coordinates=%v/%v, want %v/%v", storedLatitude, storedLongitude, latitude, longitude)
	}

	starts := now.Add(-time.Hour)
	ends := now.Add(-30 * time.Minute)
	session := domain.ShootSession{
		ID:          "session-memory-1",
		RootID:      "root-memory",
		Title:       "Paris dawn",
		State:       "manual",
		StartsAt:    &starts,
		EndsAt:      &ends,
		RegionLabel: "Paris",
		CameraLabel: "Sony FX3",
		Confidence:  0.94,
		AssetIDs:    []string{"asset-memory-1", "asset-memory-2"},
	}
	if err := repo.SaveShootSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	session.Title = "Paris dawn retitled"
	session.AssetIDs = []string{"asset-memory-1"}
	if err := repo.SaveShootSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListShootSessions(ctx, domain.ShootSessionFilter{
		RootID:      "root-memory",
		RegionLabel: "Paris",
		CameraLabel: "Sony FX3",
		State:       "manual",
		StartsAfter: timePtr(now.Add(-2 * time.Hour)),
		EndsBefore:  timePtr(now),
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Paris dawn retitled" || len(got[0].AssetIDs) != 1 || got[0].AssetIDs[0] != "asset-memory-1" {
		t.Fatalf("sessions=%+v", got)
	}

	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "latitude") || strings.Contains(string(encoded), "longitude") {
		t.Fatalf("session list leaked coordinates: %s", encoded)
	}

	assets, err := repo.ListShootSessionAssets(ctx, "session-memory-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].AssetID != "asset-memory-1" || !assets[0].IsPrimary {
		t.Fatalf("session assets=%+v", assets)
	}

	other, err := repo.ListShootSessions(ctx, domain.ShootSessionFilter{RegionLabel: "Singapore", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("region filter returned=%+v", other)
	}
}

func TestGetShootSessionReturnsDetailAndNilForUnknownSession(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "capture-memory-detail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-detail','asset-detail-fp',1,'discovered',?,?)`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	session := domain.ShootSession{ID: "session-detail", Title: "Detail session", State: "automatic", AssetIDs: []string{"asset-detail"}}
	if err := repo.SaveShootSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	got, err := repo.GetShootSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != session.ID || got.Title != session.Title || len(got.AssetIDs) != 1 || got.AssetIDs[0] != "asset-detail" {
		t.Fatalf("session detail=%+v", got)
	}
	missing, err := repo.GetShootSession(ctx, "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("unknown session=%+v", missing)
	}
}

func timePtr(value time.Time) *time.Time { return &value }

// TestListShootSessionsBatchesAssetIDLookups covers ListShootSessions across
// several sessions at once. It used to call listShootSessionAssetIDs once
// per returned session (up to maxShootSessionLimit per page); this checks
// the batched replacement still assigns the right asset IDs, in the right
// order, to the right session -- including a session with no memberships.
func TestListShootSessionsBatchesAssetIDLookups(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "capture-memory-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-batch','/footage',?,?)`, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	assetIDs := []string{"batch-asset-1", "batch-asset-2", "batch-asset-3"}
	for _, id := range assetIDs {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id+"-fp", formatTime(now), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}

	sessions := []domain.ShootSession{
		{ID: "batch-session-solo", RootID: "root-batch", Title: "Solo", State: "manual", StartsAt: timePtr(now.Add(-3 * time.Hour)), AssetIDs: []string{"batch-asset-1"}},
		{ID: "batch-session-pair", RootID: "root-batch", Title: "Pair", State: "manual", StartsAt: timePtr(now.Add(-2 * time.Hour)), AssetIDs: []string{"batch-asset-2", "batch-asset-3"}},
		{ID: "batch-session-empty", RootID: "root-batch", Title: "Empty", State: "manual", StartsAt: timePtr(now.Add(-1 * time.Hour))},
	}
	for _, session := range sessions {
		if err := repo.SaveShootSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListShootSessions(ctx, domain.ShootSessionFilter{RootID: "root-batch", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(sessions) {
		t.Fatalf("sessions=%+v, want %d", got, len(sessions))
	}
	byID := make(map[string]domain.ShootSession, len(got))
	for _, s := range got {
		byID[s.ID] = s
	}
	if ids := byID["batch-session-solo"].AssetIDs; len(ids) != 1 || ids[0] != "batch-asset-1" {
		t.Fatalf("solo session assets=%+v", ids)
	}
	pairIDs := byID["batch-session-pair"].AssetIDs
	if len(pairIDs) != 2 {
		t.Fatalf("pair session assets=%+v", pairIDs)
	}
	seen := map[string]bool{}
	for _, id := range pairIDs {
		seen[id] = true
	}
	if !seen["batch-asset-2"] || !seen["batch-asset-3"] {
		t.Fatalf("pair session assets=%+v, want batch-asset-2 and batch-asset-3", pairIDs)
	}
	if ids := byID["batch-session-empty"].AssetIDs; len(ids) != 0 {
		t.Fatalf("empty session assets=%+v, want none", ids)
	}
}
