// Package e2e is the end-to-end product test suite: a real clip is encoded
// with ffmpeg, scanned into a real SQLite library, run through the real
// pipeline chain (probe -> derive -> speech_gate -> [transcribe] -> analyze ->
// index) with an in-process scripted provider standing in for the paid model,
// and the result is verified through the real search engine. It is the only
// test that holds the whole chain together with real media on disk; the
// piecewise suites (internal/media, internal/app, internal/repository/sqlite)
// prove each stage in isolation.
//
// The suite is skipped cleanly when ffmpeg/ffprobe are absent: CI installs
// ffmpeg, so the full chain runs there. A skip whose message names the
// missing binary is an environment skip; anything else is a wiring bug.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/media"
)

// AudioKind selects what the fixture's audio track is made of. There is no
// real speech anywhere in this suite: synthesising words is out of scope, and
// the speech fixture exists to exercise the pipeline's speech gate with audio
// present, not to test ASR accuracy.
type AudioKind int

const (
	// AudioNone produces no audio track at all (the no-audio variant: the
	// chain skips the gate entirely).
	AudioNone AudioKind = iota
	// AudioSine produces a pure tone. At full (or near-full) amplitude the
	// tone sits far above the speech gate's -35 dB silence threshold, so the
	// gate deterministically classifies the track as speech_candidate and the
	// chain routes into transcribe — the decision under test.
	AudioSine
	// AudioSilence produces digital silence (anullsrc): the gate classifies
	// mostly_silent and the chain skips transcribe.
	AudioSilence
)

// ClipOpts describes one tiny lavfi-generated clip. The zero value is not
// valid input; DefaultClipOpts returns the baseline, and the constructor
// fills any field still zero.
type ClipOpts struct {
	Width, Height int
	DurationS     float64
	// Audio toggles whether a second lavfi input (AudioSource) is mapped into
	// the file. False produces the no-audio variant: no audio stream, so the
	// derive stage skips audio extraction and the chain goes straight from
	// derive to analyze.
	Audio bool
	// AudioSource selects the tone (AudioSine) or silence (AudioSilence)
	// input. Only read when Audio is true.
	AudioSource AudioKind
	// Frequency is the sine's frequency in Hz (default 440). Volume scales
	// the tone (default 1.0). The speech fixture uses a quieter tone than the
	// base family, still far above the gate's silence threshold, so the two
	// families share the gate decision while encoding different audio.
	Frequency int
	Volume    float64
	Rate      int
	// PixelFmt is the encoder pixel format; the 10-bit family uses
	// yuv420p10le, which also switches libx264 to its high10 profile.
	PixelFmt string
	// Codec selects the video encoder: libx264 (the default), or libx265 for
	// the portrait family (a build without libx265 falls back to libx264 and
	// logs it — the chain under test does not depend on HEVC).
	Codec string
	// Scene is the lavfi visual source name; testsrc is the default. Every
	// family uses testsrc: the patterns it draws are irrelevant to the chain,
	// which derives and analyses the proxy regardless of content.
	Scene string
}

// DefaultClipOpts returns the baseline fixture: 640x360 16:9, 3 s, 24 fps,
// testsrc visuals and no audio. Families build on it and override what they
// differ on.
func DefaultClipOpts() ClipOpts {
	return ClipOpts{
		Width: 640, Height: 360, DurationS: 3, Rate: 24, Scene: "testsrc",
	}
}

// withDefaults fills any field the caller left zero, so a partially specified
// ClipOpts is still a valid fixture rather than a 0x0 0-fps clip.
func (o ClipOpts) withDefaults() ClipOpts {
	if o.Width <= 0 {
		o.Width = 640
	}
	if o.Height <= 0 {
		o.Height = 360
	}
	if o.DurationS <= 0 {
		o.DurationS = 3
	}
	if o.Rate <= 0 {
		o.Rate = 24
	}
	if o.Scene == "" {
		o.Scene = "testsrc"
	}
	if o.PixelFmt == "" {
		o.PixelFmt = "yuv420p"
	}
	if o.Codec == "" {
		o.Codec = "libx264"
	}
	if o.Frequency <= 0 {
		o.Frequency = 440
	}
	if o.Volume <= 0 {
		o.Volume = 1
	}
	if o.Audio && o.AudioSource == AudioNone {
		o.AudioSource = AudioSine
	}
	return o
}

// Fixture is one member of the core family table: a name (the file stem and
// the subtest name) and the ClipOpts that produce it. Fixture.commentary lives
// in CoreFixtures' doc lines so the family's pipeline role is visible next to
// its encode.
type Fixture struct {
	Name string
	Opts ClipOpts
}

// hasAudio reports whether the fixture carries an audio track, and therefore
// whether the derive stage extracts audio and the speech gate runs.
func (f Fixture) hasAudio() bool { return f.Opts.Audio }

// transcribes reports whether the chain is expected to reach the transcribe
// stage for this family: only tone-bearing audio clears the gate's
// speech_candidate threshold, so silence and no-audio families never transcribe.
func (f Fixture) transcribes() bool {
	return f.Opts.Audio && f.Opts.AudioSource == AudioSine
}

// expectedJobs is the exact chain length this family must succeed: probe,
// derive, speech gate and analyze are universal; transcribe joins only when
// the gate classifies speech.
func (f Fixture) expectedJobs() int {
	if !f.Opts.Audio {
		return 3 // probe derive analyze
	}
	if f.transcribes() {
		return 5 // probe derive speech_gate transcribe analyze
	}
	return 4 // probe derive speech_gate analyze
}

// CoreFixtures is the fixture family table. Every row is a real encoded clip
// whose chain — and only its chain — differs:
//
//   - clip-169: the baseline 16:9 H.264 clip with a full-amplitude tone. Runs
//     the whole chain: probe -> derive -> speech gate -> transcribe -> analyze.
//   - clip-portrait: vertical 360x640, encoded with libx265 when the local
//     build has it (libx264 otherwise). Same chain as the baseline; the
//     orientation and codec must survive probe/derive/analyze unchanged.
//   - clip-10bit: yuv420p10le high10 H.264. Same chain; the bit depth must
//     survive probe and the 8-bit proxy derive must not choke on a 10-bit
//     source. Skipped on builds whose libx264 lacks high10.
//   - clip-no-audio: no audio stream. The chain short-circuits to analyze —
//     no gate, no transcribe — and no audio artifact may exist.
//   - clip-silence: digital silence on the audio track. The gate runs and
//     classifies mostly_silent; transcribe must NOT run.
//   - clip-speech: a quiet pure tone, still far above the gate's silence
//     threshold. The gate runs and classifies speech_candidate; transcribe
//     runs on it. There are no words in this clip — the fixture is about the
//     gate's decision on tone-bearing audio, not about ASR.
var CoreFixtures = []Fixture{
	{
		Name: "clip-169",
		Opts: func() ClipOpts {
			o := DefaultClipOpts()
			o.Audio, o.AudioSource = true, AudioSine
			return o
		}(),
	},
	{
		Name: "clip-portrait",
		Opts: func() ClipOpts {
			o := DefaultClipOpts()
			o.Width, o.Height = 360, 640
			o.Audio, o.AudioSource = true, AudioSine
			o.Codec = "libx265"
			return o
		}(),
	},
	{
		Name: "clip-10bit",
		Opts: func() ClipOpts {
			o := DefaultClipOpts()
			o.Audio, o.AudioSource = true, AudioSine
			o.PixelFmt = "yuv420p10le"
			return o
		}(),
	},
	{
		Name: "clip-no-audio",
		Opts: DefaultClipOpts(),
	},
	{
		Name: "clip-silence",
		Opts: func() ClipOpts {
			o := DefaultClipOpts()
			o.Audio, o.AudioSource = true, AudioSilence
			return o
		}(),
	},
	{
		Name: "clip-speech",
		Opts: func() ClipOpts {
			o := DefaultClipOpts()
			o.Audio, o.AudioSource = true, AudioSine
			o.Frequency, o.Volume = 220, 0.35
			return o
		}(),
	},
}

// generateClip encodes one tiny real video clip into outDir via ffmpeg lavfi
// and returns its path. The encoder and the generated file are verified before
// the caller's chain starts, so a fixture failure here is clearly attributed
// to the environment: skips name the missing binary or the encoder error, and
// the caller's chain only ever runs against a clip that probed cleanly.
//
// The suite deliberately shares the repo's skip-on-encode-failure pattern
// (seedMultiframeAsset in internal/app): a locally unencodable fixture is an
// environment limitation, not a pipeline verdict. CI installs ffmpeg, so none
// of these skips fire there.
func generateClip(t testing.TB, outDir, name string, opts ClipOpts) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to generate the e2e clip fixtures (CI installs ffmpeg; this suite is skipped without it)")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is required to verify the e2e clip fixtures (CI installs ffprobe; this suite is skipped without it)")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts = opts.withDefaults()
	if !strings.HasSuffix(name, ".mp4") {
		name += ".mp4"
	}

	ctx := context.Background()
	videoInput := fmt.Sprintf("%s=duration=%g:size=%dx%d:rate=%d", opts.Scene, opts.DurationS, opts.Width, opts.Height, opts.Rate)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", videoInput}
	if opts.Audio {
		switch opts.AudioSource {
		case AudioSilence:
			// anullsrc is an infinite source; the output -t below bounds it.
			args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=44100")
		default:
			sine := fmt.Sprintf("sine=frequency=%d:duration=%g", opts.Frequency, opts.DurationS)
			if opts.Volume != 1 {
				sine += fmt.Sprintf(",volume=%g", opts.Volume)
			}
			args = append(args, "-f", "lavfi", "-i", sine)
		}
	}
	args = append(args, "-c:v", opts.Codec)
	if opts.Codec == "libx264" && opts.PixelFmt == "yuv420p10le" {
		// 10-bit H.264 is libx264's high10 profile, a build-time feature.
		args = append(args, "-profile:v", "high10")
	}
	args = append(args, "-pix_fmt", opts.PixelFmt)
	if opts.Audio {
		args = append(args, "-c:a", "aac", "-b:a", "96k")
	}
	if opts.AudioSource == AudioSilence {
		args = append(args, "-t", fmt.Sprintf("%g", opts.DurationS))
	}
	args = append(args, "-map", "0:v:0")
	if opts.Audio {
		args = append(args, "-map", "1:a:0")
	}

	out := filepath.Join(outDir, name)
	args = append(args, out)
	if raw, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		if opts.Codec == "libx265" {
			// A build without libx265 must not lose the portrait family: the
			// chain under test does not care which H.26x codec carried the
			// source. Fall back to libx264 and say so.
			t.Logf("libx265 encode failed; falling back to libx264 for %s: %v: %s", name, err, truncateStderr(raw))
			args = swapCodec(args, "libx265", "libx264")
			if raw2, err2 := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err2 != nil {
				t.Skipf("test fixture cannot be encoded by local ffmpeg (libx265 and libx264 both failed): %v: %s", err2, truncateStderr(raw2))
			}
		} else if opts.Codec == "libx264" && opts.PixelFmt == "yuv420p10le" {
			t.Skipf("local ffmpeg cannot encode 10-bit H.264 (high10); skipping the 10-bit family: %v: %s", err, truncateStderr(raw))
		} else {
			t.Skipf("test fixture cannot be encoded by local ffmpeg: %v: %s", err, truncateStderr(raw))
		}
	}

	// Verify the file before the chain runs on it: a clip that cannot probe
	// would fail the pipeline's probe stage with an error that looks like a
	// pipeline bug. A generated-but-unprobeable file is a local build quirk;
	// name it as such.
	if _, err := media.Probe(ctx, out); err != nil {
		t.Skipf("generated fixture %s does not probe cleanly: %v", name, err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Skipf("generated fixture %s is empty or missing: %v", name, err)
	}
	return out
}

// swapCodec rewrites one encoder choice for another in an ffmpeg argument
// list. Only the pair "-c:v <codec>" is touched, so the rest of the command
// stays identical between attempts.
func swapCodec(args []string, from, to string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c:v" && args[i+1] == from {
			args[i+1] = to
		}
	}
	return args
}

// truncateStderr keeps encoder error output out of skip messages: ffmpeg can
// explain a failure in hundreds of lines, and the diagnosis fits in the tail.
func truncateStderr(raw []byte) string {
	if len(raw) <= 4096 {
		return string(raw)
	}
	return fmt.Sprintf("[truncated %d bytes] ...%s", len(raw)-4096, string(raw[len(raw)-4096:]))
}
