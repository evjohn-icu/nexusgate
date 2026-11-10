package media

import (
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

// WaveformManifest is the JSON half of the artifact.
type WaveformManifest struct {
	Version     string          `json:"version"`
	SampleRate  int             `json:"sample_rate"`
	BucketMS    int             `json:"bucket_ms"`
	DurationMS  int64           `json:"duration_ms"`
	Levels      []WaveformLevel `json:"levels"`
	PayloadSize int64           `json:"payload_size"`
	PayloadSHA  string          `json:"payload_sha256"`
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

// GenerateWaveformPyramid decodes the staged audio to mono PCM and writes two
// artifacts: <dst>.json (the manifest) and <dst>.bin (the samples).
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

	if err := publishAtomic(dstPath+".bin", payload); err != nil {
		return WaveformManifest{}, err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return WaveformManifest{}, err
	}
	if err := publishAtomic(dstPath+".json", encoded); err != nil {
		return WaveformManifest{}, err
	}
	return manifest, nil
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

// ReadWaveformBuckets serves an inclusive viewport from a published manifest
// and its payload. It never reads more than maxBuckets pairs.
func ReadWaveformBuckets(manifest WaveformManifest, payload []byte, level int, startMS, endMS int64, maxBuckets int) ([]WaveformBucket, error) {
	if level < 0 || level >= len(manifest.Levels) {
		return nil, fmt.Errorf("waveform level %d is out of range", level)
	}
	if manifest.BucketMS <= 0 {
		return nil, fmt.Errorf("waveform manifest has no bucket width")
	}
	if startMS < 0 || endMS <= startMS {
		return nil, fmt.Errorf("waveform viewport [%d,%d) is invalid", startMS, endMS)
	}
	desc := manifest.Levels[level]
	width := int64(manifest.BucketMS) << level
	first := startMS / width
	last := (endMS - 1) / width
	if last >= int64(desc.Count) {
		last = int64(desc.Count) - 1
	}
	if first < 0 {
		first = 0
	}
	if last < first {
		return []WaveformBucket{}, nil
	}
	if maxBuckets > 0 && last-first+1 > int64(maxBuckets) {
		last = first + int64(maxBuckets) - 1
	}
	out := make([]WaveformBucket, 0, last-first+1)
	for i := first; i <= last; i++ {
		pairOffset := (desc.Offset + int(i)) * 8
		if pairOffset+8 > len(payload) {
			return nil, fmt.Errorf("waveform payload is shorter than its manifest")
		}
		out = append(out, WaveformBucket{
			StartMS: i * width,
			Min:     math.Float32frombits(binary.LittleEndian.Uint32(payload[pairOffset : pairOffset+4])),
			Max:     math.Float32frombits(binary.LittleEndian.Uint32(payload[pairOffset+4 : pairOffset+8])),
		})
	}
	return out, nil
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
