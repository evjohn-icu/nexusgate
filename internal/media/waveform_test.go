package media

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireFFmpegBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
}

func generateTestAudio(t *testing.T, dir string, args ...string) string {
	t.Helper()
	path := filepath.Join(dir, "audio.wav")
	base := []string{"-y", "-nostdin", "-hide_banner", "-loglevel", "error"}
	cmd := exec.Command("ffmpeg", append(append(base, args...), path)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate audio: %v: %s", err, out)
	}
	return path
}

func TestGenerateWaveformPyramidProducesRealExtrema(t *testing.T) {
	requireFFmpegBinary(t)
	dir := t.TempDir()
	// Half a second of silence followed by half a second of a full-scale sine:
	// the waveform must show both, and silence must be zero rather than missing.
	src := generateTestAudio(t, dir,
		"-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono:d=0.5",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=0.5",
		"-filter_complex", "[0:a][1:a]concat=n=2:v=0:a=1")

	dst := filepath.Join(dir, "waveform")
	manifest, err := GenerateWaveformPyramid(context.Background(), src, dst, DefaultWaveformConfig())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if manifest.Version != WaveformManifestVersion {
		t.Fatalf("unexpected version %q", manifest.Version)
	}
	if manifest.DurationMS < 950 || manifest.DurationMS > 1050 {
		t.Fatalf("duration %d ms is not the decoded one second", manifest.DurationMS)
	}
	if len(manifest.Levels) < 5 {
		t.Fatalf("pyramid has %d levels; a 1s clip at 10ms buckets needs about 7", len(manifest.Levels))
	}
	last := manifest.Levels[len(manifest.Levels)-1]
	if last.Count != 1 {
		t.Fatalf("top level must collapse to one bucket, got %d", last.Count)
	}
	for i := 1; i < len(manifest.Levels); i++ {
		prev, cur := manifest.Levels[i-1], manifest.Levels[i]
		if cur.Count != (prev.Count+1)/2 {
			t.Fatalf("level %d has %d buckets, want ceil(%d/2)", i, cur.Count, prev.Count)
		}
	}

	payload, err := os.ReadFile(dst + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(payload)) != manifest.PayloadSize {
		t.Fatalf("payload is %d bytes, manifest says %d", len(payload), manifest.PayloadSize)
	}
	raw, err := os.ReadFile(dst + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var decoded WaveformManifest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if decoded.PayloadSHA != manifest.PayloadSHA {
		t.Fatal("manifest payload digest changed on disk")
	}

	silent, err := ReadWaveformBuckets(manifest, payload, 0, 0, 400, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(silent) == 0 {
		t.Fatal("silent viewport returned no buckets")
	}
	for _, bucket := range silent {
		if bucket.Min != 0 || bucket.Max != 0 {
			t.Fatalf("silence bucket at %d ms is not zero: %+v", bucket.StartMS, bucket)
		}
	}
	loud, err := ReadWaveformBuckets(manifest, payload, 0, 600, 900, 64)
	if err != nil {
		t.Fatal(err)
	}
	peaked := false
	for _, bucket := range loud {
		if bucket.Max > 0.05 || bucket.Min < -0.05 {
			peaked = true
		}
	}
	if !peaked {
		t.Fatalf("sine section shows no amplitude: %+v", loud)
	}

	if _, err := ReadWaveformBuckets(manifest, payload, 0, 900, 100, 10); err == nil {
		t.Fatal("a reversed viewport must be rejected")
	}
	if _, err := ReadWaveformBuckets(manifest, payload, 99, 0, 100, 10); err == nil {
		t.Fatal("an out-of-range level must be rejected")
	}
	clamped, err := ReadWaveformBuckets(manifest, payload, 0, 0, 100000, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(clamped) != 7 {
		t.Fatalf("viewport cap not applied: got %d buckets", len(clamped))
	}
}

func TestReadWaveformBucketsRejectsTruncatedPayload(t *testing.T) {
	manifest := WaveformManifest{
		Version: WaveformManifestVersion, SampleRate: 16000, BucketMS: 10,
		Levels: []WaveformLevel{{Level: 0, Offset: 0, Count: 4}},
	}
	if _, err := ReadWaveformBuckets(manifest, make([]byte, 16), 0, 0, 40, 10); err == nil {
		t.Fatal("a payload shorter than the manifest must be rejected")
	}
}
