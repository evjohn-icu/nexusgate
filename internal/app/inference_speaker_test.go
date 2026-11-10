package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// TestSpeakerCommitEncryptsBeforePublishing pins the biometric boundary: the
// plaintext vector is encrypted, the deletion obligation exists before the
// ciphertext, and only metadata reaches SQLite.
func TestSpeakerCommitEncryptsBeforePublishing(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "speaker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root','/media','` + now + `','` + now + `')`,
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset','fp',100,'discovered','` + now + `','` + now + `')`,
		`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc','asset','root','clip.mp4','/media/clip.mp4',1,1,1,'` + now + `')`,
		`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('asset','{}','{}','{}','test','` + now + `',10000)`,
	} {
		if _, err := repo.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(repo, config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	consent, err := repo.SetSpeakerConsent(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	space := domain.EmbeddingSpace{
		Modality: domain.ModalitySpeaker, Provider: "fixture", ModelID: "fixture-speaker",
		ModelRevision: "rev-1", Dimension: 2, Normalization: "l2",
		PreprocessingJSON: `{}`, RuntimeDigest: "runtime",
	}
	spaceID, err := domain.CanonicalSpaceID(space)
	if err != nil {
		t.Fatal(err)
	}
	space.ID = spaceID
	if err := repo.UpsertEmbeddingSpace(ctx, space); err != nil {
		t.Fatal(err)
	}
	profile := domain.SpeakerProfile{ID: "profile-1", Generation: 1, State: "active", AssetID: "asset"}
	if err := repo.CreateSpeakerProfile(ctx, profile, []domain.SpeakerProfileSource{{
		ProfileID: profile.ID, AssetID: "asset", DiarizationGenerationID: "diarization-1", LocalSpeakerID: "spk-1",
	}}); err != nil {
		t.Fatal(err)
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion, OperationID: "operation-1",
		Operation: domain.OpSpeakerEmbedding, ModelID: space.ModelID, ModelRevision: space.ModelRevision,
		SpaceID: spaceID, InputHash: "input", ConfigHash: "config", TimeoutMS: 60000,
		Input: domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "asset", DurationMS: 10000},
		Speaker: &domain.SpeakerSettings{
			ProfileID: profile.ID, DiarizationGenerationID: "diarization-1",
			Interval: &domain.TimeInterval{StartMS: 1000, EndMS: 4000},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(request)
	jobID, err := repo.EnqueueInferenceTask(ctx, nil, domain.InferenceTask{
		AssetID: "asset", TaskKey: "speaker-task", Operation: domain.OpSpeakerEmbedding,
		RequestJSON: string(encoded), ConfigHash: request.ConfigHash, ModelRevision: request.ModelRevision,
	}, 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=1,lease_owner='owner',lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
	result := domain.InferenceResult{
		SchemaVersion: request.SchemaVersion, OperationID: request.OperationID,
		Operation: request.Operation, ModelID: request.ModelID, ModelRevision: request.ModelRevision,
		SpaceID: request.SpaceID, InputHash: request.InputHash, ConfigHash: request.ConfigHash,
		Backend: "cpu", Embeddings: []domain.EmbeddingRecord{{
			StartMS: 1000, EndMS: 4000, Vector: []float32{1, 0},
		}},
	}
	if err := service.commitSpeakerInferenceResult(ctx, jobID, "owner", request, result); err != nil {
		t.Fatal(err)
	}
	records, err := repo.ListSpeakerEmbeddings(ctx, profile.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("speaker embeddings = %d err=%v", len(records), err)
	}
	if records[0].ConsentGeneration != consent.Generation || records[0].GenerationID != "diarization-1" {
		t.Fatalf("published provenance = %+v", records[0])
	}
	raw, ok, err := service.secrets.Resolve(records[0].SecretRef)
	if err != nil || !ok {
		t.Fatalf("ciphertext missing: ok=%v err=%v", ok, err)
	}
	var payload struct {
		Dtype     string    `json:"dtype"`
		Dimension int       `json:"dimension"`
		Vector    []float32 `json:"vector"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.Dtype != "float32" || payload.Dimension != 2 {
		t.Fatalf("stored payload = %s err=%v", raw, err)
	}
	if _, ok, err := service.secrets.Resolve(records[0].SecretRef + "-missing"); ok || err != nil {
		t.Fatalf("unexpected secret lookup result ok=%v err=%v", ok, err)
	}
}
