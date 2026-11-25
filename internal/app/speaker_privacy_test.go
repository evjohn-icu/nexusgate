package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

// speakerPrivacyFixture13 builds a real SQLite service with consent plumbing
// wired but no local inference executor, which is the Worker-only shape: the
// Hub must be able to queue biometric extraction it cannot run itself.
func speakerPrivacyFixture13(t *testing.T) (*Service, *sqlite.Repository) {
	t.Helper()
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
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('other','fp2',100,'discovered','` + now + `','` + now + `')`,
		`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc','asset','root','clip.mp4','/media/clip.mp4',1,1,1,'` + now + `')`,
		`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('asset','{}','{}','{}','test','` + now + `',10000)`,
		`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('other','{}','{}','{}','test','` + now + `',10000)`,
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
	return service, repo
}

func seedDiarizationGeneration13(t *testing.T, repo *sqlite.Repository, generationID, assetID, operation, state string, turns []domain.SpeakerTurnRow) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := repo.DB().ExecContext(ctx, `INSERT INTO modality_generations(id,asset_id,operation,input_hash,config_hash,state,processed_intervals_json,failure_intervals_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		generationID, assetID, operation, "input-"+generationID, "config-"+generationID, state, "[]", "[]", now, now); err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		if _, err := repo.DB().ExecContext(ctx, `INSERT INTO speaker_turns(id,generation_id,asset_id,start_ms,end_ms,local_speaker_id,confidence,overlap,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			turn.ID, generationID, turn.AssetID, turn.StartMS, turn.EndMS, turn.LocalSpeakerID, turn.Confidence, turn.Overlap, now); err != nil {
			t.Fatal(err)
		}
	}
}

func speakerTurn13(id, assetID, speaker string, start, end int64, overlap bool) domain.SpeakerTurnRow {
	return domain.SpeakerTurnRow{ID: id, GenerationID: "", AssetID: assetID, StartMS: start, EndMS: end, LocalSpeakerID: speaker, Overlap: overlap}
}

func biometricRowCounts13(t *testing.T, repo *sqlite.Repository) (profiles, sources, tasks, embeddings int) {
	t.Helper()
	ctx := context.Background()
	for _, probe := range []struct {
		query string
		out   *int
	}{
		{`SELECT COUNT(*) FROM speaker_profiles`, &profiles},
		{`SELECT COUNT(*) FROM speaker_profile_sources`, &sources},
		{`SELECT COUNT(*) FROM inference_tasks WHERE operation='speaker_embedding'`, &tasks},
		{`SELECT COUNT(*) FROM speaker_embeddings`, &embeddings},
	} {
		if err := repo.DB().QueryRowContext(ctx, probe.query).Scan(probe.out); err != nil {
			t.Fatal(err)
		}
	}
	return profiles, sources, tasks, embeddings
}

// TestCreateSpeakerProfileRejectsInvalidRequests pins the eligibility checks:
// every invalid request is rejected before any profile row exists.
func TestCreateSpeakerProfileRejectsInvalidRequests(t *testing.T) {
	ctx := context.Background()

	t.Run("consent disabled", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		seedDiarizationGeneration13(t, repo, "gen", "asset", string(domain.OpDiarization), "committed", []domain.SpeakerTurnRow{
			speakerTurn13("t1", "asset", "spk-1", 0, 3500, false),
		})
		_, err := service.CreateSpeakerProfile(ctx, "asset", "gen", "spk-1")
		if !errors.Is(err, ErrSpeakerConsentRequired) {
			t.Fatalf("disabled consent error=%v, want ErrSpeakerConsentRequired", err)
		}
		profiles, sources, tasks, _ := biometricRowCounts13(t, repo)
		if profiles != 0 || sources != 0 || tasks != 0 {
			t.Fatalf("disabled consent persisted profiles=%d sources=%d tasks=%d", profiles, sources, tasks)
		}
	})

	t.Run("unknown generation", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		_, err := service.CreateSpeakerProfile(ctx, "asset", "gen-missing", "spk-1")
		if err == nil || !strings.Contains(err.Error(), "does not exist or is not committed") {
			t.Fatalf("unknown generation error=%v", err)
		}
		profiles, sources, tasks, _ := biometricRowCounts13(t, repo)
		if profiles != 0 || sources != 0 || tasks != 0 {
			t.Fatalf("unknown generation persisted rows profiles=%d sources=%d tasks=%d", profiles, sources, tasks)
		}
	})

	t.Run("generation not committed", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		seedDiarizationGeneration13(t, repo, "gen-staged", "asset", string(domain.OpDiarization), "staged", []domain.SpeakerTurnRow{
			speakerTurn13("t1", "asset", "spk-1", 0, 3500, false),
		})
		_, err := service.CreateSpeakerProfile(ctx, "asset", "gen-staged", "spk-1")
		if err == nil || !strings.Contains(err.Error(), "does not exist or is not committed") {
			t.Fatalf("staged generation error=%v", err)
		}
		profiles, _, _, _ := biometricRowCounts13(t, repo)
		if profiles != 0 {
			t.Fatalf("staged generation persisted %d profiles", profiles)
		}
	})

	t.Run("cross-asset generation", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		seedDiarizationGeneration13(t, repo, "gen-other", "other", string(domain.OpDiarization), "committed", []domain.SpeakerTurnRow{
			speakerTurn13("t1", "other", "spk-1", 0, 3500, false),
		})
		_, err := service.CreateSpeakerProfile(ctx, "asset", "gen-other", "spk-1")
		if err == nil || !strings.Contains(err.Error(), "belongs to asset") {
			t.Fatalf("cross-asset generation error=%v", err)
		}
		profiles, _, _, _ := biometricRowCounts13(t, repo)
		if profiles != 0 {
			t.Fatalf("cross-asset generation persisted %d profiles", profiles)
		}
	})

	t.Run("generation is not diarization", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		seedDiarizationGeneration13(t, repo, "gen-events", "asset", string(domain.OpSoundEvents), "committed", nil)
		_, err := service.CreateSpeakerProfile(ctx, "asset", "gen-events", "spk-1")
		if err == nil || !strings.Contains(err.Error(), "not diarization") {
			t.Fatalf("sound-events generation error=%v", err)
		}
		profiles, _, _, _ := biometricRowCounts13(t, repo)
		if profiles != 0 {
			t.Fatalf("non-diarization generation persisted %d profiles", profiles)
		}
	})

	t.Run("speaker with no eligible samples", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		seedDiarizationGeneration13(t, repo, "gen-thin", "asset", string(domain.OpDiarization), "committed", []domain.SpeakerTurnRow{
			speakerTurn13("t1", "asset", "spk-1", 0, 2000, false),   // too short
			speakerTurn13("t2", "asset", "spk-1", 3000, 8000, true), // overlapping
			speakerTurn13("t3", "asset", "spk-2", 0, 3500, false),   // other speaker
		})
		_, err := service.CreateSpeakerProfile(ctx, "asset", "gen-thin", "spk-1")
		if err == nil || !strings.Contains(err.Error(), "no eligible clean speech") {
			t.Fatalf("thin speaker error=%v", err)
		}
		profiles, sources, tasks, _ := biometricRowCounts13(t, repo)
		if profiles != 0 || sources != 0 || tasks != 0 {
			t.Fatalf("thin speaker persisted rows profiles=%d sources=%d tasks=%d", profiles, sources, tasks)
		}
	})

	t.Run("worker-only service queues eligible extraction", func(t *testing.T) {
		service, repo := speakerPrivacyFixture13(t)
		if service.inferenceExec != nil {
			t.Fatal("fixture must run without a local inference executor")
		}
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		seedDiarizationGeneration13(t, repo, "gen-ok", "asset", string(domain.OpDiarization), "committed", []domain.SpeakerTurnRow{
			speakerTurn13("t1", "asset", "spk-1", 0, 3500, false),
			speakerTurn13("t2", "asset", "spk-1", 4000, 5000, false), // too short
			speakerTurn13("t3", "asset", "spk-1", 5000, 9000, true),  // overlapping
			speakerTurn13("t4", "asset", "spk-2", 0, 3500, false),    // other speaker
		})
		profile, err := service.CreateSpeakerProfile(ctx, "asset", "gen-ok", "spk-1")
		if err != nil {
			t.Fatal(err)
		}
		profiles, sources, tasks, embeddings := biometricRowCounts13(t, repo)
		if profiles != 1 || sources != 1 || tasks != 1 {
			t.Fatalf("happy path rows profiles=%d sources=%d tasks=%d", profiles, sources, tasks)
		}
		if embeddings != 0 {
			t.Fatalf("happy path published %d embeddings before any run", embeddings)
		}
		var request domain.InferenceRequest
		var encoded string
		if err := repo.DB().QueryRowContext(ctx, `SELECT request_json FROM inference_tasks WHERE operation='speaker_embedding'`).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(encoded), &request); err != nil {
			t.Fatal(err)
		}
		if request.Speaker == nil || request.Speaker.Interval.StartMS != 0 || request.Speaker.Interval.EndMS != 3500 {
			t.Fatalf("queued request interval=%+v, want the single eligible turn", request.Speaker)
		}
		if request.Speaker.ProfileID != profile.ID || request.Speaker.DiarizationGenerationID != "gen-ok" {
			t.Fatalf("queued request provenance=%+v", request.Speaker)
		}
		var jobState string
		if err := repo.DB().QueryRowContext(ctx, `SELECT state FROM jobs WHERE asset_id='asset' AND job_type='inference'`).Scan(&jobState); err != nil {
			t.Fatal(err)
		}
		if jobState != string(domain.JobPending) {
			t.Fatalf("queued job state=%q, want pending", jobState)
		}
	})
}

// TestSpeakerCommitDigestIdempotence pins the Hub-side fence end to end: a
// lost response resubmits the same bytes and is answered idempotently, a
// different result under the same operation id mutates nothing, and staging
// interrupted before publication is repaired from a valid submission.
func TestSpeakerCommitDigestIdempotence(t *testing.T) {
	ctx := context.Background()

	newCommittedJob := func(t *testing.T, key, operationID string) (*Service, *sqlite.Repository, string, domain.InferenceRequest, domain.InferenceResult) {
		service, repo := speakerPrivacyFixture13(t)
		if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
			t.Fatal(err)
		}
		profile := domain.SpeakerProfile{ID: "profile-" + key, Generation: 1, State: "active", AssetID: "asset"}
		if _, err := repo.CreateSpeakerProfileWithTasks(ctx, profile, []domain.SpeakerProfileSource{{
			ProfileID: profile.ID, AssetID: "asset", DiarizationGenerationID: "gen-" + key, LocalSpeakerID: "spk-1",
		}}, nil, 40, mustConsent13(t, repo).Generation); err != nil {
			t.Fatal(err)
		}
		request := domain.InferenceRequest{
			SchemaVersion: domain.InferenceSchemaVersion, OperationID: operationID,
			Operation: domain.OpSpeakerEmbedding, ModelID: "fixture-speaker", ModelRevision: "rev-1",
			SpaceID: "space-" + key, InputHash: "input-" + key, ConfigHash: "config-" + key, TimeoutMS: 60000,
			Input: domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "asset", DurationMS: 10000},
			Speaker: &domain.SpeakerSettings{
				ProfileID: profile.ID, DiarizationGenerationID: "gen-" + key,
				Interval: &domain.TimeInterval{StartMS: 0, EndMS: 3000},
			},
		}
		result := domain.InferenceResult{
			SchemaVersion: request.SchemaVersion, OperationID: operationID,
			Operation: request.Operation, ModelID: request.ModelID, ModelRevision: request.ModelRevision,
			SpaceID: request.SpaceID, InputHash: request.InputHash, ConfigHash: request.ConfigHash,
			Backend: "cpu", Embeddings: []domain.EmbeddingRecord{{StartMS: 0, EndMS: 3000, Vector: []float32{1, 0}}},
		}
		return service, repo, leaseSpeakerJob13(t, repo, request, key), request, result
	}

	t.Run("repeat bytes after lost response", func(t *testing.T) {
		service, repo, jobID, request, result := newCommittedJob(t, "repeat", "op-repeat")
		if err := service.commitSpeakerInferenceResult(ctx, jobID, "owner", request, result); err != nil {
			t.Fatal(err)
		}
		err := service.commitSpeakerInferenceResult(ctx, jobID, "owner", request, result)
		if !errors.Is(err, ErrInferenceAlreadyCommitted) {
			t.Fatalf("repeat commit error=%v, want ErrInferenceAlreadyCommitted", err)
		}
		if _, _, _, embeddings := biometricRowCounts13(t, repo); embeddings != 1 {
			t.Fatalf("repeat published %d embeddings, want exactly the original one", embeddings)
		}
		if got := purgeCount13(t, repo); got != 0 {
			t.Fatalf("repeat left %d purge obligations, want none", got)
		}
	})

	t.Run("different bytes under one operation id", func(t *testing.T) {
		service, repo, jobID, request, result := newCommittedJob(t, "conflict", "op-conflict")
		if err := service.commitSpeakerInferenceResult(ctx, jobID, "owner", request, result); err != nil {
			t.Fatal(err)
		}
		conflicting := result
		conflicting.Embeddings = []domain.EmbeddingRecord{{StartMS: 0, EndMS: 3000, Vector: []float32{0, 1}}}
		err := service.commitSpeakerInferenceResult(ctx, jobID, "owner", request, conflicting)
		if !errors.Is(err, domain.ErrPermanentFailure) {
			t.Fatalf("conflicting result error=%v, want permanent failure", err)
		}
		if _, _, _, embeddings := biometricRowCounts13(t, repo); embeddings != 1 {
			t.Fatalf("conflicting result left %d embeddings, want the original one untouched", embeddings)
		}
	})

	t.Run("interrupted staging is repaired", func(t *testing.T) {
		service, repo, jobID, request, result := newCommittedJob(t, "repair", "op-repair")
		digest, err := remote.DigestResult(result)
		if err != nil {
			t.Fatal(err)
		}
		// A previous attempt registered the obligation and the staging row but
		// its ciphertext never became readable (interrupted publication).
		ghostRef := "biometric-op-repair-ghost"
		if err := repo.AddPurgeRef(ctx, ghostRef); err != nil {
			t.Fatal(err)
		}
		if err := repo.PutSpeakerEmbeddingStaging(ctx, domain.SpeakerEmbeddingStaging{
			OperationID: request.OperationID, JobID: jobID, ProfileID: request.Speaker.ProfileID,
			ProfileGeneration: 1, ConsentGeneration: mustConsent13(t, repo).Generation,
			SecretRef: ghostRef, ResultDigest: digest,
		}); err != nil {
			t.Fatal(err)
		}
		if err := service.commitSpeakerInferenceResult(ctx, jobID, "owner", request, result); err != nil {
			t.Fatal(err)
		}
		records, err := repo.ListSpeakerEmbeddings(ctx, request.Speaker.ProfileID)
		if err != nil || len(records) != 1 {
			t.Fatalf("repaired commit published %d records err=%v", len(records), err)
		}
		if records[0].SecretRef == ghostRef {
			t.Fatal("repaired commit reused the unreadable ghost ciphertext")
		}
		if got := purgeCount13(t, repo); got != 1 {
			t.Fatalf("purge obligations=%d, want the ghost reference retained for the next drain", got)
		}
	})
}

func mustConsent13(t *testing.T, repo *sqlite.Repository) domain.SpeakerConsent {
	t.Helper()
	consent, err := repo.GetSpeakerConsent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !consent.Enabled {
		t.Fatal("fixture requires enabled consent")
	}
	return consent
}

func leaseSpeakerJob13(t *testing.T, repo *sqlite.Repository, request domain.InferenceRequest, key string) string {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := repo.EnqueueInferenceTask(ctx, nil, domain.InferenceTask{
		AssetID: request.Input.AssetID, TaskKey: "speaker-task-" + key, Operation: request.Operation,
		RequestJSON: string(encoded), ConfigHash: request.ConfigHash, ModelRevision: request.ModelRevision,
	}, 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DB().ExecContext(ctx, `UPDATE jobs SET state='running',attempt_count=1,lease_owner='owner',lease_expires_at=? WHERE id=?`,
		time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func purgeCount13(t *testing.T, repo *sqlite.Repository) int {
	t.Helper()
	var count int
	if err := repo.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM biometric_purge_refs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestDrainBiometricPurgeBoundedAndResilient seeds 257 deletable references,
// one deletion failure and live staged plus canonical references. The drain
// must delete every eligible reference on both sides of the failure and across
// the 256-batch boundary, keep only the failed deletion for retry, clear the
// erroneous obligation on canonical ciphertext and leave a live staged job's
// reference alone.
func TestDrainBiometricPurgeBoundedAndResilient(t *testing.T) {
	ctx := context.Background()
	service, repo := speakerPrivacyFixture13(t)
	consent, err := repo.SetSpeakerConsent(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	profile := domain.SpeakerProfile{ID: "profile-purge", Generation: 1, State: "active", AssetID: "asset"}
	if _, err := repo.CreateSpeakerProfileWithTasks(ctx, profile, []domain.SpeakerProfileSource{{
		ProfileID: profile.ID, AssetID: "asset", DiarizationGenerationID: "gen-purge", LocalSpeakerID: "spk-1",
	}}, nil, 40, consent.Generation); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddSpeakerEmbedding(ctx, domain.SpeakerEmbedding{
		ProfileID: profile.ID, AssetID: "asset", GenerationID: "gen-purge", SpaceID: "space-purge",
		StartMS: 0, EndMS: 3000, SpeechDurationMS: 3000, QualityJSON: `{}`,
		SecretRef: "biometric-canonical-ref", ConsentGeneration: consent.Generation, ProfileGeneration: profile.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion, OperationID: "op-purge",
		Operation: domain.OpSpeakerEmbedding, ModelID: "m", ModelRevision: "rev-1",
		SpaceID: "space-purge", InputHash: "input-purge", ConfigHash: "config-purge", TimeoutMS: 60000,
		Input: domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "asset", DurationMS: 10000},
		Speaker: &domain.SpeakerSettings{
			ProfileID: profile.ID, DiarizationGenerationID: "gen-purge",
			Interval: &domain.TimeInterval{StartMS: 0, EndMS: 3000},
		},
	}
	jobID := leaseSpeakerJob13(t, repo, request, "purge")
	// Keep the job pending: a queued staged extraction under unrevoked consent
	// and an active profile retains its reference.
	if _, err := repo.DB().ExecContext(ctx, `UPDATE jobs SET state='pending',lease_owner=NULL,lease_expires_at=NULL WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutSpeakerEmbeddingStaging(ctx, domain.SpeakerEmbeddingStaging{
		OperationID: request.OperationID, JobID: jobID, ProfileID: profile.ID,
		ProfileGeneration: profile.Generation, ConsentGeneration: consent.Generation,
		SecretRef: "biometric-live-staged-ref", ResultDigest: "sha256:live",
	}); err != nil {
		t.Fatal(err)
	}

	var deletable []string
	for i := range 101 {
		deletable = append(deletable, fmt.Sprintf("biometric-a-del-%03d", i))
	}
	deletable = append(deletable, "biometric-fail-ref")
	for i := range 156 {
		deletable = append(deletable, fmt.Sprintf("biometric-z-del-%03d", i))
	}
	obligations := append([]string{"biometric-canonical-ref", "biometric-live-staged-ref"}, deletable...)
	for _, ref := range obligations {
		if err := repo.AddPurgeRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	payload := `{"dtype":"float32","dimension":2,"vector":[1,0]}`
	for _, ref := range append([]string{"biometric-canonical-ref", "biometric-live-staged-ref", "biometric-fail-ref"}, deletable...) {
		if err := service.secrets.Put(ref, payload); err != nil {
			t.Fatal(err)
		}
	}
	biometricDeleteHook = func(svc *Service, ref string) error {
		if ref == "biometric-fail-ref" {
			return errors.New("injected deletion failure")
		}
		return svc.secrets.Delete(ref)
	}
	t.Cleanup(func() { biometricDeleteHook = nil })

	drainErr := service.DrainBiometricPurge(ctx)
	if drainErr == nil || !strings.Contains(drainErr.Error(), "injected deletion failure") {
		t.Fatalf("drain error=%v, want the failed deletion reported", drainErr)
	}
	remaining, err := repo.ListPurgeRefs(ctx, "", 16)
	if err != nil {
		t.Fatal(err)
	}
	// The live staged reference is retained (its commit will clear it); the
	// only failed deletion is kept for retry.
	if len(remaining) != 2 || remaining[0] != "biometric-fail-ref" || remaining[1] != "biometric-live-staged-ref" {
		t.Fatalf("remaining worklist=%v, want the failed deletion plus the retained live staging", remaining)
	}
	var lastError string
	if err := repo.DB().QueryRowContext(ctx, `SELECT last_error_code FROM biometric_purge_refs WHERE secret_ref='biometric-fail-ref'`).Scan(&lastError); err != nil || lastError != "secret_delete_failed" {
		t.Fatalf("failed deletion error_code=%q err=%v", lastError, err)
	}
	for _, ref := range deletable {
		if ref == "biometric-fail-ref" {
			continue
		}
		if _, ok, resolveErr := service.secrets.Resolve(ref); resolveErr != nil || ok {
			t.Fatalf("deletable %s survived the drain: ok=%v err=%v", ref, ok, resolveErr)
		}
	}
	if _, ok, _ := service.secrets.Resolve("biometric-canonical-ref"); !ok {
		t.Fatal("canonical ciphertext was deleted while still referenced")
	}
	worklist, err := repo.ListPurgeRefs(ctx, "", 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range worklist {
		if ref == "biometric-canonical-ref" {
			t.Fatal("canonical obligation was not cleared by the drain")
		}
	}
	if _, ok, _ := service.secrets.Resolve("biometric-live-staged-ref"); !ok {
		t.Fatal("live staged ciphertext was deleted while its job is queued")
	}
	if got := purgeCount13(t, repo); got != 2 {
		t.Fatalf("purge worklist=%d, want the failed deletion plus the retained live staging", got)
	}
	staging, err := repo.GetSpeakerEmbeddingStaging(ctx, request.OperationID)
	if err != nil || staging.SecretRef != "biometric-live-staged-ref" {
		t.Fatalf("live staging row disturbed: %+v err=%v", staging, err)
	}

	// A later drain without the injected fault completes the failed deletion;
	// the live staged reference stays until its commit clears it.
	biometricDeleteHook = nil
	if err := service.DrainBiometricPurge(ctx); err != nil {
		t.Fatalf("retry drain error=%v", err)
	}
	remaining, err = repo.ListPurgeRefs(ctx, "", 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0] != "biometric-live-staged-ref" {
		t.Fatalf("worklist after retry=%v, want only the retained live staging", remaining)
	}
	if _, ok, _ := service.secrets.Resolve("biometric-fail-ref"); ok {
		t.Fatal("failed deletion was not retried on the next drain")
	}
}

// TestDisableConsentTerminalizesAndPurges pins the destructive consent path:
// profiles are tombstoned, extraction jobs take the shared terminal
// cancellation shape, staging metadata goes with the obligations and every
// ciphertext is destroyed.
func TestDisableConsentTerminalizesAndPurges(t *testing.T) {
	ctx := context.Background()
	service, repo := speakerPrivacyFixture13(t)
	consent, err := repo.SetSpeakerConsent(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	profile := domain.SpeakerProfile{ID: "profile-disable", Generation: 1, State: "active", AssetID: "asset"}
	if _, err := repo.CreateSpeakerProfileWithTasks(ctx, profile, []domain.SpeakerProfileSource{{
		ProfileID: profile.ID, AssetID: "asset", DiarizationGenerationID: "gen-disable", LocalSpeakerID: "spk-1",
	}}, nil, 40, consent.Generation); err != nil {
		t.Fatal(err)
	}
	request := domain.InferenceRequest{
		SchemaVersion: domain.InferenceSchemaVersion, OperationID: "op-disable",
		Operation: domain.OpSpeakerEmbedding, ModelID: "m", ModelRevision: "rev-1",
		SpaceID: "space-disable", InputHash: "input-disable", ConfigHash: "config-disable", TimeoutMS: 60000,
		Input: domain.InferenceInput{Kind: domain.InputKindAsset, AssetID: "asset", DurationMS: 10000},
		Speaker: &domain.SpeakerSettings{
			ProfileID: profile.ID, DiarizationGenerationID: "gen-disable",
			Interval: &domain.TimeInterval{StartMS: 0, EndMS: 3000},
		},
	}
	jobID := leaseSpeakerJob13(t, repo, request, "disable")
	if err := repo.PutSpeakerEmbeddingStaging(ctx, domain.SpeakerEmbeddingStaging{
		OperationID: request.OperationID, JobID: jobID, ProfileID: profile.ID,
		ProfileGeneration: profile.Generation, ConsentGeneration: consent.Generation,
		SecretRef: "biometric-disable-ref", ResultDigest: "sha256:disable",
	}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"biometric-disable-ref", "biometric-canonical-disable"} {
		if err := repo.AddPurgeRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.AddSpeakerEmbedding(ctx, domain.SpeakerEmbedding{
		ProfileID: profile.ID, AssetID: "asset", GenerationID: "gen-disable", SpaceID: "space-disable",
		StartMS: 0, EndMS: 3000, SpeechDurationMS: 3000, QualityJSON: `{}`,
		SecretRef: "biometric-canonical-disable", ConsentGeneration: consent.Generation, ProfileGeneration: profile.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	payload := `{"dtype":"float32","dimension":2,"vector":[1,0]}`
	for _, ref := range []string{"biometric-disable-ref", "biometric-canonical-disable"} {
		if err := service.secrets.Put(ref, payload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.SetSpeakerConsent(ctx, false); err != nil {
		t.Fatalf("disable consent error=%v", err)
	}
	profiles, err := repo.ListSpeakerProfiles(ctx)
	if err != nil || len(profiles) != 0 {
		t.Fatalf("profiles after disable=%d err=%v, want none active", len(profiles), err)
	}
	var state, errorCode, stage string
	var terminal int
	if err := repo.DB().QueryRowContext(ctx, `SELECT state,COALESCE(last_error_code,''),COALESCE(current_stage,''),terminal FROM jobs WHERE id=?`, jobID).
		Scan(&state, &errorCode, &stage, &terminal); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.JobFailed) || errorCode != "cancelled" || stage != "cancelled" || terminal != 1 {
		t.Fatalf("queued extraction state=%q code=%q stage=%q terminal=%d, want terminal cancellation", state, errorCode, stage, terminal)
	}
	if got := purgeCount13(t, repo); got != 0 {
		t.Fatalf("purge worklist=%d after disable, want drained", got)
	}
	for _, ref := range []string{"biometric-disable-ref", "biometric-canonical-disable"} {
		if _, ok, _ := service.secrets.Resolve(ref); ok {
			t.Fatalf("ciphertext %s survived consent disable", ref)
		}
	}
	if got, err := repo.GetSpeakerEmbeddingStaging(ctx, request.OperationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("staging row after disable=%+v err=%v, want gone", got, err)
	}
}

// TestSearchSpeakerMatchesPreservesDecisions pins the acceptance case: a
// confirmed or rejected pairing survives a fresh match request unchanged.
func TestSearchSpeakerMatchesPreservesDecisions(t *testing.T) {
	ctx := context.Background()
	service, repo := speakerPrivacyFixture13(t)
	if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
		t.Fatal(err)
	}
	payload := func(vector []float32) string {
		encoded, _ := json.Marshal(struct {
			Dtype     string    `json:"dtype"`
			Dimension int       `json:"dimension"`
			Vector    []float32 `json:"vector"`
		}{"float32", len(vector), vector})
		return string(encoded)
	}
	profileIDs := []string{"profile-alpha", "profile-beta"}
	for index, id := range profileIDs {
		profile := domain.SpeakerProfile{ID: id, Generation: 1, State: "active", AssetID: "asset"}
		if _, err := repo.CreateSpeakerProfileWithTasks(ctx, profile, []domain.SpeakerProfileSource{{
			ProfileID: id, AssetID: "asset", DiarizationGenerationID: "gen-match", LocalSpeakerID: fmt.Sprintf("spk-%d", index+1),
		}}, nil, 40, mustConsent13(t, repo).Generation); err != nil {
			t.Fatal(err)
		}
		ref := "biometric-match-" + id
		if err := service.secrets.Put(ref, payload([]float32{1, 0})); err != nil {
			t.Fatal(err)
		}
		if err := repo.AddSpeakerEmbedding(ctx, domain.SpeakerEmbedding{
			ProfileID: id, AssetID: "asset", GenerationID: "gen-match", SpaceID: "space-match",
			StartMS: 0, EndMS: 3000, SpeechDurationMS: 3000, QualityJSON: `{}`,
			SecretRef: ref, ConsentGeneration: 1, ProfileGeneration: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	associations, err := service.SearchSpeakerMatches(ctx, "profile-alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(associations) != 1 || associations[0].State != domain.AssociationUnknown {
		t.Fatalf("first match returned %+v err=%v, want one unknown pairing", associations, err)
	}
	if err := service.SetSpeakerAssociationState(ctx, associations[0].ID, domain.AssociationConfirmed); err != nil {
		t.Fatal(err)
	}
	associations, err = service.SearchSpeakerMatches(ctx, "profile-alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(associations) != 1 || associations[0].State != domain.AssociationConfirmed {
		t.Fatalf("confirmed pairing degraded to %+v on refresh", associations)
	}
	if err := service.SetSpeakerAssociationState(ctx, associations[0].ID, domain.AssociationRejected); err != nil {
		t.Fatal(err)
	}
	associations, err = service.SearchSpeakerMatches(ctx, "profile-alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(associations) != 1 || associations[0].State != domain.AssociationRejected {
		t.Fatalf("rejected pairing degraded to %+v on refresh", associations)
	}
}

// TestSanitizeSpeakerQuality pins the allowlist: only the three measured
// metrics persist, anything else — including a vector smuggled into the
// quality payload — is dropped or rejected.
func TestSanitizeSpeakerQuality(t *testing.T) {
	t.Run("allowed metrics survive", func(t *testing.T) {
		quality, err := sanitizeSpeakerQuality(`{"clipped_fraction":0.001,"rms_dbfs":-20.5,"speech_duration_ms":3000,"extra":"dropped"}`)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"clipped_fraction", "rms_dbfs", "speech_duration_ms"} {
			if !strings.Contains(quality, key) {
				t.Fatalf("sanitized quality %q lost %s", quality, key)
			}
		}
		if strings.Contains(quality, "extra") {
			t.Fatalf("sanitized quality %q kept a disallowed key", quality)
		}
	})
	t.Run("vector cannot ride along", func(t *testing.T) {
		quality, err := sanitizeSpeakerQuality(`{"clipped_fraction":0,"vector":[0.1,0.2,0.3],"identity":"alice"}`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(quality, "vector") || strings.Contains(quality, "identity") {
			t.Fatalf("sanitized quality %q persisted smuggled content", quality)
		}
	})
	for name, input := range map[string]string{
		"array payload":   `[[0.1,0.2]]`,
		"malformed":       `not json`,
		"fraction range":  `{"clipped_fraction":2}`,
		"dbfs sign":       `{"rms_dbfs":1}`,
		"negative length": `{"speech_duration_ms":-5}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sanitizeSpeakerQuality(input)
			if !errors.Is(err, domain.ErrPermanentFailure) {
				t.Fatalf("quality %q error=%v, want permanent failure", input, err)
			}
		})
	}
	t.Run("empty and null are empty object", func(t *testing.T) {
		for _, input := range []string{"", "  ", "null"} {
			quality, err := sanitizeSpeakerQuality(input)
			if err != nil || quality != "{}" {
				t.Fatalf("quality %q -> %q err=%v, want {}", input, quality, err)
			}
		}
	})
}
