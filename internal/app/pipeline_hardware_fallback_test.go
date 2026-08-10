package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/staging"
)

func TestPipelineDeriveHardwareFallbackPersistsActualProfile(t *testing.T) {
	testPipelineDerive(t, true, false)
}

func TestPipelineDeriveHardwareSuccessPersistsHardwareProfile(t *testing.T) {
	testPipelineDerive(t, false, false)
}

func TestPipelineDeriveReusesSoftwareCacheWithoutRendering(t *testing.T) {
	testPipelineDerive(t, false, true)
}

func testPipelineDerive(t *testing.T, failHardware, reuseSoftware bool) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(dir, "footage")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootDir, "clip.mp4")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, rootDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := repo.UpsertScannedFile(ctx, root, "clip.mp4", source, info, "fallback-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMediaMetadata(ctx, asset.AssetID, domain.MediaMetadata{DurationMS: 1000, SourceColor: string(media.SourceColorSDR)}, "test"); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	assetDir := filepath.Join(cacheDir, asset.AssetID)
	if reuseSoftware {
		if err := os.MkdirAll(assetDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"thumbnail-software.jpg", "proxy-software.mp4"} {
			if err := os.WriteFile(filepath.Join(assetDir, name), []byte("cached"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	ffmpegDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(ffmpegDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeMediaCommands(t, ffmpegDir, failHardware)
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", ffmpegDir+string(os.PathListSeparator)+oldPath)
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	hardware := media.HardwarePlan{Mode: "cuda", DecoderArgs: []string{"-hwaccel", "cuda"}, EncoderArgs: []string{"-c:v", "h264_nvenc"}, AllowFallback: true}
	stager, err := staging.New("copy", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := NewPipeline(repo, cacheDir, nil, nil, nil, nil, nil, hardware, stager, providerRouteDeferral, 0)
	if err := repo.EnqueueJob(ctx, asset.AssetID, domain.JobDerive, "derive-fallback", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	thumb, err := repo.GetArtifact(ctx, asset.AssetID, "thumbnail")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := repo.GetArtifact(ctx, asset.AssetID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	wantProfile := "hw-cuda-h264-v1"
	wantMode := "cuda"
	if failHardware || reuseSoftware {
		wantProfile, wantMode = "software-h264-x264-v1", "software"
	}
	if thumb == nil || thumb.ProfileHash != "thumb-"+wantProfile || !strings.HasSuffix(thumb.LocalPath, "thumbnail-"+wantMode+".jpg") {
		t.Fatalf("thumbnail=%+v", thumb)
	}
	if proxy == nil || proxy.ProfileHash != "proxy-720-"+wantProfile || !strings.HasSuffix(proxy.LocalPath, "proxy-"+wantMode+".mp4") {
		t.Fatalf("proxy=%+v", proxy)
	}
	if failHardware {
		if _, err := os.Stat(filepath.Join(assetDir, "thumbnail-cuda.jpg")); !os.IsNotExist(err) {
			t.Fatalf("hardware thumbnail exists: %v", err)
		}
		if _, err := os.Stat(filepath.Join(assetDir, "proxy-cuda.mp4")); !os.IsNotExist(err) {
			t.Fatalf("hardware proxy exists: %v", err)
		}
	}
	for _, pattern := range []string{".derive-thumbnail.tmp.jpg", ".derive-proxy.tmp.mp4"} {
		if _, err := os.Stat(filepath.Join(assetDir, pattern)); !os.IsNotExist(err) {
			t.Fatalf("staging remnant %s: %v", pattern, err)
		}
	}
}

func writeFakeMediaCommands(t *testing.T, dir string, failHardware bool) {
	t.Helper()
	ffprobe := `#!/bin/sh
printf '%s' '{"format":{"duration":"1.0","filename":"clip.mp4"},"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,"avg_frame_rate":"30/1","pix_fmt":"yuv420p"}]}'
`
	ffmpeg := `#!/bin/sh
out=""
for arg in "$@"; do out="$arg"; done
` + func() string {
		if failHardware {
			return `case " $* " in *" h264_nvenc "*|*" -hwaccel "*) exit 1;; esac
`
		}
		return ""
	}() + `printf 'derived' > "$out"
`
	for name, body := range map[string]string{"ffprobe": ffprobe, "ffmpeg": ffmpeg} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}
