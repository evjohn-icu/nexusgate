package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// The schema claim behind KnownAssetIDs — an id either exists in assets or it
// does not — is what cache orphan detection rests on, so it is pinned against
// real SQLite rather than the in-memory app fakes.
func TestKnownAssetIDsReturnsOnlyExistingAssets(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	want := []string{seedAsset(t, repo, 1), seedAsset(t, repo, 2)}

	// The missing id appears twice: dedup must not turn the second copy into
	// a "known" answer, and the result must come back in input order.
	got, err := repo.KnownAssetIDs(ctx, []string{want[0], "asset-missing", want[1], "asset-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KnownAssetIDs = %v, want %v", got, want)
	}
}

func TestKnownAssetIDsEmptyAndAllMissing(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	got, err := repo.KnownAssetIDs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("nil input = %v, want empty", got)
	}
	got, err = repo.KnownAssetIDs(ctx, []string{"nope-1", "nope-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("all-missing input = %v, want empty", got)
	}
}

// 501 known assets cross the 500-id chunk boundary; the duplicate trailing
// copy also proves dedup survives chunking (a duplicate that lands in a
// later chunk must not come back twice).
func TestKnownAssetIDsChunksPastSQLiteVariableLimit(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	var want []string
	for i := 0; i < 501; i++ {
		id := seedAsset(t, repo, i+10)
		want = append(want, id)
	}
	ids := append(append([]string{}, want...), want[0], "missing")
	got, err := repo.KnownAssetIDs(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KnownAssetIDs returned %d ids, want %d", len(got), len(want))
	}
}

// ListDerivedArtifacts must return every row with all fields intact, ordered
// deterministically — the cache-health commands build their known-path set
// from it, so a dropped row would silently turn a real artifact file into a
// false orphan.
func TestListDerivedArtifacts(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	// derived_artifacts.asset_id is a foreign key into assets; every artifact
	// below must belong to a seeded asset row or the insert is rejected.
	for _, asset := range []string{"asset-1", "asset-2"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, asset, asset, now, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, artifact := range []struct {
		id, asset, typ, profile, path string
		size                          int64
	}{
		{"art-1", "asset-1", "thumbnail", "thumb-sw-v1", "/cache/asset-1/thumbnail-sw.jpg", 10},
		{"art-2", "asset-1", "proxy", "proxy-720-sw-v1", "/cache/asset-1/proxy-sw.mp4", 99},
		{"art-3", "asset-2", "audio", "audio-16k-v1", "/cache/asset-2/audio.m4a", 7},
		{"art-4", "asset-2", "proxy", "proxy-720-hw-v1", "/cache/asset-2/proxy-hw.mp4", 42},
	} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, artifact.id, artifact.asset, artifact.typ, artifact.profile, artifact.path, artifact.size, now); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListDerivedArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("ListDerivedArtifacts = %d rows, want 4", len(got))
	}
	// The implementation's documented order is asset_id, then artifact_type
	// ("proxy" < "thumbnail"), not insertion order.
	for i, want := range []struct {
		id, asset, typ, profile, path string
		size                          int64
	}{
		{"art-2", "asset-1", "proxy", "proxy-720-sw-v1", "/cache/asset-1/proxy-sw.mp4", 99},
		{"art-1", "asset-1", "thumbnail", "thumb-sw-v1", "/cache/asset-1/thumbnail-sw.jpg", 10},
		{"art-3", "asset-2", "audio", "audio-16k-v1", "/cache/asset-2/audio.m4a", 7},
		{"art-4", "asset-2", "proxy", "proxy-720-hw-v1", "/cache/asset-2/proxy-hw.mp4", 42},
	} {
		a := got[i]
		if a.ID != want.id || a.AssetID != want.asset || a.Type != want.typ || a.ProfileHash != want.profile || a.LocalPath != want.path || a.SizeBytes != want.size {
			t.Errorf("row %d = %+v, want %+v", i, a, want)
		}
	}
}

func TestMatchingAndDeletingHardwareDerivedProfiles(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	assetID := "asset-hardware-repair"
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, assetID, "fp-hardware-repair", now, now); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []struct{ id, typ, profile string }{
		{"hw-thumb", "thumbnail", "thumb-hw-cuda-h264-v1"},
		{"hw-proxy", "proxy", "proxy-720-hw-cuda-h264-v1"},
		{"software", "thumbnail", "thumb-software-h264-x264-v1"},
		{"audio", "audio", "audio-16k-v1"},
	} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, artifact.id, assetID, artifact.typ, artifact.profile, "/cache/"+artifact.id, 1, now); err != nil {
			t.Fatal(err)
		}
	}
	matching, err := repo.MatchingDerivedArtifactsByProfilePrefixes(ctx, []string{"thumb-hw-", "proxy-720-hw-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matching) != 2 {
		t.Fatalf("matching rows=%d, want 2", len(matching))
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM derived_artifacts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("query-only matching changed row count to %d", count)
	}
	assets, err := repo.DeleteDerivedArtifactsByProfilePrefixes(ctx, []string{"thumb-hw-", "proxy-720-hw-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0] != assetID {
		t.Fatalf("deleted assets=%v, want [%s]", assets, assetID)
	}
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM derived_artifacts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("remaining rows=%d, want software and audio", count)
	}
}

// ListAllAssetIDs feeds OrphanDirectories: an empty table must yield no ids,
// and every seeded asset must come back.
func TestListAllAssetIDs(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := formatTime(time.Now().UTC())

	empty, err := repo.ListAllAssetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("ListAllAssetIDs on empty table = %d ids, want 0", len(empty))
	}

	for _, id := range []string{"asset-list-1", "asset-list-2", "asset-list-3"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, id, "fp-"+id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ListAllAssetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ListAllAssetIDs = %d ids, want 3", len(got))
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range []string{"asset-list-1", "asset-list-2", "asset-list-3"} {
		if !seen[id] {
			t.Errorf("ListAllAssetIDs missing %s", id)
		}
	}
}

func TestListLiveJobAssetIDsLeaseMatrix(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	stamp := formatTime(now)
	for _, id := range []string{"live", "expired", "pending", "terminal", "nulllease", "wrongstate"} {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, id, id, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		id, state string
		terminal  int
		expiry    any
	}{
		{"live", "running", 0, formatTime(now.Add(time.Minute))},
		{"expired", "running", 0, formatTime(now.Add(-time.Minute))},
		{"pending", "pending", 0, formatTime(now.Add(time.Minute))},
		{"terminal", "running", 1, formatTime(now.Add(time.Minute))},
		{"nulllease", "running", 0, nil},
		{"wrongstate", "succeeded", 0, formatTime(now.Add(time.Minute))},
	}
	for i, row := range rows {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,lease_owner,lease_expires_at,terminal,created_at,updated_at) VALUES(?,?, 'derive',?,?,1,3,?,?,?, ?,?,?,?)`, fmt.Sprintf("job-%d", i), row.id, row.state, 1, stamp, "hash", "owner", row.expiry, row.terminal, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ListLiveJobAssetIDs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"live"}) {
		t.Fatalf("live ids = %v", got)
	}
}

// A row read back through ListDerivedArtifacts must populate every field of
// domain.DerivedArtifact — Verify builds its known-path set from these
// structs, so a dropped field would turn a real artifact file into a false
// orphan.
func TestListDerivedArtifactsPopulatesDomainType(t *testing.T) {
	repo := openTestRepo(t)
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	want := domain.DerivedArtifact{ID: "art-roundtrip", AssetID: "asset-roundtrip", Type: "proxy", ProfileHash: "proxy-720-sw-v1", LocalPath: "/cache/asset-roundtrip/proxy-sw.mp4", SizeBytes: 1234}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'discovered',?,?)`, want.AssetID, want.AssetID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, want.ID, want.AssetID, want.Type, want.ProfileHash, want.LocalPath, want.SizeBytes, now); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ListDerivedArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListDerivedArtifacts = %d rows, want 1", len(got))
	}
	if got[0] != want {
		t.Errorf("round-trip = %+v, want %+v", got[0], want)
	}
}
