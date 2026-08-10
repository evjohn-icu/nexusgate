// Command timingdex-corpusgen generates the offline eval corpus for
// timingdex-eval: deterministic ffmpeg clips plus ground_truth.json in the
// exact format internal/eval LoadCorpus expects.
//
//	# generate the corpus into ./corpus
//	timingdex-corpusgen --out ./corpus
//
//	# then benchmark a provider config the way the Hub itself would:
//	timingdex-eval run  --corpus ./corpus --data-dir ./eval/qwen   --label qwen3vl-4b
//	timingdex-eval run  --corpus ./corpus --data-dir ./eval/gemini --label gemini-flash
//	timingdex-eval score --corpus ./corpus --data-dir ./eval --labels qwen3vl-4b,gemini-flash
//
// The clips are synthetic (lavfi test sources), so the corpus is
// reproducible, licence-free and safe to commit or distribute: no personal
// footage ever enters it. The scene sequence of each clip mirrors the
// corresponding adversarial asset in the retrieval golden set
// (internal/repository/sqlite/retrieval_golden_corpus_test.go): the car only
// in the second half, the beach asset with an indoor shot, a window-boundary
// shot, timed speech, and so on. Running a real video-understanding provider
// over these clips and scoring the result is the manual, offline benchmark —
// it is never part of CI.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// scene is one segment of a generated clip: a lavfi source for a number of
// seconds. testsrc (color bars + counter) and testsrc2 (gradient + counter)
// are visibly distinct, which is what a scene detector needs to separate.
type scene struct {
	source string
	secs   int
}

type clipSpec struct {
	name    string // clip filename (without .mp4); also the golden asset id
	scenes  []scene
	queries []corpusQuery
}

type corpusQuery struct {
	query    string
	language string
	expected []expectedShot
}

type expectedShot struct {
	asset   string
	startMS int64
	endMS   int64
}

// groundTruthQuery is one line of ground_truth.json, mirroring
// internal/eval's Query shape.
type groundTruthQuery struct {
	Query    string            `json:"query"`
	Language string            `json:"language"`
	Expected []groundTruthSpan `json:"expected"`
}

type groundTruthSpan struct {
	Asset   string `json:"asset"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

func main() {
	out := flag.String("out", "./corpus", "corpus output directory")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `timingdex-corpusgen — generate the offline eval corpus for timingdex-eval

Writes <out>/clips/*.mp4 (deterministic synthetic clips) and <out>/ground_truth.json
(the queries + expected shot spans internal/eval LoadCorpus expects). The clips
mirror the retrieval golden set's adversarial assets, so timingdex-eval scores a
provider the way the Hub's own search would. Never part of CI.

Usage:
  timingdex-corpusgen --out <dir>

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := generate(*out); err != nil {
		fmt.Fprintf(os.Stderr, "timingdex-corpusgen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("corpus written to %s (clips/ + ground_truth.json)\n", *out)
}

// ground_truth.json is a top-level JSON ARRAY of queries — the exact shape
// internal/eval LoadCorpus unmarshals (json.Unmarshal into Corpus.Queries).
// Wrapping it in an object would load fine by hand and fail at eval time with
// "cannot unmarshal object into Go value of type []eval.Query".
func generate(out string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required to synthesize clips: %w", err)
	}
	clips := goldenClips()
	clipsDir := filepath.Join(out, "clips")
	if err := os.MkdirAll(clipsDir, 0o755); err != nil {
		return err
	}
	queries := []groundTruthQuery{}
	for _, clip := range clips {
		path := filepath.Join(clipsDir, clip.name+".mp4")
		if err := synthClip(clip, path); err != nil {
			return err
		}
		for _, q := range clip.queries {
			entry := groundTruthQuery{Query: q.query, Language: q.language}
			for _, e := range q.expected {
				entry.Expected = append(entry.Expected, groundTruthSpan{
					Asset:   clip.name + ".mp4",
					StartMS: e.startMS,
					EndMS:   e.endMS,
				})
			}
			queries = append(queries, entry)
		}
	}
	raw, err := json.MarshalIndent(queries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "ground_truth.json"), raw, 0o644)
}

// synthClip concatenates the scene sources into one mp4, mirroring the
// scene-detection integration test's concat pattern.
func synthClip(clip clipSpec, path string) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	var filterParts []string
	for i, sc := range clip.scenes {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("%s=duration=%d:size=320x240:rate=25", sc.source, sc.secs))
		filterParts = append(filterParts, fmt.Sprintf("[%d:v]trim=duration=%d[v%d]", i, sc.secs, i))
	}
	var concatParts []string
	for i := range clip.scenes {
		concatParts = append(concatParts, fmt.Sprintf("[v%d]", i))
	}
	args = append(args, "-filter_complex",
		fmt.Sprintf("%s;%sconcat=n=%d:v=1",
			strings.Join(filterParts, ";"), strings.Join(concatParts, ""), len(clip.scenes)))
	args = append(args, "-an", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path)
	if raw, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("synth %s: %w: %s", path, err, truncate(raw))
	}
	return nil
}

// goldenClips maps the retrieval golden set's adversarial families onto
// synthetic clips. The scene sequence is the only thing the model can see,
// so the expected spans below are the ground truth the corpus claims.
func goldenClips() []clipSpec {
	sceneA := scene{source: "testsrc", secs: 2}
	sceneB := scene{source: "testsrc2", secs: 2}
	return []clipSpec{
		// car only in the second half (golden asset asset-car-second-half).
		{
			name:   "clip-car-second-half",
			scenes: []scene{sceneA, sceneB},
			queries: []corpusQuery{
				{query: "car", language: "en", expected: []expectedShot{{asset: "clip-car-second-half.mp4", startMS: 2000, endMS: 4000}}},
				{query: "汽车", language: "zh", expected: []expectedShot{{asset: "clip-car-second-half.mp4", startMS: 2000, endMS: 4000}}},
			},
		},
		// beach asset with an indoor shot (asset-beach-indoor): scene A is
		// the "indoor" half, scene B the "beach" half.
		{
			name:   "clip-beach-indoor",
			scenes: []scene{sceneA, sceneB},
			queries: []corpusQuery{
				{query: "beach", language: "en", expected: []expectedShot{{asset: "clip-beach-indoor.mp4", startMS: 2000, endMS: 4000}}},
			},
		},
		// window-boundary shot: the scene cut lands exactly at the 2s mark
		// of a 4s clip, mirroring asset-window-boundary's pinned times.
		{
			name:   "clip-window-boundary",
			scenes: []scene{sceneA, sceneB},
			queries: []corpusQuery{
				{query: "scene change", language: "en", expected: []expectedShot{{asset: "clip-window-boundary.mp4", startMS: 2000, endMS: 4000}}},
			},
		},
		// timed speech asset (asset-timed-speech): the narration interval
		// is the second half of the clip.
		{
			name:   "clip-timed-speech",
			scenes: []scene{sceneA, sceneB},
			queries: []corpusQuery{
				{query: "narration", language: "en", expected: []expectedShot{{asset: "clip-timed-speech.mp4", startMS: 2000, endMS: 4000}}},
			},
		},
	}
}

func truncate(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 512 {
		return text[:512] + "…(truncated)"
	}
	return text
}
