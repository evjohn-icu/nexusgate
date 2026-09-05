package remote

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func jsonRoundTrip[T any](t *testing.T, name string, in T, wantFields []string) T {
	t.Helper()
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("%s: Marshal: %v", name, err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("%s: Unmarshal: %v", name, err)
	}
	reEncoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("%s: re-Marshal: %v", name, err)
	}
	if string(encoded) != string(reEncoded) {
		t.Fatalf("%s: round-trip mismatch\n  original: %s\n  re-encoded: %s", name, encoded, reEncoded)
	}
	// Verify every wanted field appears in the JSON text.
	raw := string(encoded)
	for _, f := range wantFields {
		if !strings.Contains(raw, `"`+f+`"`) && !strings.Contains(raw, `"`+f+`":`) {
			t.Errorf("%s: missing field %q in JSON: %s", name, f, raw)
		}
	}
	return out
}

func TestWorkerJobRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := WorkerJob{
		JobID:             "job-abc-123",
		AssetID:           "asset-xyz-456",
		RootID:            "root-1",
		RelativePath:      "footage/clip.mp4",
		ModifiedNS:        now.UnixNano(),
		JobType:           domain.JobDerive,
		AttemptCount:      2,
		MaxAttempts:       5,
		CurrentStage:      "proxy",
		Progress:          0.75,
		PreferredWorkerID: "worker-west",
		AssignedWorkerID:  "worker-east",
		SourceBytes:       1048576,
		ReadRate:          2.5,
	}
	out := jsonRoundTrip(t, "WorkerJob", in, []string{
		"job_id", "asset_id", "root_id", "relative_path", "modified_ns",
		"job_type", "attempt_count", "max_attempts", "current_stage",
		"progress", "preferred_worker_id", "assigned_worker_id",
		"source_bytes", "read_rate",
	})
	if out.JobID != in.JobID || out.AssetID != in.AssetID || out.RootID != in.RootID {
		t.Fatalf("WorkerJob field mismatch: %+v vs %+v", in, out)
	}
	if out.JobType != domain.JobDerive {
		t.Fatalf("WorkerJob JobType = %q, want %q", out.JobType, domain.JobDerive)
	}
}

func TestWorkerJobMinimal(t *testing.T) {
	// Only required fields populated; optional fields stay zero-valued.
	in := WorkerJob{
		JobID:        "minimal-job",
		AssetID:      "minimal-asset",
		RootID:       "r",
		RelativePath: "a.mp4",
		JobType:      domain.JobProbe,
	}
	out := jsonRoundTrip(t, "WorkerJob-minimal", in, nil)
	if out.JobID != "minimal-job" {
		t.Fatalf("JobID = %q", out.JobID)
	}
	// Optional fields must remain zero.
	if out.CurrentStage != "" {
		t.Errorf("CurrentStage = %q, want empty", out.CurrentStage)
	}
	if out.PreferredWorkerID != "" {
		t.Errorf("PreferredWorkerID = %q, want empty", out.PreferredWorkerID)
	}
	if out.SourceBytes != 0 {
		t.Errorf("SourceBytes = %d, want 0", out.SourceBytes)
	}
	if out.ReadRate != 0 {
		t.Errorf("ReadRate = %f, want 0", out.ReadRate)
	}
}

func TestWorkerJobEventRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := WorkerJobEvent{
		ID:        "evt-1",
		JobID:     "job-1",
		WorkerID:  "worker-1",
		EventType: "stage_started",
		Stage:     "proxy",
		Progress:  0.5,
		ErrorCode: "timeout",
		Retryable: true,
		Message:   "connection reset",
		CreatedAt: now,
	}
	out := jsonRoundTrip(t, "WorkerJobEvent", in, []string{
		"id", "job_id", "worker_id", "event_type", "stage",
		"progress", "error_code", "retryable", "message", "created_at",
	})
	if out.EventType != "stage_started" || out.Message != "connection reset" {
		t.Fatalf("field mismatch: %+v", out)
	}
	if !out.Retryable {
		t.Fatal("Retryable = false, want true")
	}
	if !out.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", out.CreatedAt, now)
	}
}

func TestWorkerJobEventMinimal(t *testing.T) {
	in := WorkerJobEvent{
		ID:        "evt-min",
		JobID:     "j-min",
		EventType: "job_completed",
		CreatedAt: time.Now().Truncate(time.Second),
	}
	out := jsonRoundTrip(t, "WorkerJobEvent-minimal", in, nil)
	if out.WorkerID != "" {
		t.Errorf("WorkerID = %q, want empty", out.WorkerID)
	}
	if out.ErrorCode != "" {
		t.Errorf("ErrorCode = %q, want empty", out.ErrorCode)
	}
}

func TestWorkerJobStatusRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := WorkerJobStatus{
		JobID:               "job-status-1",
		AssetID:             "asset-1",
		JobType:             domain.JobTranscribe,
		State:               domain.JobRunning,
		Priority:            10,
		AttemptCount:        1,
		MaxAttempts:         3,
		RunAfter:            now.Add(-time.Hour),
		LeaseOwner:          "worker-west",
		LeaseExpiresAt:      now.Add(time.Hour),
		CurrentStage:        "transcribe",
		Progress:            0.3,
		LastErrorCode:       "E_PROVIDER",
		LastErrorMessage:    "upstream returned 503",
		LastFailureAt:       now.Add(-2 * time.Hour),
		LastFailureWorkerID: "worker-old",
		LastFailureStage:    "transcribe",
		PreferredWorkerID:   "worker-west",
		AssignedWorkerID:    "worker-west",
		Events: []WorkerJobEvent{
			{ID: "e1", JobID: "job-status-1", EventType: "job_started", CreatedAt: now.Add(-30 * time.Minute)},
			{ID: "e2", JobID: "job-status-1", EventType: "stage_started", Stage: "probe", CreatedAt: now.Add(-25 * time.Minute)},
		},
	}
	out := jsonRoundTrip(t, "WorkerJobStatus", in, []string{
		"job_id", "asset_id", "job_type", "state", "priority",
		"attempt_count", "max_attempts", "run_after", "lease_owner",
		"lease_expires_at", "current_stage", "progress", "last_error_code",
		"last_error_message", "last_failure_at", "last_failure_worker_id",
		"last_failure_stage", "preferred_worker_id", "assigned_worker_id",
		"events",
	})
	if len(out.Events) != 2 {
		t.Fatalf("Events count = %d, want 2", len(out.Events))
	}
	if out.Events[0].EventType != "job_started" {
		t.Fatalf("Events[0].EventType = %q", out.Events[0].EventType)
	}
	if out.State != domain.JobRunning {
		t.Fatalf("State = %q, want %q", out.State, domain.JobRunning)
	}
}

func TestWorkerCapabilitiesRoundTrip(t *testing.T) {
	in := WorkerCapabilities{
		Proxy:                true,
		Thumbnail:            true,
		AudioExtract:         false,
		MaxParallelProxyJobs: 4,
		MaxProxyHeight:       1080,
		SpeedClass:           "fast",
		Hardware:             []string{"nvenc", "qsv"},
		LibraryRoots:         []string{"root-a", "root-b"},
		ProviderOperations:   []string{"video_analysis", "asr"},
	}
	out := jsonRoundTrip(t, "WorkerCapabilities", in, []string{
		"proxy", "thumbnail", "audio_extract", "max_parallel_proxy_jobs",
		"max_proxy_height", "speed_class", "hardware", "library_roots",
		"provider_operations",
	})
	if !out.Proxy || !out.Thumbnail || out.AudioExtract {
		t.Fatalf("bool fields mismatch: %+v", out)
	}
	if len(out.Hardware) != 2 || out.Hardware[0] != "nvenc" {
		t.Fatalf("Hardware = %v", out.Hardware)
	}
	if len(out.LibraryRoots) != 2 || out.LibraryRoots[1] != "root-b" {
		t.Fatalf("LibraryRoots = %v", out.LibraryRoots)
	}
	if len(out.ProviderOperations) != 2 || out.ProviderOperations[0] != "video_analysis" {
		t.Fatalf("ProviderOperations = %v", out.ProviderOperations)
	}
}

func TestWorkerCapabilitiesEmptySlices(t *testing.T) {
	in := WorkerCapabilities{
		Proxy:          true,
		Thumbnail:      false,
		MaxProxyHeight: 720,
		SpeedClass:     "standard",
	}
	out := jsonRoundTrip(t, "WorkerCapabilities-empty-slices", in, nil)
	if out.Hardware != nil {
		t.Errorf("Hardware = %v, want nil", out.Hardware)
	}
	if out.LibraryRoots != nil {
		t.Errorf("LibraryRoots = %v, want nil", out.LibraryRoots)
	}
	if out.ProviderOperations != nil {
		t.Errorf("ProviderOperations = %v, want nil", out.ProviderOperations)
	}
}

func TestWorkerRegistrationRoundTrip(t *testing.T) {
	in := WorkerRegistration{
		Name:     "garage-worker",
		Platform: "linux-amd64",
		Version:  "v0.22.0",
		Capabilities: WorkerCapabilities{
			Proxy: true, Thumbnail: true, MaxProxyHeight: 720, SpeedClass: "standard",
			Hardware: []string{"vaapi"},
		},
	}
	out := jsonRoundTrip(t, "WorkerRegistration", in, []string{
		"name", "platform", "version", "capabilities",
	})
	if out.Name != "garage-worker" {
		t.Fatalf("Name = %q", out.Name)
	}
	if out.Version != "v0.22.0" {
		t.Fatalf("Version = %q", out.Version)
	}
	if !out.Capabilities.Proxy {
		t.Fatal("Capabilities.Proxy = false")
	}
}

func TestWorkerRegistrationWithoutVersion(t *testing.T) {
	in := WorkerRegistration{
		Name:     "simple-worker",
		Platform: "darwin-arm64",
		Capabilities: WorkerCapabilities{
			Thumbnail: true, SpeedClass: "slow",
		},
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"version"`) {
		// omitempty should suppress empty version
		t.Logf("version field was serialized (might be ok): %s", encoded)
	}
	var out WorkerRegistration
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if out.Version != "" {
		t.Errorf("Version after unmarshal = %q, want empty", out.Version)
	}
}

func TestWorkerRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := Worker{
		ID:       "w-abc-123",
		Name:     "basement-node",
		Platform: "linux-arm64",
		Version:  "v0.22.0",
		Status:   WorkerOnline,
		Capabilities: WorkerCapabilities{
			Proxy: true, Thumbnail: true, AudioExtract: true,
			MaxParallelProxyJobs: 2, MaxProxyHeight: 720, SpeedClass: "slow",
			LibraryRoots: []string{"root-main"},
		},
		LastSeenAt: now,
		CreatedAt:  now.Add(-24 * time.Hour),
	}
	out := jsonRoundTrip(t, "Worker", in, []string{
		"id", "name", "platform", "version", "status",
		"capabilities", "last_seen_at", "created_at",
	})
	if out.ID != "w-abc-123" || out.Status != WorkerOnline {
		t.Fatalf("field mismatch: %+v", out)
	}
	if !out.LastSeenAt.Equal(now) {
		t.Fatalf("LastSeenAt = %v, want %v", out.LastSeenAt, now)
	}
}

func TestPairingTokenRoundTrip(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	in := PairingToken{
		Token:     "pt-secret-token-value",
		ExpiresAt: expiry,
	}
	out := jsonRoundTrip(t, "PairingToken", in, []string{"token", "expires_at"})
	if out.Token != "pt-secret-token-value" {
		t.Fatalf("Token = %q", out.Token)
	}
	if !out.ExpiresAt.Equal(expiry) {
		t.Fatalf("ExpiresAt = %v, want %v", out.ExpiresAt, expiry)
	}
}

func TestWorkerAssignmentModeConstants(t *testing.T) {
	// Ensure the constants have the expected values — these are part of the wire contract.
	if WorkerAssignmentAny != "any" {
		t.Errorf("WorkerAssignmentAny = %q", WorkerAssignmentAny)
	}
	if WorkerAssignmentPreferred != "preferred" {
		t.Errorf("WorkerAssignmentPreferred = %q", WorkerAssignmentPreferred)
	}
	if WorkerAssignmentRequired != "required" {
		t.Errorf("WorkerAssignmentRequired = %q", WorkerAssignmentRequired)
	}
	if WorkerOnline != "online" || WorkerOffline != "offline" || WorkerRevoked != "revoked" {
		t.Errorf("Worker status constants: %q %q %q", WorkerOnline, WorkerOffline, WorkerRevoked)
	}
}
