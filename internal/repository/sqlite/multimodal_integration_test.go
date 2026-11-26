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

	// The rain event must keep its own interval and pair with every
	// overlapping canonical shot, one row per (event, shot) pair.
	events, err := repo.SoundEventMatches(ctx, []string{"Rain"}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("rain matches=%d, want one pair per overlapping shot", len(events))
	}
	rain := domain.TimeInterval{StartMS: events[0].StartMS, EndMS: events[0].EndMS}
	if rain != (domain.TimeInterval{StartMS: 1000, EndMS: 35000}) {
		t.Fatalf("rain interval=%+v", rain)
	}
	for i, event := range events {
		shot := shots[i]
		if event.ShotID != shot.ID {
			t.Fatalf("rain pair %d shot=%s, want %s", i, event.ShotID, shot.ID)
		}
		if event.ShotStartMS != shot.StartMS || event.ShotEndMS != shot.EndMS {
			t.Fatalf("rain pair %d canonical bounds=%d-%d, want %d-%d", i, event.ShotStartMS, event.ShotEndMS, shot.StartMS, shot.EndMS)
		}
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

	// Touching events do not overlap: [0,1000) and [1000,2000); each pairs
	// with shot-1 only.
	touching, err := repo.SoundEventMatches(ctx, []string{"Speech"}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(touching) != 2 || touching[0].ShotID != "shot-1" || touching[1].ShotID != "shot-1" {
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

// fixtureShots leaves a shot gap so a fully-outside window can be asserted:
// shot-1 [0,10s), gap [10s,15s), shot-2 [15s,30s).
func fixtureShots() []domain.AssetShot {
	return []domain.AssetShot{
		{ID: "shot-1", Ordinal: 0, StartMS: 0, EndMS: 10000, Description: "wide street"},
		{ID: "shot-2", Ordinal: 1, StartMS: 15000, EndMS: 30000, Description: "action"},
	}
}

func insertAudioFixture(t *testing.T, ctx context.Context, repo *Repository, space domain.EmbeddingSpace, now string) {
	t.Helper()
	if _, err := repo.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at,model_id,model_revision,backend)
		VALUES('gen-audio','asset-e2e','audio_embedding','in','cfg-audio','%s','','committed','[]','[]',?,?, 'test/model','rev-1','cpu')`, space.ID), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at)
		VALUES('asset-e2e','audio_embedding','cfg-audio','gen-audio',?)`, now); err != nil {
		t.Fatal(err)
	}
	insertWindow := func(id int, start, end int64) {
		t.Helper()
		blob, err := encodeQueryVector([]float32{1, 0, 0, 0}, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO audio_embeddings(id,generation_id,asset_id,space_id,start_ms,end_ms,vector,created_at)
			VALUES(?,'gen-audio','asset-e2e','%s',?,?,?,?)`, space.ID), id, start, end, blob, now); err != nil {
			t.Fatal(err)
		}
	}
	insertWindow(1, 5000, 16000)  // spans shot-1, the gap and shot-2
	insertWindow(2, 11000, 14000) // fully inside the shot gap
}

func insertSpeakerFixture(t *testing.T, ctx context.Context, repo *Repository, now string) {
	t.Helper()
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at,model_id,model_revision,backend)
		VALUES('gen-diar','asset-e2e','diarization','in','cfg-diar',NULL,'','committed','[]','[]',?,?,'test/model','rev-1','cpu')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at)
		VALUES('asset-e2e','diarization','cfg-diar','gen-diar',?)`, now); err != nil {
		t.Fatal(err)
	}
	insertTurn := func(id, speaker string, start, end int64) {
		t.Helper()
		if _, err := repo.db.ExecContext(ctx, `
			INSERT INTO speaker_turns(id,generation_id,asset_id,start_ms,end_ms,local_speaker_id,confidence,overlap,created_at)
			VALUES(?,'gen-diar','asset-e2e',?,?,?,0.9,0,?)`, id, start, end, speaker, now); err != nil {
			t.Fatal(err)
		}
	}
	insertTurn("turn-in-shot-1", "speaker-1", 1000, 6000)
	insertTurn("turn-in-shot-2", "speaker-1", 16000, 22000)
	insertTurn("turn-in-gap", "speaker-1", 11000, 14000)
}

// TestMultimodalShotJoinedChannelsRespectUniverse: audio windows and speaker
// turns pair with the canonical shots under the half-open rule, records fully
// outside every shot produce no pair, and the eligible universe applies to the
// joined shot id — a 5–15 s record spanning two shots yields only the eligible
// overlapping shot with its canonical bounds and unaltered record interval.
func TestMultimodalShotJoinedChannelsRespectUniverse(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "multimodal-join.db"))
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
	if err := repo.ReplaceAssetShots(ctx, "asset-e2e", "", fixtureShots(), "", ""); err != nil {
		t.Fatal(err)
	}
	space := domain.EmbeddingSpace{
		Modality: domain.ModalityAudio, Provider: "test", ModelID: "test/model",
		ModelRevision: "rev-1", Dimension: 4, Normalization: "l2",
		PreprocessingJSON: "{}", RuntimeDigest: "test",
	}
	space.ID, _ = domain.CanonicalSpaceID(space)
	if err := repo.UpsertEmbeddingSpace(ctx, space); err != nil {
		t.Fatal(err)
	}
	insertAudioFixture(t, ctx, repo, space, now)
	insertSpeakerFixture(t, ctx, repo, now)

	minDuration := int64(11000)
	universe, err := repo.FilteredShotUniverse(ctx, domain.FacetFilter{MinDurationMS: &minDuration}, domain.AssetContextFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(universe) != 1 || !universe["shot-2"] {
		t.Fatalf("facet universe=%v, want only shot-2", universe)
	}

	audio, err := repo.AudioWindowMatches(ctx, space.ID, []float32{1, 0, 0, 0}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) != 2 {
		t.Fatalf("audio pairs=%d, want one per overlapping shot (gap window invisible)", len(audio))
	}
	wantShots := []string{"shot-1", "shot-2"}
	wantBounds := [][2]int64{{0, 10000}, {15000, 30000}}
	for i, match := range audio {
		if match.RecordID != "audio-1" || match.ShotID != wantShots[i] {
			t.Fatalf("audio pair %d = %s/%s", i, match.RecordID, match.ShotID)
		}
		if match.StartMS != 5000 || match.EndMS != 16000 {
			t.Fatalf("audio pair %d interval=%d-%d, want the window's own 5000-16000", i, match.StartMS, match.EndMS)
		}
		if match.ShotStartMS != wantBounds[i][0] || match.ShotEndMS != wantBounds[i][1] {
			t.Fatalf("audio pair %d canonical bounds=%d-%d", i, match.ShotStartMS, match.ShotEndMS)
		}
	}
	restricted, err := repo.AudioWindowMatches(ctx, space.ID, []float32{1, 0, 0, 0}, universe, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(restricted) != 1 || restricted[0].ShotID != "shot-2" {
		t.Fatalf("facet-restricted audio pairs=%+v, want only shot-2", restricted)
	}

	turns, err := repo.SpeakerTurnMatches(ctx, "asset-e2e", "gen-diar", "speaker-1", universe, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].ShotID != "shot-2" || turns[0].StartMS != 16000 || turns[0].EndMS != 22000 {
		t.Fatalf("facet-restricted turns=%+v, want the shot-2 turn only", turns)
	}
	allTurns, err := repo.SpeakerTurnMatches(ctx, "asset-e2e", "gen-diar", "speaker-1", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(allTurns) != 2 {
		t.Fatalf("turn pairs=%d, want 2 (the gap turn pairs with nothing)", len(allTurns))
	}
	if allTurns[0].ShotStartMS != 0 || allTurns[0].ShotEndMS != 10000 || allTurns[1].ShotStartMS != 15000 {
		t.Fatalf("turn canonical bounds wrong: %+v", allTurns)
	}
}

// sqliteModalityStore is the minimal search.ModalityStore adapter for the
// full-pipeline test below; the app adapter maps the same four calls.
type sqliteModalityStore struct {
	repo *Repository
}

func (s *sqliteModalityStore) FilteredShotUniverse(ctx context.Context, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) (map[string]bool, error) {
	return s.repo.FilteredShotUniverse(ctx, facets, assetFilter)
}

func (s *sqliteModalityStore) VisualWindows(ctx context.Context, _ string, _ []float32, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	matches, err := s.repo.VisualWindowMatches(ctx, "unused", nil, universe, limit)
	return convertModalityMatches(matches), err
}

func (s *sqliteModalityStore) AudioWindows(ctx context.Context, spaceID string, vector []float32, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	matches, err := s.repo.AudioWindowMatches(ctx, spaceID, vector, universe, limit)
	return convertModalityMatches(matches), err
}

func (s *sqliteModalityStore) SoundEvents(ctx context.Context, labels []string, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	matches, err := s.repo.SoundEventMatches(ctx, labels, universe, limit)
	return convertModalityMatches(matches), err
}

func (s *sqliteModalityStore) SpeakerTurns(ctx context.Context, filter search.SpeakerFilter, universe map[string]bool, limit int) ([]search.ModalityWindow, error) {
	if filter.ProfileID != "" {
		return nil, fmt.Errorf("speaker profile filtering requires an authorised profile search")
	}
	matches, err := s.repo.SpeakerTurnMatches(ctx, filter.AssetID, filter.DiarizationGenerationID, filter.LocalSpeakerID, universe, limit)
	return convertModalityMatches(matches), err
}

func convertModalityMatches(matches []domain.ModalityMatch) []search.ModalityWindow {
	out := make([]search.ModalityWindow, 0, len(matches))
	for _, match := range matches {
		out = append(out, search.ModalityWindow{
			AssetID:        match.AssetID,
			ShotID:         match.ShotID,
			RecordID:       match.RecordID,
			Modality:       string(match.Modality),
			StartMS:        match.StartMS,
			EndMS:          match.EndMS,
			ShotStartMS:    match.ShotStartMS,
			ShotEndMS:      match.ShotEndMS,
			Score:          match.Score,
			Label:          match.Label,
			EvidenceStatus: match.EvidenceStatus,
			Confidence:     match.Confidence,
		})
	}
	return out
}

// TestMultimodalSearchJoinsShotsUnderFacet exercises the composed pipeline
// against real migrations: a facet universe containing one shot, a 5–15 s
// sound event spanning two shots, and the eligible overlapping shot returned
// with its own range while the event interval stays unaltered.
func TestMultimodalSearchJoinsShotsUnderFacet(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "multimodal-search.db"))
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
	if err := repo.ReplaceAssetShots(ctx, "asset-e2e", "", fixtureShots(), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,space_id,shot_set_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at,model_id,model_revision,backend)
		VALUES('gen-sound','asset-e2e','sound_events','in','cfg-sound',NULL,'','committed','[]','[]',?,?,'test/model','rev-1','cpu')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO modality_heads(asset_id,operation,config_hash,generation_id,updated_at)
		VALUES('asset-e2e','sound_events','cfg-sound','gen-sound',?)`, now); err != nil {
		t.Fatal(err)
	}
	// The label is the canonical class id the sound-event vocabulary emits,
	// so the composed search path matches "rain" through SoundLabelVocabulary.
	if _, err := repo.db.ExecContext(ctx, `
		INSERT INTO audio_events(id,generation_id,asset_id,start_ms,end_ms,event_label,confidence,raw_score,evidence_status,boundary_resolution_ms,created_at)
		VALUES('event-rain','gen-sound','asset-e2e',5000,16000,'/m/06mb1',NULL,0.9,'possible',0,?)`, now); err != nil {
		t.Fatal(err)
	}
	minDuration := int64(11000)
	opts := search.DefaultOptions()
	opts.ModalityStore = &sqliteModalityStore{repo: repo}
	svc := search.NewService(repo, opts)

	response, err := svc.Search(ctx, search.SearchRequest{
		Query: "rain", Modalities: []string{search.ModalitySoundEvent},
		Facets: domain.FacetFilter{MinDurationMS: &minDuration},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].ShotID != "shot-2" {
		t.Fatalf("results=%+v, want only the facet-eligible overlapping shot", response.Results)
	}
	result := response.Results[0]
	if result.StartMS != 15000 || result.EndMS != 30000 {
		t.Fatalf("result bounds=%d-%d, want canonical shot bounds", result.StartMS, result.EndMS)
	}
	if len(result.Matches) != 1 || result.Matches[0].RecordID != "event-event-rain" || result.Matches[0].StartMS != 5000 || result.Matches[0].EndMS != 16000 {
		t.Fatalf("matches=%+v, want the unaltered 5–16s event interval", result.Matches)
	}
	if len(result.Coverage) != 1 || result.Coverage[0].State != "complete" {
		t.Fatalf("coverage=%+v, want complete for the untruncated pool", result.Coverage)
	}

	// The same request through the temporal path: the event spanning two
	// shots satisfies the condition once per asset; only the eligible shot
	// enters the candidate pool.
	response, err = svc.Search(ctx, search.SearchRequest{
		Query: "rain", Facets: domain.FacetFilter{MinDurationMS: &minDuration},
		Temporal: &search.TemporalQuery{Conditions: []search.TemporalCondition{
			{ID: "rain", Modality: search.ModalitySoundEvent, Query: "rain"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].ShotID != "shot-2" {
		t.Fatalf("temporal results=%+v, want the eligible overlapping shot", response.Results)
	}
	if len(response.Results[0].Matches) != 1 || response.Results[0].Matches[0].ConditionID != "rain" {
		t.Fatalf("temporal matches=%+v", response.Results[0].Matches)
	}

	// A speaker filter alone must restrict results to the speaker's shots.
	insertSpeakerFixture(t, ctx, repo, now)
	speaker := search.SpeakerFilter{AssetID: "asset-e2e", DiarizationGenerationID: "gen-diar", LocalSpeakerID: "speaker-1"}
	response, err = svc.Search(ctx, search.SearchRequest{
		Query:         "action",
		SpeakerFilter: &speaker,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range response.Results {
		if item.ShotID != "shot-1" && item.ShotID != "shot-2" {
			t.Fatalf("speaker filter leaked a shot without turns: %+v", item)
		}
	}
}
