package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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

	payloadPath := WaveformPayloadPath(dst+".json", manifest)
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(payload)) != manifest.PayloadSize {
		t.Fatalf("payload is %d bytes, manifest says %d", len(payload), manifest.PayloadSize)
	}
	// The payload is content-addressed: its name carries the digest the
	// manifest records, so a published binary is recognizable on sight.
	if expected := "waveform." + manifest.PayloadSHA + ".bin"; filepath.Base(payloadPath) != expected {
		t.Fatalf("payload name %q is not the content-addressed %q", filepath.Base(payloadPath), expected)
	}
	// The manifest names its payload explicitly, so the pair needs no
	// convention to find its other half.
	if manifest.PayloadFile != filepath.Base(payloadPath) {
		t.Fatalf("manifest payload_file %q does not name the published payload", manifest.PayloadFile)
	}
	// And the published pair verifies as one artifact.
	if _, err := VerifyWaveformPair(dst + ".json"); err != nil {
		t.Fatalf("a freshly published pair must verify: %v", err)
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

// TestLegacyManifestLayoutStillReadsAndServes pins backward compatibility: a
// manifest written before payload_file existed carries no field, keeps its
// fixed sibling .bin, and every read path — payload derivation, viewport
// reads, pair verification — treats it as the pair's other half.
func TestLegacyManifestLayoutStillReadsAndServes(t *testing.T) {
	dir := t.TempDir()
	manifest := WaveformManifest{
		Version: WaveformManifestVersion, SampleRate: 16000, BucketMS: 10,
		DurationMS: 20, PayloadSize: 16,
		PayloadSHA: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		Levels:     []WaveformLevel{{Level: 0, Offset: 0, Count: 2}},
	}
	payload := make([]byte, 16)
	for i := range payload {
		payload[i] = byte(i)
	}
	sum := sha256.Sum256(payload)
	manifest.PayloadSHA = hex.EncodeToString(sum[:])
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "waveform.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "waveform.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	decoded, err := LoadWaveformManifest(filepath.Join(dir, "waveform.json"))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.PayloadFile != "" {
		t.Fatalf("a legacy manifest carries no payload_file, got %q", decoded.PayloadFile)
	}
	if got := WaveformPayloadPath(filepath.Join(dir, "waveform.json"), decoded); got != filepath.Join(dir, "waveform.bin") {
		t.Fatalf("legacy payload path = %q, want the fixed sibling", got)
	}
	if _, err := VerifyWaveformPair(filepath.Join(dir, "waveform.json")); err != nil {
		t.Fatalf("legacy pair must verify: %v", err)
	}
	buckets, err := ReadWaveformBuckets(decoded, payload, 0, 0, 20, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("legacy viewport = %d buckets, want 2", len(buckets))
	}
}

// TestGenerateWaveformPyramidSwapsPairAndRemovesPriorPayload pins the publish
// order: the binary is published under a content-addressed name, the manifest
// switches atomically, and only then is the prior managed payload removed —
// whether that prior was itself content-addressed or a legacy fixed-sibling
// .bin. A failed generation (undecodable source) leaves the prior pair
// untouched.
func TestGenerateWaveformPyramidSwapsPairAndRemovesPriorPayload(t *testing.T) {
	requireFFmpegBinary(t)
	dir := t.TempDir()
	src := generateTestAudio(t, dir,
		"-f", "lavfi", "-i", "sine=frequency=220:sample_rate=16000:duration=0.2")
	dst := filepath.Join(dir, "nested", "waveform")
	first, err := GenerateWaveformPyramid(context.Background(), src, dst, DefaultWaveformConfig())
	if err != nil {
		t.Fatal(err)
	}
	firstPayload := WaveformPayloadPath(dst+".json", first)

	// A second generation of DIFFERENT audio at the same destination must
	// switch the manifest to a new content-addressed payload and remove the
	// one the prior manifest managed.
	src2 := generateTestAudio(t, dir,
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=16000:duration=0.4")
	second, err := GenerateWaveformPyramid(context.Background(), src2, dst, DefaultWaveformConfig())
	if err != nil {
		t.Fatal(err)
	}
	secondPayload := WaveformPayloadPath(dst+".json", second)
	if firstPayload == secondPayload {
		t.Fatal("different audio must publish a different content-addressed payload")
	}
	if _, err := os.Stat(secondPayload); err != nil {
		t.Fatalf("the new payload must exist: %v", err)
	}
	if _, err := os.Stat(firstPayload); !os.IsNotExist(err) {
		t.Fatalf("the prior managed payload must be removed after the manifest switches, stat err = %v", err)
	}
	if _, err := VerifyWaveformPair(dst + ".json"); err != nil {
		t.Fatalf("the switched pair must verify: %v", err)
	}

	// A legacy pair at the same destination is replaced the same way: the
	// fixed-sibling .bin the legacy manifest managed is removed after the
	// manifest switches, and nothing else of the old layout survives.
	legacy := WaveformManifest{
		Version: WaveformManifestVersion, SampleRate: 16000, BucketMS: 10,
		DurationMS: 10, PayloadSize: 8, PayloadSHA: strings.Repeat("0", 64),
		Levels: []WaveformLevel{{Level: 0, Offset: 0, Count: 1}},
	}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".json", legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".bin", []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := GenerateWaveformPyramid(context.Background(), src2, dst, DefaultWaveformConfig())
	if err != nil {
		t.Fatal(err)
	}
	if WaveformPayloadPath(dst+".json", third) == dst+".bin" {
		t.Fatal("a new generation must publish a content-addressed payload, not reuse the legacy sibling")
	}
	if _, err := os.Stat(dst + ".bin"); !os.IsNotExist(err) {
		t.Fatalf("the legacy .bin the prior manifest managed must be removed, stat err = %v", err)
	}
	if _, err := VerifyWaveformPair(dst + ".json"); err != nil {
		t.Fatalf("the pair replacing a legacy pair must verify: %v", err)
	}

	// A failed generation — an undecodable source — must leave the prior,
	// verified pair exactly as it was.
	broken := filepath.Join(dir, "broken.wav")
	if err := os.WriteFile(broken, []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateWaveformPyramid(context.Background(), broken, dst, DefaultWaveformConfig()); err == nil {
		t.Fatal("an undecodable source must fail generation")
	}
	if _, err := VerifyWaveformPair(dst + ".json"); err != nil {
		t.Fatalf("a failed generation must leave the prior pair intact: %v", err)
	}
}

// TestVerifyWaveformPairReportsMissingPayload pins the missing-pair shape:
// either half missing or a payload that does not match the manifest's recorded
// size or digest fails verification, so derive regenerates instead of serving
// or repairing against a broken pair.
func TestVerifyWaveformPairReportsMissingPayload(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "waveform.json")
	manifest := WaveformManifest{
		Version: WaveformManifestVersion, SampleRate: 16000, BucketMS: 10,
		DurationMS: 10, PayloadSize: 8,
		Levels: []WaveformLevel{{Level: 0, Offset: 0, Count: 1}},
	}
	payload := []byte{0, 0, 0x80, 0xbf, 0, 0, 0x80, 0x3f}
	sum := sha256.Sum256(payload)
	manifest.PayloadSHA = hex.EncodeToString(sum[:])
	manifest.PayloadFile = "waveform." + manifest.PayloadSHA + ".bin"
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	payloadPath := WaveformPayloadPath(manifestPath, manifest)
	// Missing payload: the pair is incomplete.
	if _, err := VerifyWaveformPair(manifestPath); err == nil {
		t.Fatal("a pair with a missing payload must not verify")
	}
	// Short payload: a size mismatch.
	if err := os.WriteFile(payloadPath, payload[:4], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWaveformPair(manifestPath); err == nil {
		t.Fatal("a short payload must not verify")
	}
	// Corrupt payload: a digest mismatch.
	if err := os.WriteFile(payloadPath, []byte{1, 1, 1, 1, 1, 1, 1, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWaveformPair(manifestPath); err == nil {
		t.Fatal("a payload with the wrong bytes must not verify")
	}
	// Restored payload: the pair verifies again.
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWaveformPair(manifestPath); err != nil {
		t.Fatalf("a restored payload must verify: %v", err)
	}
	// A directory in the payload's place is not a payload.
	if err := os.Remove(payloadPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(payloadPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWaveformPayload(manifest, payloadPath); err == nil {
		t.Fatal("a directory at the payload path must not verify")
	}
}

// TestReadWaveformBucketsAtServesWindowFromOpenedFile pins the serving path:
// an opened payload handle plus its stat size is all a viewport needs — the
// window is read from the handle, bounds are checked against the stat before
// any read, and a payload shorter than the manifest is rejected before a byte
// is pulled.
func TestReadWaveformBucketsAtServesWindowFromOpenedFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "waveform.json")
	// Two levels: level 0 has 4 buckets, level 1 coarsens them to 2.
	level0 := []float32{-1, 1, -0.5, 0.5, -0.25, 0.25, 0, 0}
	level1 := []float32{-1, 1, -0.25, 0.25}
	var payload []byte
	for _, v := range append(level0, level1...) {
		bits := math.Float32bits(v)
		payload = binary.LittleEndian.AppendUint32(payload, bits)
	}
	sum := sha256.Sum256(payload)
	manifest := WaveformManifest{
		Version: WaveformManifestVersion, SampleRate: 16000, BucketMS: 10,
		DurationMS: 40, PayloadSize: int64(len(payload)),
		PayloadSHA:  hex.EncodeToString(sum[:]),
		PayloadFile: "waveform." + hex.EncodeToString(sum[:]) + ".bin",
		Levels: []WaveformLevel{
			{Level: 0, Offset: 0, Count: 4},
			{Level: 1, Offset: 4, Count: 2},
		},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	payloadPath := WaveformPayloadPath(manifestPath, manifest)
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	// A level-1 viewport is served from the opened handle with the exact
	// clamped geometry the slice reader produces.
	fromHandle, err := ReadWaveformBucketsAt(manifest, f, info.Size(), 1, 0, 40, 4096)
	if err != nil {
		t.Fatal(err)
	}
	fromSlice, err := ReadWaveformBuckets(manifest, payload, 1, 0, 40, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromHandle, fromSlice) || len(fromHandle) != 2 {
		t.Fatalf("handle and slice readers disagree: %+v vs %+v", fromHandle, fromSlice)
	}
	// The maxBuckets cap applies on the ReaderAt path too.
	capped, err := ReadWaveformBucketsAt(manifest, f, info.Size(), 0, 0, 40, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 3 {
		t.Fatalf("cap = %d buckets, want 3", len(capped))
	}
	// A stat shorter than the manifest is rejected before reading.
	if _, err := ReadWaveformBucketsAt(manifest, f, info.Size()-1, 0, 0, 40, 4096); err == nil {
		t.Fatal("a payload stat shorter than the manifest must be rejected")
	}
	// A manifest claiming more pairs than the opened payload stat holds is
	// rejected at the window: the stat equals the manifest's own declared
	// size, so only the per-window bound can catch it, not the size check.
	overhanging := manifest
	overhanging.PayloadSize = 40
	overhanging.Levels = []WaveformLevel{{Level: 0, Offset: 0, Count: 6}}
	if _, err := ReadWaveformBucketsAt(overhanging, f, 40, 0, 0, 70, 4096); err == nil {
		t.Fatal("a viewport beyond the payload stat must be rejected")
	}
	// A stat longer than the manifest (a trailing-grow corruption) still
	// serves the manifest's declared window.
	grown, err := ReadWaveformBucketsAt(manifest, f, info.Size()+128, 1, 0, 40, 4096)
	if err != nil || len(grown) != 2 {
		t.Fatalf("an oversized stat must serve the declared window, got %d buckets, err %v", len(grown), err)
	}
}

// TestReadWaveformManifestBoundedShape pins the bounded, shape-validating
// decoder behind the API's rooted handle: oversize input, wrong version, and
// a manifest naming an escaping payload file are all rejected before any
// payload byte is read.
func TestReadWaveformManifestBoundedShape(t *testing.T) {
	valid := WaveformManifest{
		Version: WaveformManifestVersion, SampleRate: 16000, BucketMS: 10,
		DurationMS: 10, PayloadSize: 8, PayloadSHA: strings.Repeat("ab", 32),
		Levels: []WaveformLevel{{Level: 0, Offset: 0, Count: 1}},
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWaveformManifest(bytes.NewReader(encoded)); err != nil {
		t.Fatalf("a valid manifest must decode: %v", err)
	}
	wrongVersion := valid
	wrongVersion.Version = "waveform.v999"
	broken, _ := json.Marshal(wrongVersion)
	if _, err := ReadWaveformManifest(bytes.NewReader(broken)); err == nil {
		t.Fatal("an unsupported version must be rejected")
	}
	escaping := valid
	escaping.PayloadFile = "../escape.bin"
	broken, _ = json.Marshal(escaping)
	if _, err := ReadWaveformManifest(bytes.NewReader(broken)); err == nil {
		t.Fatal("a payload_file escaping the manifest directory must be rejected")
	}
	if _, err := ReadWaveformManifest(bytes.NewReader(make([]byte, manifestMaxBytes+2))); err == nil {
		t.Fatal("an oversized manifest must be rejected")
	}
	short := valid
	short.PayloadSize = 4
	broken, _ = json.Marshal(short)
	if _, err := ReadWaveformManifest(bytes.NewReader(broken)); err == nil {
		t.Fatal("a manifest with an empty payload must be rejected")
	}
	// Levels must fit inside the recorded payload.
	overhanging := valid
	overhanging.Levels = []WaveformLevel{{Level: 0, Offset: 0, Count: 4}}
	broken, _ = json.Marshal(overhanging)
	if _, err := ReadWaveformManifest(bytes.NewReader(broken)); err == nil {
		t.Fatal("levels overhanging the payload must be rejected")
	}
}
