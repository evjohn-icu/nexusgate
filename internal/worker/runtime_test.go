package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/remote"
)

// A container that gains /dev/dri after a template edit must pick that up on
// its next restart without losing what an operator declared by hand at
// enroll time and that no probe can infer: which library roots are mounted,
// which Provider operations this node is trusted for, its speed class and
// its parallelism limits. Only the fields DetectHardware can actually
// measure should move.
func TestMergeDetectedCapabilitiesPreservesOperatorDeclaredFields(t *testing.T) {
	declared := remote.WorkerCapabilities{
		Proxy: false, Thumbnail: false, AudioExtract: false,
		Hardware:             []string{"stale-from-enroll"},
		LibraryRoots:         []string{"root-a", "root-b"},
		ProviderOperations:   []string{"video_analysis"},
		SpeedClass:           "slow",
		MaxParallelProxyJobs: 2,
		MaxProxyHeight:       1080,
	}
	report := media.HardwareReport{FFmpegFound: true, SelectedMode: "vaapi"}

	merged := MergeDetectedCapabilities(declared, report)

	if !merged.Proxy || !merged.Thumbnail || !merged.AudioExtract {
		t.Fatalf("a fresh detection that found ffmpeg must replace the stale enroll-time booleans, got %+v", merged)
	}
	if !reflect.DeepEqual(merged.Hardware, []string{"vaapi"}) {
		t.Fatalf("Hardware must be replaced by the fresh detection, got %v", merged.Hardware)
	}
	if !reflect.DeepEqual(merged.LibraryRoots, declared.LibraryRoots) {
		t.Fatalf("LibraryRoots is operator-declared and undetectable; it must survive the merge unchanged, got %v", merged.LibraryRoots)
	}
	if !reflect.DeepEqual(merged.ProviderOperations, declared.ProviderOperations) {
		t.Fatalf("ProviderOperations is operator-declared; it must survive the merge unchanged, got %v", merged.ProviderOperations)
	}
	if merged.SpeedClass != declared.SpeedClass {
		t.Fatalf("SpeedClass is operator-declared; it must survive the merge unchanged, got %q", merged.SpeedClass)
	}
	if merged.MaxParallelProxyJobs != declared.MaxParallelProxyJobs || merged.MaxProxyHeight != declared.MaxProxyHeight {
		t.Fatalf("parallelism limits are operator-declared; they must survive the merge unchanged, got %+v", merged)
	}
}

// validateArtifact must reject a zero-byte file the same way the Hub does —
// before the upload wastes a network round-trip. A derive that leaves an
// empty artifact (FFmpeg crash, disk full, etc.) should never reach
// UploadArtifact.
func TestValidateArtifactRejectsZeroByteFile(t *testing.T) {
	tmp := t.TempDir()
	emptyPath := filepath.Join(tmp, "empty.jpg")
	if err := os.WriteFile(emptyPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	if err := validateArtifact(ArtifactUpload{Type: "thumbnail", ProfileHash: "thumb-v1", Path: emptyPath}); err == nil {
		t.Fatalf("validateArtifact must reject a zero-byte artifact")
	}

	okPath := filepath.Join(tmp, "ok.jpg")
	if err := os.WriteFile(okPath, []byte("not empty"), 0o600); err != nil {
		t.Fatalf("write non-empty file: %v", err)
	}
	if err := validateArtifact(ArtifactUpload{Type: "thumbnail", ProfileHash: "thumb-v1", Path: okPath}); err != nil {
		t.Fatalf("validateArtifact must accept a non-empty regular file: %v", err)
	}
}

// A Worker that fell back to software must not keep whatever accelerator it
// happened to enroll with — the Hub would otherwise keep routing GPU work to
// a node that cannot do it.
func TestMergeDetectedCapabilitiesReportsNoHardwareForSoftwareFallback(t *testing.T) {
	declared := remote.WorkerCapabilities{Hardware: []string{"cuda"}}
	report := media.HardwareReport{FFmpegFound: true, SelectedMode: "software"}

	merged := MergeDetectedCapabilities(declared, report)

	if len(merged.Hardware) != 0 {
		t.Fatalf("a software fallback must not claim a hardware backend, got %v", merged.Hardware)
	}
}
