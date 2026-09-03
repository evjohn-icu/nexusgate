package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/media"
	"github.com/evjohn-icu/nexusslate/internal/remote"
	"github.com/evjohn-icu/nexusslate/internal/testhelper"
)

type runtimeTestClient struct {
	job      *remote.WorkerJob
	uploads  []ArtifactUpload
	complete domain.JobState
}

func (c *runtimeTestClient) Heartbeat(context.Context, string, string, remote.WorkerCapabilities) error {
	return nil
}
func (c *runtimeTestClient) Lease(context.Context, string) (*remote.WorkerJob, error) {
	job := c.job
	c.job = nil
	return job, nil
}
func (c *runtimeTestClient) UploadArtifact(_ context.Context, _ string, _ string, artifact ArtifactUpload) error {
	c.uploads = append(c.uploads, artifact)
	return nil
}
func (c *runtimeTestClient) Complete(_ context.Context, _ string, _ string, state domain.JobState, _ string) error {
	c.complete = state
	return nil
}

func TestFFmpegDeriverFallbackUploadsSoftwareProfiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "footage")
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	testhelper.InstallCommand(t, bin, "ffprobe")
	testhelper.InstallCommand(t, bin, "ffmpeg")
	t.Setenv("NEXUSSLATE_FAKE_FFMPEG_FAIL_HARDWARE", "1")
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	client := &runtimeTestClient{job: &remote.WorkerJob{JobID: "job-1", AssetID: "asset-1", RootID: "root-1", RelativePath: "clip.mp4", ModifiedNS: 1, JobType: domain.JobDerive}}
	runtime := NewRuntime(client, Config{Token: "token", CacheDir: cache, Mounts: map[string]string{"root-1": root}}, NewFFmpegDeriver(media.HardwarePlan{Mode: "cuda", DecoderArgs: []string{"-hwaccel", "cuda"}, EncoderArgs: []string{"-c:v", "h264_nvenc"}, AllowFallback: true}), remote.WorkerCapabilities{})
	worked, err := runtime.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%t err=%v", worked, err)
	}
	if client.complete != domain.JobSucceeded {
		t.Fatalf("complete state=%q", client.complete)
	}
	if len(client.uploads) != 2 {
		t.Fatalf("uploads=%d, want 2: %+v", len(client.uploads), client.uploads)
	}
	for _, want := range []struct{ typ, profile, path string }{{"thumbnail", "thumb-software-h264-x264-v1", "thumbnail-software.jpg"}, {"proxy", "proxy-720-software-h264-x264-v1", "proxy-software.mp4"}} {
		var found bool
		for _, upload := range client.uploads {
			if upload.Type == want.typ {
				found = true
				if upload.ProfileHash != want.profile || !strings.HasSuffix(upload.Path, want.path) {
					t.Errorf("upload=%+v, want profile=%q path suffix=%q", upload, want.profile, want.path)
				}
			}
		}
		if !found {
			t.Errorf("missing %s upload", want.typ)
		}
	}
	assetDir := filepath.Join(cache, "artifacts", "asset-1")
	for _, name := range []string{"thumbnail-cuda.jpg", "proxy-cuda.mp4", ".derive-thumbnail.tmp.jpg", ".derive-proxy.tmp.mp4"} {
		if _, err := os.Stat(filepath.Join(assetDir, name)); !os.IsNotExist(err) {
			t.Errorf("unexpected file %s: %v", name, err)
		}
	}
}

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

func TestValidateArtifactRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target.jpg")
	if err := os.WriteFile(target, []byte("usable"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link.jpg")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifact(ArtifactUpload{Type: "thumbnail", ProfileHash: "thumb-v1", Path: link}); err == nil {
		t.Fatal("validateArtifact must reject a symlink")
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
