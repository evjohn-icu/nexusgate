package remote

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// The grants body the administrator sends on pairing creation must keep the
// documented field names, and an empty grant set must serialize to an empty
// object so legacy installers read it as derive-only.
func TestWorkerGrantsJSONShape(t *testing.T) {
	grants := WorkerGrants{
		InferenceOperations:       []string{"sound_events", "visual_embedding"},
		ModelManagementOperations: []string{"install", "probe"},
	}
	encoded, err := json.Marshal(grants)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, field := range []string{`"inference_operations"`, `"model_management_operations"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("grants JSON %s is missing %s", encoded, field)
		}
	}
	var decoded WorkerGrants
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if strings.Join(decoded.InferenceOperations, ",") != "sound_events,visual_embedding" {
		t.Fatalf("decoded inference operations = %v", decoded.InferenceOperations)
	}
	if strings.Join(decoded.ModelManagementOperations, ",") != "install,probe" {
		t.Fatalf("decoded model management operations = %v", decoded.ModelManagementOperations)
	}

	empty, err := json.Marshal(WorkerGrants{})
	if err != nil {
		t.Fatalf("Marshal empty: %v", err)
	}
	if string(empty) != "{}" {
		t.Fatalf("an empty grant set must stay derive-only on the wire, got %s", empty)
	}
}

// Maintenance leases carry the typed task payload under model_management; a
// derive lease omits the field entirely and a Worker that predates the field
// decodes such a lease to a nil task.
func TestWorkerJobModelManagementWireShape(t *testing.T) {
	job := WorkerJob{
		JobID:   "job-1",
		JobType: domain.JobModelInstall,
		ModelManagement: &ModelManagementTask{
			JobID:           "job-1",
			TargetWorkerID:  "local",
			CatalogID:       "google/yamnet",
			ManifestDigest:  "sha256:abc",
			Action:          "install",
			LicenseAccepted: true,
		},
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"model_management"`) {
		t.Fatalf("maintenance lease JSON %s must carry model_management", encoded)
	}
	var decoded WorkerJob
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ModelManagement == nil || decoded.ModelManagement.CatalogID != "google/yamnet" || decoded.ModelManagement.Action != "install" {
		t.Fatalf("round-trip lost the maintenance task: %+v", decoded.ModelManagement)
	}
	if decoded.ModelManagement.JobID != job.JobID {
		t.Fatalf("task job id %q must match the lease %q", decoded.ModelManagement.JobID, job.JobID)
	}

	derive, err := json.Marshal(WorkerJob{JobID: "job-2", JobType: domain.JobDerive})
	if err != nil {
		t.Fatalf("Marshal derive: %v", err)
	}
	if strings.Contains(string(derive), "model_management") {
		t.Fatalf("a derive lease must not carry model_management, got %s", derive)
	}
	var oldWorkerJob WorkerJob
	if err := json.Unmarshal([]byte(`{"job_id":"job-3","job_type":"model_probe"}`), &oldWorkerJob); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if oldWorkerJob.ModelManagement != nil {
		t.Fatalf("a Worker predating the field must decode a nil task, got %+v", oldWorkerJob.ModelManagement)
	}
}
