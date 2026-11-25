package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// TestCommitSpeakerEmbeddingDigestIdempotence pins the result-digest fence: a
// repeat of the committed bytes is a read-only success, a different result
// under the same operation id mutates nothing, and a legacy commit without a
// recorded digest is rejected rather than guessed.
func TestCommitSpeakerEmbeddingDigestIdempotence(t *testing.T) {
	t.Run("same digest is read-only success", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "idempotent")
		if err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records); err != nil {
			t.Fatalf("same-digest retry failed: %v", err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != 1 {
			t.Fatalf("speaker embeddings=%d, want exactly 1 after retry", got)
		}
		if got := inferenceTestCount(t, repo, "speaker_embedding_staging", ""); got != 0 {
			t.Fatalf("staging rows=%d, want 0", got)
		}
		if got := inferenceTestCount(t, repo, "biometric_purge_refs", ""); got != 0 {
			t.Fatalf("purge obligations=%d, want 0 after commit", got)
		}
		var digest sql.NullString
		if err := repo.db.QueryRow(`SELECT result_digest FROM inference_tasks WHERE job_id=?`, jobID).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		if !digest.Valid || digest.String != speakerCommitFixtureDigest {
			t.Fatalf("recorded digest=%v, want %q", digest, speakerCommitFixtureDigest)
		}
	})
	t.Run("different digest is rejected without mutation", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "different-digest")
		if err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records); err != nil {
			t.Fatal(err)
		}
		before, err := repo.db.Query(`SELECT id,profile_id,secret_ref FROM speaker_embeddings ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot [][]string
		for before.Next() {
			row := make([]string, 3)
			if err := before.Scan(&row[0], &row[1], &row[2]); err != nil {
				before.Close()
				t.Fatal(err)
			}
			snapshot = append(snapshot, row)
		}
		before.Close()
		err = repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, "sha256:different-bytes", records)
		if !errors.Is(err, domain.ErrPermanentFailure) {
			t.Fatalf("different-digest retry error=%v, want permanent failure", err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != 1 {
			t.Fatalf("speaker embeddings=%d after rejection, want 1", got)
		}
		after, err := repo.db.Query(`SELECT id,profile_id,secret_ref FROM speaker_embeddings ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer after.Close()
		var replay [][]string
		for after.Next() {
			row := make([]string, 3)
			if err := after.Scan(&row[0], &row[1], &row[2]); err != nil {
				t.Fatal(err)
			}
			replay = append(replay, row)
		}
		if fmt.Sprint(snapshot) != fmt.Sprint(replay) {
			t.Fatalf("rows mutated by rejected retry: %v vs %v", snapshot, replay)
		}
	})
	t.Run("legacy succeeded row without digest is rejected", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "legacy-digest")
		if err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.Exec(`UPDATE inference_tasks SET result_digest=NULL WHERE job_id=?`, jobID); err != nil {
			t.Fatal(err)
		}
		err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records)
		if !errors.Is(err, domain.ErrPermanentFailure) {
			t.Fatalf("legacy retry error=%v, want permanent failure", err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != 1 {
			t.Fatalf("speaker embeddings=%d, want exactly the original 1", got)
		}
	})
	t.Run("staged digest mismatch is rejected", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "staged-mismatch")
		if _, err := repo.db.Exec(`UPDATE speaker_embedding_staging SET result_digest='sha256:other-bytes'`); err != nil {
			t.Fatal(err)
		}
		err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records)
		if !errors.Is(err, domain.ErrPermanentFailure) {
			t.Fatalf("staged-digest mismatch error=%v, want permanent failure", err)
		}
		if got := inferenceTestCount(t, repo, "speaker_embeddings", ""); got != 0 {
			t.Fatalf("mismatched staging published %d rows, want 0", got)
		}
		if state := queryJobState(t, repo, jobID); state != string(domain.JobRunning) {
			t.Fatalf("job state=%q, want still running", state)
		}
	})
	t.Run("stale completion leaves obligation durable", func(t *testing.T) {
		repo, jobID, consentGeneration, profileGeneration, records := prepareSpeakerEmbeddingCommit(t, "stale-complete")
		// The obligation is registered at stage time in production; seed the
		// same state here so the stale commit cannot clear it.
		if _, err := repo.db.Exec(`INSERT INTO biometric_purge_refs(secret_ref,created_at) VALUES(?,?)`, records[0].SecretRef, formatTime(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, formatTime(time.Now().UTC().Add(-time.Second)), jobID); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitSpeakerEmbedding(context.Background(), jobID, "speaker-owner", consentGeneration, profileGeneration, speakerCommitFixtureDigest, records); !errors.Is(err, domain.ErrJobLeaseLost) {
			t.Fatalf("stale completion error=%v, want ErrJobLeaseLost", err)
		}
		if got := inferenceTestCount(t, repo, "biometric_purge_refs", "secret_ref=?", records[0].SecretRef); got != 1 {
			t.Fatalf("stale completion left %d obligations, want the staged one retained", got)
		}
		if got := inferenceTestCount(t, repo, "speaker_embedding_staging", ""); got != 1 {
			t.Fatalf("staging rows=%d, want the unreferenced obligation's row retained", got)
		}
	})
}

func queryJobState(t *testing.T, repo *Repository, jobID string) string {
	t.Helper()
	var state string
	if err := repo.db.QueryRow(`SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestUpsertSpeakerAssociationPreservesDecisions pins the decision boundary: a
// similarity refresh never rewrites a pairing the operator confirmed or
// rejected, while similarity itself stays current.
func TestUpsertSpeakerAssociationPreservesDecisions(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	ctx := context.Background()
	if _, err := repo.db.Exec(`INSERT INTO speaker_profiles(id,generation,state,created_at) VALUES('p-left',1,'active',?),('p-right',1,'active',?)`, formatTime(time.Now().UTC()), formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	refresh := func(similarity float64, state string) {
		t.Helper()
		if err := repo.UpsertSpeakerAssociation(ctx, domain.SpeakerAssociation{
			LeftProfileID: "p-left", RightProfileID: "p-right", State: state,
			Similarity: similarity, ConsentGeneration: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}
	refresh(0.4, domain.AssociationUnknown)
	if err := repo.SetSpeakerAssociationState(ctx, queryAssociationID(t, repo, "p-left", "p-right"), domain.AssociationConfirmed); err != nil {
		t.Fatal(err)
	}
	refresh(0.9, domain.AssociationUnknown)
	association := scanAssociation(t, repo, "p-left", "p-right")
	if association.State != domain.AssociationConfirmed {
		t.Fatalf("confirmed pairing moved to %q on similarity refresh", association.State)
	}
	if association.Similarity != 0.9 {
		t.Fatalf("similarity=%v, want refreshed 0.9", association.Similarity)
	}
	// Unknown pairings still refresh normally before any decision exists.
	refresh(0.2, domain.AssociationUnknown)
	if association := scanAssociation(t, repo, "p-left", "p-right"); association.State != domain.AssociationConfirmed {
		t.Fatalf("decision lost to a later refresh: %q", association.State)
	}
	if err := repo.SetSpeakerAssociationState(ctx, queryAssociationID(t, repo, "p-left", "p-right"), domain.AssociationRejected); err != nil {
		t.Fatal(err)
	}
	refresh(0.7, domain.AssociationUnknown)
	if association := scanAssociation(t, repo, "p-left", "p-right"); association.State != domain.AssociationRejected {
		t.Fatalf("rejected pairing moved to %q on similarity refresh", association.State)
	}
}

func queryAssociationID(t *testing.T, repo *Repository, left, right string) string {
	t.Helper()
	var id string
	if err := repo.db.QueryRow(`SELECT id FROM speaker_associations WHERE left_profile_id=? AND right_profile_id=?`, left, right).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func scanAssociation(t *testing.T, repo *Repository, left, right string) domain.SpeakerAssociation {
	t.Helper()
	associations, err := repo.ListSpeakerAssociations(context.Background(), left)
	if err != nil {
		t.Fatal(err)
	}
	for _, association := range associations {
		if association.LeftProfileID == left && association.RightProfileID == right {
			return association
		}
	}
	t.Fatalf("association %s/%s not found", left, right)
	return domain.SpeakerAssociation{}
}

// TestListPurgeRefsKeysetPagination pins the bounded, stable ordering the
// purge drain depends on: pages advance by secret_ref, never repeat a row and
// never stall on one.
func TestListPurgeRefsKeysetPagination(t *testing.T) {
	repo, _ := newInferenceRepository(t)
	ctx := context.Background()
	var refs []string
	for i := range 300 {
		refs = append(refs, fmt.Sprintf("biometric-purge-%03d", i))
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if _, err := repo.db.Exec(`INSERT INTO biometric_purge_refs(secret_ref,created_at) VALUES(?,?)`, ref, formatTime(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	after := ""
	for {
		page, err := repo.ListPurgeRefs(ctx, after, 256)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 256 {
			t.Fatalf("page size %d exceeds the bounded batch", len(page))
		}
		seen = append(seen, page...)
		after = page[len(page)-1]
	}
	if len(seen) != len(refs) {
		t.Fatalf("drained %d refs, want all %d", len(seen), len(refs))
	}
	for i, ref := range seen {
		if ref != refs[i] {
			t.Fatalf("page %d got %q, want stable order %q", i, ref, refs[i])
		}
	}
	if page, err := repo.ListPurgeRefs(ctx, refs[len(refs)-1], 256); err != nil || len(page) != 0 {
		t.Fatalf("exhausted worklist returned %v err=%v", page, err)
	}
}

// TestClassifyBiometricPurgeRefs pins the live-state rules the drain applies:
// canonical ciphertext stays while referenced, a staged extraction under
// unrevoked consent and an active profile retains its reference, an obsolete
// staging row for a terminal job is deletable, and a plain orphan is purged.
func TestClassifyBiometricPurgeRefs(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	consent, err := repo.SetSpeakerConsent(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	for _, id := range []string{"profile-live", "profile-dead"} {
		if _, err := repo.db.Exec(`INSERT INTO speaker_profiles(id,generation,state,created_at) VALUES(?,1,'active',?)`, id, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.db.Exec(`INSERT INTO speaker_embeddings(id,profile_id,asset_id,generation_id,space_id,start_ms,end_ms,speech_duration_ms,secret_ref,consent_generation,profile_generation,created_at) VALUES('emb-1','profile-live',?,'gen','space',0,1000,1000,'biometric-canonical',?,1,?)`, assetID, consent.Generation, now); err != nil {
		t.Fatal(err)
	}
	liveTask := inferenceTestRequest(domain.OpSpeakerEmbedding, assetID, "space", "classify-live")
	liveTask.Speaker.ProfileID = "profile-live"
	liveJob := enqueueInferenceTestTask(t, repo, liveTask, "classify-live-key")
	if _, err := repo.db.Exec(`INSERT INTO speaker_embedding_staging(operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref,result_digest,created_at) VALUES(?,?,?,?,?,?,?,?)`, liveTask.OperationID, liveJob, "profile-live", 1, consent.Generation, "biometric-live-staged", "sha256:live", now); err != nil {
		t.Fatal(err)
	}
	deadTask := inferenceTestRequest(domain.OpSpeakerEmbedding, assetID, "space", "classify-dead")
	deadTask.Speaker.ProfileID = "profile-dead"
	deadJob := enqueueInferenceTestTask(t, repo, deadTask, "classify-dead-key")
	leaseInferenceTestJob(t, repo, deadJob, "owner", time.Now().UTC().Add(time.Minute))
	if _, err := repo.db.Exec(`UPDATE jobs SET state='failed',terminal=1,last_error_code='cancelled' WHERE id=?`, deadJob); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO speaker_embedding_staging(operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref,result_digest,created_at) VALUES(?,?,?,?,?,?,?,?)`, deadTask.OperationID, deadJob, "profile-dead", 1, consent.Generation, "biometric-terminal-staged", "sha256:dead", now); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"biometric-canonical", "biometric-live-staged", "biometric-terminal-staged", "biometric-plain-orphan"} {
		if _, err := repo.db.Exec(`INSERT INTO biometric_purge_refs(secret_ref,created_at) VALUES(?,?)`, ref, now); err != nil {
			t.Fatal(err)
		}
	}
	dispositions, err := repo.ClassifyBiometricPurgeRefs(ctx, []string{"biometric-canonical", "biometric-live-staged", "biometric-terminal-staged", "biometric-plain-orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if got := dispositions["biometric-canonical"]; !got.Canonical || got.LiveStaging || got.Staged {
		t.Fatalf("canonical disposition=%+v, want canonical only", got)
	}
	if got := dispositions["biometric-live-staged"]; got.Canonical || !got.LiveStaging || !got.Staged {
		t.Fatalf("live-staged disposition=%+v, want live staging", got)
	}
	if got := dispositions["biometric-terminal-staged"]; got.Canonical || got.LiveStaging || !got.Staged {
		t.Fatalf("terminal-staged disposition=%+v, want staged-only", got)
	}
	if got := dispositions["biometric-plain-orphan"]; got.Canonical || got.LiveStaging || got.Staged {
		t.Fatalf("orphan disposition=%+v, want purgable", got)
	}
}

// TestCreateSpeakerProfileWithTasksConsentCAS pins the persistence gate: a
// consent change between capture and write rejects the whole batch and leaves
// no rows.
func TestCreateSpeakerProfileWithTasksConsentCAS(t *testing.T) {
	repo, assetID := newInferenceRepository(t)
	ctx := context.Background()
	consent, err := repo.SetSpeakerConsent(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	profile := domain.SpeakerProfile{ID: "profile-cas", Generation: 1, State: "active", AssetID: assetID}
	sources := []domain.SpeakerProfileSource{{ProfileID: profile.ID, AssetID: assetID, DiarizationGenerationID: "gen", LocalSpeakerID: "spk-1"}}
	tasks := []domain.InferenceTask{{
		AssetID: assetID, TaskKey: "speaker-cas-key", Operation: domain.OpSpeakerEmbedding,
		RequestJSON: mustJSON(t, inferenceTestRequest(domain.OpSpeakerEmbedding, assetID, "space", "cas")),
		ConfigHash:  "config-speaker_embedding", ModelRevision: "revision-1",
	}}
	if _, err := repo.CreateSpeakerProfileWithTasks(ctx, profile, sources, tasks, 40, consent.Generation); err != nil {
		t.Fatal(err)
	}
	if got := inferenceTestCount(t, repo, "speaker_profiles", ""); got != 1 {
		t.Fatalf("profiles=%d, want 1", got)
	}
	if got := inferenceTestCount(t, repo, "inference_tasks", "task_key='speaker-cas-key'"); got != 1 {
		t.Fatalf("tasks=%d, want 1", got)
	}
	// Flip consent: the captured generation no longer matches.
	if _, err := repo.SetSpeakerConsent(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetSpeakerConsent(ctx, true); err != nil {
		t.Fatal(err)
	}
	stale := domain.SpeakerProfile{ID: "profile-stale", Generation: 1, State: "active", AssetID: assetID}
	staleSources := []domain.SpeakerProfileSource{{ProfileID: stale.ID, AssetID: assetID, DiarizationGenerationID: "gen", LocalSpeakerID: "spk-2"}}
	staleTasks := []domain.InferenceTask{{
		AssetID: assetID, TaskKey: "speaker-cas-key-stale", Operation: domain.OpSpeakerEmbedding,
		RequestJSON: mustJSON(t, inferenceTestRequest(domain.OpSpeakerEmbedding, assetID, "space", "cas-stale")),
		ConfigHash:  "config-speaker_embedding", ModelRevision: "revision-1",
	}}
	_, err = repo.CreateSpeakerProfileWithTasks(ctx, stale, staleSources, staleTasks, 40, consent.Generation)
	if !errors.Is(err, domain.ErrSpeakerConsentGenerationInactive) {
		t.Fatalf("stale consent creation error=%v, want ErrSpeakerConsentGenerationInactive", err)
	}
	if got := inferenceTestCount(t, repo, "speaker_profiles", "id='profile-stale'"); got != 0 {
		t.Fatalf("stale CAS persisted %d profiles, want none", got)
	}
	if got := inferenceTestCount(t, repo, "inference_tasks", "task_key='speaker-cas-key-stale'"); got != 0 {
		t.Fatalf("stale CAS persisted %d tasks, want none", got)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestMigration0043SpeakerResultDigestsUpgrade runs the digest migration over
// a database that stopped at 0042 with live pre-0043 rows: the new nullable
// columns must appear, existing rows keep NULL (legacy identity is unknown,
// never guessed), and the staged-row reader treats them as unrecorded.
func TestMigration0043SpeakerResultDigestsUpgrade(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "upgrade13.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	applyMigrationsThrough(t, ctx, repo, "0042_local_model_target.sql")
	now := formatTime(time.Now().UTC())
	for _, statement := range []string{
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-upgrade','fp',100,'discovered','` + now + `','` + now + `')`,
		`INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at,lease_owner,lease_expires_at) VALUES('job-upgrade','asset-upgrade','inference','running',40,1,3,'` + now + `','speaker-upgrade-input','` + now + `','` + now + `','owner','` + time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano) + `')`,
		`INSERT INTO inference_tasks(job_id,task_key,operation,request_json,config_hash,model_revision,created_at,updated_at) VALUES('job-upgrade','speaker-upgrade-key','speaker_embedding','{"schema_version":"inference.v1","operation_id":"op-upgrade","operation":"speaker_embedding","model_id":"m","model_revision":"r","input_hash":"i","config_hash":"c","timeout_ms":1000,"input":{"kind":"asset","asset_id":"asset-upgrade","duration_ms":10000},"speaker":{"interval":{"start_ms":0,"end_ms":1000}}}','c','r','` + now + `','` + now + `')`,
		`INSERT INTO speaker_embedding_staging(operation_id,job_id,profile_id,profile_generation,consent_generation,secret_ref,created_at) VALUES('op-upgrade','job-upgrade','profile-upgrade',1,1,'biometric-upgrade-ref','` + now + `')`,
	} {
		if _, err := repo.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("apply remaining migrations over pre-0043 rows: %v", err)
	}
	staging, err := repo.GetSpeakerEmbeddingStaging(ctx, "op-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if staging.ResultDigest != "" {
		t.Fatalf("legacy staged digest=%q, want unrecorded", staging.ResultDigest)
	}
	state, err := repo.SpeakerResultCommitState(ctx, "job-upgrade", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if state.CommittedDigest != "" {
		t.Fatalf("legacy committed digest=%q, want unrecorded", state.CommittedDigest)
	}
	if !state.LeaseValid || state.CancelRequested {
		t.Fatalf("upgraded lease state=%+v, want live unexpired lease", state)
	}
}
