package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
)

// TestShotSearchNotPollutedByAssetGlobalObjects is the regression test for
// the shot-truth rule: whole-asset Objects/Actions/Mood must never be copied
// into shots that did not observe them. Here the asset is globally tagged with
// "car" (subjects_json) while shot A observed nothing — searching "car" must
// only find shot B, whose own evidence contains it. Before the fix the
// fallback put "car" into shot A's FTS row and semantic vector as well.
func TestShotSearchNotPollutedByAssetGlobalObjects(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-pollution.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-poll','fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	runID, _, err := repo.CreateModelRun(ctx, "asset-poll", "vision", "fixture", "fixture-model", "poll-hash", "footage-analysis-v4", "asset-analysis/v2", "{}", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Asset-global analysis claims the asset contains a car; shot A's own
	// observation list is empty. Only shot B reports the car itself.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_analysis(asset_id,source_run_id,schema_version,asset_type,shot_size,camera_motion,audio_type,lighting,people_count,has_speech,quality,summary,scene_tags_json,subjects_json,mood_tags_json,usable_as_json,quality_flags_json,extra_tags_json,editorial_reason,updated_at)
		VALUES('asset-poll',?,'asset-analysis/v2','b_roll','wide','static','none',0,0,0,'usable','traffic intersection','[]','["car"]','[]','[]','[]','["driving"]','',?)`, runID, now); err != nil {
		t.Fatal(err)
	}
	// The model answer mirrors the real bug shape: whole-asset objects claim
	// the asset contains a car (subjects_json in asset_analysis), while shot A
	// observed nothing. The shots are produced through ToAssetShots — the very
	// function under test — so a reintroduced fallback inside it is caught
	// here, not just by the domain-level test.
	result := videoanalysis.Result{
		Objects: []videoanalysis.Object{{Name: "car"}, {Name: "person"}},
		Actions: []videoanalysis.Action{{Name: "driving"}},
		Mood:    []string{"tense"},
		Shots: []videoanalysis.Shot{
			{StartMS: 0, EndMS: 10_000, Description: "empty street at dawn"},
			{StartMS: 20_000, EndMS: 30_000, Description: "red car crossing", Objects: []string{"car"}},
		},
	}
	shots := result.ToAssetShots("asset-poll", runID)
	if len(shots) != 2 || len(shots[0].Objects) != 0 {
		t.Fatalf("ToAssetShots polluted shot A: %+v", shots)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-poll", runID, shots, "", ""); err != nil {
		t.Fatal(err)
	}

	// FTS: "car" appears only in shot B's indexed objects.
	hits, err := repo.SearchShots(ctx, "car", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Ordinal != 1 {
		t.Fatalf("FTS search for car must hit only the shot that saw it, got %+v", hits)
	}

	// Hybrid: the semantic side must not attribute the asset-global car to
	// shot A either.
	hybrid, err := repo.HybridSearchShots(ctx, "car", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hybrid {
		if hit.Ordinal == 0 {
			t.Fatalf("hybrid search for car must not hit shot A (no evidence), got %+v", hybrid)
		}
	}
	if len(hybrid) != 1 || hybrid[0].Ordinal != 1 {
		t.Fatalf("hybrid search for car must hit shot B only, got %+v", hybrid)
	}

	// The canonical rows themselves must carry the empty list, not a
	// convenience fallback.
	got, err := repo.ListAssetShots(ctx, "asset-poll")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 shots, got %d", len(got))
	}
	for _, shot := range got {
		if shot.Ordinal == 0 && len(shot.Objects) != 0 {
			t.Fatalf("shot A objects must stay empty, got %v", shot.Objects)
		}
		if shot.Ordinal == 1 && (len(shot.Objects) != 1 || shot.Objects[0] != "car") {
			t.Fatalf("shot B objects mangled: %v", shot.Objects)
		}
	}
}
