package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/search"
)

// TestMultimodalTimelineIntervals exercises the exact contract the V1.5 e2e
// case describes, against real migrations: canonical shots partition a
// 40-second asset, one rain event spans most of it, and the intersections the
// API reports are the actual overlaps rather than the whole asset.
func TestMultimodalTimelineIntervals(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "multimodal-e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at)
		VALUES('asset-e2e','fp',1000,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	shots := []domain.AssetShot{
		{ID: "shot-1", Ordinal: 0, StartMS: 0, EndMS: 10000, Description: "wide street"},
		{ID: "shot-2", Ordinal: 1, StartMS: 10000, EndMS: 20000, Description: "horn and action"},
		{ID: "shot-3", Ordinal: 2, StartMS: 20000, EndMS: 30000, Description: "walking"},
		{ID: "shot-4", Ordinal: 3, StartMS: 30000, EndMS: 40000, Description: "rain tail"},
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-e2e", "", shots, "", ""); err != nil {
		t.Fatal(err)
	}

	space := domain.EmbeddingSpace{
		Modality: domain.ModalityVisual, Provider: "test", ModelID: "test/model",
		ModelRevision: "rev-1", Dimension: 4, Normalization: "l2",
		PreprocessingJSON: "{}", RuntimeDigest: "test",
	}
	space.ID, _ = domain.CanonicalSpaceID(space)
	if err := repo.UpsertEmbeddingSpace(ctx, space); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at,model_id,model_revision,backend)
		VALUES('gen-visual','asset-e2e','visual_embedding','in','cfg-visual','%s','shots','committed','[]','[]',?,?, 'test/model','rev-1','cpu')`, space.ID), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at)
		VALUES('asset-e2e','visual_embedding','cfg-visual','gen-visual',?)`, now); err != nil {
		t.Fatal(err)
	}
	for i, shot := range shots {
		vector := []float32{0, 0, 0, 0}
		vector[i] = 1
		blob, err := encodeQueryVector(vector, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO visual_embeddings(generation_id,asset_id,shot_id,space_id,sample_index,timestamp_ms,strategy,source_hash,vector,created_at)
			VALUES('gen-visual','asset-e2e',?, '%s', -1, 0, 'shot_frame_pooled','',?,?)`, space.ID), shot.ID, blob, now); err != nil {
			t.Fatal(err)
		}
	}

	// One sound generation: rain [1000,35000), horn [9000,10000), plus two
	// touching events used to prove the half-open rule at SQL level.
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at,model_id,model_revision,backend)
		VALUES('gen-sound','asset-e2e','sound_events','in','cfg-sound',NULL,'','committed','[{"start_ms":0,"end_ms":40000}]','[]',?,?, 'google/yamnet','rev-1','cpu')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at)
		VALUES('asset-e2e','sound_events','cfg-sound','gen-sound',?)`, now); err != nil {
		t.Fatal(err)
	}
	insertEvent := func(id, label string, start, end int64) {
		t.Helper()
		if _, err := repo.db.ExecContext(ctx, `
			INSERT INTO audio_events(id,generation_id,asset_id,start_ms,end_ms,event_label,confidence,raw_score,evidence_status,boundary_resolution_ms,created_at)
			VALUES(?,'gen-sound','asset-e2e',?,?,?,NULL,0.7,'possible',100,?)`, id, start, end, label, now); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent("event-rain", "Rain", 1000, 35000)
	insertEvent("event-horn", "Horn", 9000, 10000)
	insertEvent("event-touch-a", "Speech", 0, 1000)
	insertEvent("event-touch-b", "Speech", 1000, 2000)

	// Visual retrieval returns the four canonical shots that overlap the asset.
	visual, err := repo.VisualWindowMatches(ctx, space.ID, []float32{0, 1, 0, 0}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(visual) != 4 {
		t.Fatalf("visual matches=%d, want 4", len(visual))
	}
	if visual[0].ShotID != "shot-2" {
		t.Fatalf("best visual match is %s, want shot-2 for the aligned vector", visual[0].ShotID)
	}

	// The rain event must intersect each shot with the real overlap, not the
	// whole asset.
	events, err := repo.SoundEventMatches(ctx, []string{"Rain"}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Label != "Rain" {
		t.Fatalf("rain matches=%+v", events)
	}
	rain := domain.TimeInterval{StartMS: events[0].StartMS, EndMS: events[0].EndMS}
	if rain != (domain.TimeInterval{StartMS: 1000, EndMS: 35000}) {
		t.Fatalf("rain interval=%+v", rain)
	}
	want := []domain.TimeInterval{
		{StartMS: 1000, EndMS: 10000},
		{StartMS: 10000, EndMS: 20000},
		{StartMS: 20000, EndMS: 30000},
		{StartMS: 30000, EndMS: 35000},
	}
	for i, shot := range shots {
		got, ok := rain.Intersect(domain.TimeInterval{StartMS: shot.StartMS, EndMS: shot.EndMS})
		if !ok || got != want[i] {
			t.Fatalf("shot %d intersection=%+v ok=%v, want %+v", i, got, ok, want[i])
		}
	}

	// Touching events do not overlap: [0,1000) and [1000,2000).
	touching, err := repo.SoundEventMatches(ctx, []string{"Speech"}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(touching) != 2 {
		t.Fatalf("speech events=%d", len(touching))
	}
	a := domain.TimeInterval{StartMS: touching[0].StartMS, EndMS: touching[0].EndMS}
	b := domain.TimeInterval{StartMS: touching[1].StartMS, EndMS: touching[1].EndMS}
	if a.Overlaps(b) {
		t.Fatalf("touching events overlap: %+v %+v", a, b)
	}

	// Horn before the visual action [10500,12000): a 500 ms gap matches, 499
	// does not; a later horn cannot satisfy the predicate.
	action := domain.TimeInterval{StartMS: 10500, EndMS: 12000}
	horn := domain.TimeInterval{StartMS: 9000, EndMS: 10000}
	for gap, wantMatch := range map[int64]bool{500: true, 499: false} {
		ok, err := search.EvaluatePredicate("before", horn, action, gap)
		if err != nil {
			t.Fatal(err)
		}
		if ok != wantMatch {
			t.Fatalf("gap %d: before=%v, want %v", gap, ok, wantMatch)
		}
	}
	lateHorn := domain.TimeInterval{StartMS: 12000, EndMS: 13000}
	if ok, _ := search.EvaluatePredicate("before", lateHorn, action, 500); ok {
		t.Fatal("a horn starting at the action end must not satisfy before")
	}
}
