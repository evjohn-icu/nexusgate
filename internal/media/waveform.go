package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WaveformManifestVersion is the on-disk schema of a waveform pyramid.
const WaveformManifestVersion = "waveform.v1"

// WaveformConfig fixes the geometry of a waveform pyramid. It is part of the
// artifact's profile hash: two manifests with different settings describe
// different pixels, so they must never be served under one identity.
type WaveformConfig struct {
	// BucketMS is the width of a level-0 bucket. 10 ms matches the frame
	// granularity of the timeline tracks and is small enough that a zoomed-in
	// view still looks like a waveform rather than a bar chart.
	BucketMS int
	// SampleRate is the decode rate. 16 kHz is the rate the audio derivative
	// already uses; resampling costs time and buys no visible detail for a
	// min/max envelope.
	SampleRate int
}

// DefaultWaveformConfig is the shipped geometry.
func DefaultWaveformConfig() WaveformConfig {
	return WaveformConfig{BucketMS: 10, SampleRate: 16000}
}

// WaveformLevel describes one level's slice of the binary payload.
type WaveformLevel struct {
	Level  int `json:"level"`
	Offset int `json:"offset"`
	Count  int `json:"count"`
}

// WaveformManifest is the JSON half of the artifact. It is the commit marker
// of the pair: the payload half is published first, and the manifest is what
// makes a payload part of the served artifact.
type WaveformManifest struct {
	Version     string          `json:"version"`
	SampleRate  int             `json:"sample_rate"`
	BucketMS    int             `json:"bucket_ms"`
	DurationMS  int64           `json:"duration_ms"`
	Levels      []WaveformLevel `json:"levels"`
	PayloadSize int64           `json:"payload_size"`
	PayloadSHA  string          `json:"payload_sha256"`
	// PayloadFile names the sibling file that carries the binary payload,
	// relative to the manifest's directory. It is empty on legacy manifests,
	// which store the payload at the fixed sibling <manifest>.bin. New
	// generations name it after the payload's digest so a binary can be
	// published and swapped without ever shortening the served artifact.
	PayloadFile string `json:"payload_file,omitempty"`
}

// WaveformBucket is one served min/max pair.
type WaveformBucket struct {
	StartMS int64   `json:"start_ms"`
	Min     float32 `json:"min"`
	Max     float32 `json:"max"`
}

// Validate rejects a geometry that would divide by zero or produce an unusable
// resolution.
func (c WaveformConfig) Validate() error {
	if c.BucketMS <= 0 || c.BucketMS > 1000 {
		return fmt.Errorf("waveform bucket_ms must be between 1 and 1000")
	}
	if c.SampleRate < 8000 || c.SampleRate > 96000 {
		return fmt.Errorf("waveform sample_rate must be between 8000 and 96000")
	}
	if c.SampleRate*c.BucketMS/1000 < 1 {
		return fmt.Errorf("waveform bucket is shorter than one sample")
	}
	return nil
}

// GenerateWaveformPyramid decodes the staged audio to mono PCM and publishes
// the pair: the payload under a content-addressed sibling name, then
// <dst>.json (the manifest, the pair's commit marker) naming it in
// payload_file. The payload the previous manifest managed is removed only
// after the manifest switches.
//
// The payload stores level 0 then each coarser level, as little-endian float32
// (min,max) pairs. A level is built by pairing adjacent buckets and carrying an
// odd tail forward unchanged, so every level's bucket count is ceil(prev/2) and
// the final level always has exactly one bucket. Silence produces real zero
// extrema — the renderer draws what the recorder captured, not decoration.
func GenerateWaveformPyramid(ctx context.Context, audioPath, dstPath string, cfg WaveformConfig) (WaveformManifest, error) {
	if err := cfg.Validate(); err != nil {
		return WaveformManifest{}, err
	}
	if strings.TrimSpace(audioPath) == "" || strings.TrimSpace(dstPath) == "" {
		return WaveformManifest{}, fmt.Errorf("waveform needs a source and a destination")
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return WaveformManifest{}, err
	}

	level0, err := decodeBuckets(ctx, audioPath, cfg)
	if err != nil {
		return WaveformManifest{}, err
	}
	levels := [][]waveformExtrema{level0}
	for len(levels[len(levels)-1]) > 1 {
		levels = append(levels, coarsen(levels[len(levels)-1]))
	}

	manifest := WaveformManifest{
		Version:    WaveformManifestVersion,
		SampleRate: cfg.SampleRate,
		BucketMS:   cfg.BucketMS,
		DurationMS: int64(len(level0)) * int64(cfg.BucketMS),
	}
	var payload []byte
	offset := 0
	for level, buckets := range levels {
		manifest.Levels = append(manifest.Levels, WaveformLevel{Level: level, Offset: offset, Count: len(buckets)})
		for _, bucket := range buckets {
			var pair [8]byte
			binary.LittleEndian.PutUint32(pair[0:4], math.Float32bits(bucket.min))
			binary.LittleEndian.PutUint32(pair[4:8], math.Float32bits(bucket.max))
			payload = append(payload, pair[:]...)
		}
		offset += len(buckets)
	}
	manifest.PayloadSize = int64(len(payload))
	sum := sha256.Sum256(payload)
	manifest.PayloadSHA = hex.EncodeToString(sum[:])

	// The payload is published under a content-addressed sibling name and the
	// manifest names it in payload_file. The manifest is the commit marker:
	// until it names the new payload, readers keep using the old pair, so a
	// crash between the two renames leaves the previously served pair intact
	// and only a byte-identifiable orphan behind, which the next generation
	// reuses (same content, same name) or cache maintenance reclaims.
	// The prior manifest is read BEFORE the payload publish: once the new
	// manifest lands, its word is about the new pair, and the old payload's
	// owner can only be proven from what the manifest said before the switch.
	priorPayload, priorKnown := managedWaveformPayloadPath(dstPath + ".json")
	payloadPath := filepath.Join(filepath.Dir(dstPath), "waveform."+manifest.PayloadSHA+".bin")
	if err := publishAtomic(payloadPath, payload); err != nil {
		return WaveformManifest{}, err
	}
	manifest.PayloadFile = filepath.Base(payloadPath)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return WaveformManifest{}, err
	}
	if err := publishAtomic(dstPath+".json", encoded); err != nil {
		return WaveformManifest{}, err
	}
	// The manifest now names the new payload, so the binary the previous
	// manifest managed is unreferenced. It is removed only after the switch,
	// and only when it could be proven managed (the prior manifest was
	// readable and named it); an unprovable file is left for cache
	// maintenance rather than deleted on a guess. Removal failure is not an
	// error: the served pair is already consistent, and the leftover is
	// content-addressed litter that GC classifies.
	if priorKnown && priorPayload != payloadPath {
		_ = os.Remove(priorPayload)
	}
	return manifest, nil
}

// managedWaveformPayloadPath reports which payload a previously published
// manifest at manifestPath managed, and whether it could be proven managed at
// all. An unreadable or malformed prior manifest proves nothing, so nothing
// it may have owned is ever removed on its word.
func managedWaveformPayloadPath(manifestPath string) (string, bool) {
	prior, err := LoadWaveformManifest(manifestPath)
	if err != nil {
		return "", false
	}
	path := WaveformPayloadPath(manifestPath, prior)
	if path == "" {
		return "", false
	}
	return path, true
}

type waveformExtrema struct {
	min, max float32
}

func coarsen(prev []waveformExtrema) []waveformExtrema {
	out := make([]waveformExtrema, 0, (len(prev)+1)/2)
	for i := 0; i < len(prev); i += 2 {
		bucket := prev[i]
		if i+1 < len(prev) {
			bucket.min = minFloat32(bucket.min, prev[i+1].min)
			bucket.max = maxFloat32(bucket.max, prev[i+1].max)
		}
		out = append(out, bucket)
	}
	return out
}

// decodeBuckets streams ffmpeg's s16le output and folds it into 10 ms buckets.
// A non-zero ffmpeg exit is an error, not an empty waveform: "the file did not
// decode" and "the file is silent" must stay distinguishable.
func decodeBuckets(ctx context.Context, audioPath string, cfg WaveformConfig) ([]waveformExtrema, error) {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", audioPath,
		"-vn", "-ac", "1", "-ar", fmt.Sprintf("%d", cfg.SampleRate),
		"-f", "s16le", "-acodec", "pcm_s16le", "-",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	stderr.Grow(2048)
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: 8 << 10}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	samplesPerBucket := cfg.SampleRate * cfg.BucketMS / 1000
	buckets := make([]waveformExtrema, 0, 1024)
	buf := make([]byte, 64<<10)
	pending := make([]byte, 0, 1)
	remaining := samplesPerBucket
	current := waveformExtrema{}
	started := false

	flush := func() {
		buckets = append(buckets, current)
		current = waveformExtrema{}
		remaining = samplesPerBucket
		started = false
	}
	consumeSample := func(sample int16) {
		value := float32(sample) / 32768
		if !started {
			current.min, current.max = value, value
			started = true
		} else {
			current.min = minFloat32(current.min, value)
			current.max = maxFloat32(current.max, value)
		}
		remaining--
		if remaining == 0 {
			flush()
		}
	}

	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			data := buf[:n]
			if len(pending) == 1 {
				data = append(pending, data...)
				pending = pending[:0]
			}
			limit := len(data) - len(data)%2
			for i := 0; i < limit; i += 2 {
				consumeSample(int16(binary.LittleEndian.Uint16(data[i : i+2])))
			}
			if limit < len(data) {
				pending = append(pending, data[limit])
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = cmd.Wait()
			return nil, fmt.Errorf("read ffmpeg pcm: %w", readErr)
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg waveform decode: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// A trailing partial bucket is real audio; keep it rather than dropping the
	// last few milliseconds of every clip.
	if started {
		flush()
	}
	if len(buckets) == 0 {
		return nil, fmt.Errorf("audio decoded to no samples")
	}
	return buckets, nil
}

// ReadWaveformManifest decodes a published manifest from an opened handle
// with bounded reading and shape validation. It exists so a caller that has
// already secured a rooted, regular-file handle (the API serving path) never
// has to re-open the path itself, while getting the same guarantees as
// LoadWaveformManifest: a manifest is at most manifestMaxBytes, so a corrupt
// or hostile file cannot balloon memory, and its shape must be usable before
// any payload byte is read.
func ReadWaveformManifest(r io.Reader) (WaveformManifest, error) {
	encoded, err := io.ReadAll(io.LimitReader(r, manifestMaxBytes+1))
	if err != nil {
		return WaveformManifest{}, err
	}
	if int64(len(encoded)) > manifestMaxBytes {
		return WaveformManifest{}, fmt.Errorf("waveform manifest exceeds %d bytes", manifestMaxBytes)
	}
	var manifest WaveformManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return WaveformManifest{}, fmt.Errorf("waveform manifest is corrupt: %w", err)
	}
	if manifest.Version != WaveformManifestVersion {
		return WaveformManifest{}, fmt.Errorf("waveform manifest version %q is not supported", manifest.Version)
	}
	if manifest.BucketMS <= 0 {
		return WaveformManifest{}, fmt.Errorf("waveform manifest has no bucket width")
	}
	if manifest.SampleRate <= 0 {
		return WaveformManifest{}, fmt.Errorf("waveform manifest has no sample rate")
	}
	if manifest.DurationMS < 0 {
		return WaveformManifest{}, fmt.Errorf("waveform manifest has a negative duration")
	}
	if len(manifest.Levels) == 0 {
		return WaveformManifest{}, fmt.Errorf("waveform manifest has no levels")
	}
	if manifest.PayloadSize < 8 {
		return WaveformManifest{}, fmt.Errorf("waveform manifest has an empty payload")
	}
	if len(manifest.PayloadSHA) != 64 {
		return WaveformManifest{}, fmt.Errorf("waveform manifest has no payload digest")
	}
	if manifest.PayloadFile != "" && !isSafePayloadName(manifest.PayloadFile) {
		return WaveformManifest{}, fmt.Errorf("waveform manifest names an unusable payload file")
	}
	var pairs int64
	for _, level := range manifest.Levels {
		if level.Count < 0 || level.Offset < 0 {
			return WaveformManifest{}, fmt.Errorf("waveform manifest level %d is malformed", level.Level)
		}
		pairs = max64(pairs, int64(level.Offset)+int64(level.Count))
	}
	if pairs*8 > manifest.PayloadSize {
		return WaveformManifest{}, fmt.Errorf("waveform payload is shorter than its manifest")
	}
	return manifest, nil
}

// manifestMaxBytes bounds one manifest: even a fully populated pyramid has a
// handful of levels, so anything near this size is not a manifest this Hub
// wrote.
const manifestMaxBytes = 1 << 20

// isSafePayloadName reports whether name can only resolve to a sibling of the
// manifest: one path segment, no separators, no dot-dot escape.
func isSafePayloadName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return false
	}
	return filepath.Base(name) == name
}

// LoadWaveformManifest reads and validates the manifest half of a published
// pair from its path.
func LoadWaveformManifest(manifestPath string) (WaveformManifest, error) {
	f, err := os.Open(manifestPath)
	if err != nil {
		return WaveformManifest{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return WaveformManifest{}, err
	}
	if !info.Mode().IsRegular() {
		return WaveformManifest{}, fmt.Errorf("waveform manifest is not a regular file")
	}
	return ReadWaveformManifest(f)
}

// WaveformPayloadPath derives the payload half's path for a manifest. Newer
// manifests name their sibling payload in payload_file; legacy manifests have
// no such field and keep their fixed <manifest-base>.bin sibling. A manifest
// naming an unsafe payload file could not have come from this Hub; Load
// validation rejects it, and this derivation falls back to the legacy
// location so a caller holding only a decoded manifest still stays inside the
// managed directory.
func WaveformPayloadPath(manifestPath string, manifest WaveformManifest) string {
	if manifest.PayloadFile != "" && isSafePayloadName(manifest.PayloadFile) {
		return filepath.Join(filepath.Dir(manifestPath), manifest.PayloadFile)
	}
	return strings.TrimSuffix(manifestPath, ".json") + ".bin"
}

// VerifyWaveformPair checks the published pair at manifestPath: the manifest
// must decode and the payload it names must exist as a regular file with
// exactly the manifest's recorded size and digest. It reads the whole payload
// once, so it belongs to derive, repair and maintenance paths — never to a
// per-request serving path, which reads only the viewport it needs.
func VerifyWaveformPair(manifestPath string) (WaveformManifest, error) {
	manifest, err := LoadWaveformManifest(manifestPath)
	if err != nil {
		return WaveformManifest{}, err
	}
	if err := VerifyWaveformPayload(manifest, WaveformPayloadPath(manifestPath, manifest)); err != nil {
		return WaveformManifest{}, err
	}
	return manifest, nil
}

// VerifyWaveformPayload checks one half of a pair against an already decoded
// manifest: the payload must be a regular file of exactly the manifest's
// recorded size whose SHA-256 digest matches. A mismatching or missing
// payload means the pair is incomplete and must be regenerated, never served.
func VerifyWaveformPayload(manifest WaveformManifest, payloadPath string) error {
	info, err := os.Stat(payloadPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("waveform payload %s is not a regular file", payloadPath)
	}
	if info.Size() != manifest.PayloadSize {
		return fmt.Errorf("waveform payload is %d bytes, manifest says %d", info.Size(), manifest.PayloadSize)
	}
	f, err := os.Open(payloadPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(sum.Sum(nil)), manifest.PayloadSHA) {
		return fmt.Errorf("waveform payload does not match its manifest digest")
	}
	return nil
}

// ReadWaveformBuckets serves an inclusive viewport from a published manifest
// and an in-memory payload. It is the slice entry point over the same decoder
// as ReadWaveformBucketsAt and never reads more than maxBuckets pairs.
func ReadWaveformBuckets(manifest WaveformManifest, payload []byte, level int, startMS, endMS int64, maxBuckets int) ([]WaveformBucket, error) {
	return ReadWaveformBucketsAt(manifest, bytes.NewReader(payload), int64(len(payload)), level, startMS, endMS, maxBuckets)
}

// ReadWaveformBucketsAt serves an inclusive viewport from an opened payload
// handle. payloadSize is the caller's stat of the opened file and bounds every
// read: the manifest's geometry is validated first, then only the byte window
// the viewport needs is pulled from the handle, so serving a zoomed-in
// viewport of a long waveform never reads — let alone hashes — the whole
// payload.
func ReadWaveformBucketsAt(manifest WaveformManifest, payload io.ReaderAt, payloadSize int64, level int, startMS, endMS int64, maxBuckets int) ([]WaveformBucket, error) {
	if payload == nil {
		return nil, fmt.Errorf("waveform payload is unavailable")
	}
	first, last, width, desc, err := viewportBounds(manifest, level, startMS, endMS, maxBuckets)
	if err != nil {
		return nil, err
	}
	if last < first {
		return []WaveformBucket{}, nil
	}
	windowStart := (int64(desc.Offset) + first) * 8
	windowEnd := (int64(desc.Offset) + last + 1) * 8
	if manifest.PayloadSize > 0 && payloadSize < manifest.PayloadSize {
		return nil, fmt.Errorf("waveform payload is shorter than its manifest")
	}
	if windowEnd > payloadSize {
		return nil, fmt.Errorf("waveform payload is shorter than its manifest")
	}
	window := make([]byte, windowEnd-windowStart)
	if _, err := payload.ReadAt(window, windowStart); err != nil {
		return nil, fmt.Errorf("waveform payload read: %w", err)
	}
	out := make([]WaveformBucket, 0, last-first+1)
	for i := first; i <= last; i++ {
		pairOffset := (i - first) * 8
		out = append(out, WaveformBucket{
			StartMS: i * width,
			Min:     math.Float32frombits(binary.LittleEndian.Uint32(window[pairOffset : pairOffset+4])),
			Max:     math.Float32frombits(binary.LittleEndian.Uint32(window[pairOffset+4 : pairOffset+8])),
		})
	}
	return out, nil
}

// viewportBounds validates a viewport request against the manifest and returns
// the inclusive half-open bucket range [first,last] clamped to the level's
// bucket count and the maxBuckets cap, plus the level's bucket width in
// milliseconds and its descriptor.
func viewportBounds(manifest WaveformManifest, level int, startMS, endMS int64, maxBuckets int) (first, last, width int64, desc WaveformLevel, err error) {
	if level < 0 || level >= len(manifest.Levels) {
		return 0, 0, 0, WaveformLevel{}, fmt.Errorf("waveform level %d is out of range", level)
	}
	if manifest.BucketMS <= 0 {
		return 0, 0, 0, WaveformLevel{}, fmt.Errorf("waveform manifest has no bucket width")
	}
	if startMS < 0 || endMS <= startMS {
		return 0, 0, 0, WaveformLevel{}, fmt.Errorf("waveform viewport [%d,%d) is invalid", startMS, endMS)
	}
	desc = manifest.Levels[level]
	width = int64(manifest.BucketMS) << level
	first = startMS / width
	last = (endMS - 1) / width
	if last >= int64(desc.Count) {
		last = int64(desc.Count) - 1
	}
	if first < 0 {
		first = 0
	}
	if maxBuckets > 0 && last-first+1 > int64(maxBuckets) {
		last = first + int64(maxBuckets) - 1
	}
	return first, last, width, desc, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minFloat32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

func maxFloat32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// limitedWriter bounds a captured stderr so a chatty decoder cannot fill
// memory with a diagnostic that will be truncated anyway.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	if len(p) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.w.Write(p)
	l.remaining -= n
	return len(p), err
}

func publishAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".waveform-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
