// Package eval is the offline benchmark harness for comparing video
// understanding providers on Timingdex's own workload: real clips through the
// real pipeline into a real database, scored by the same hybrid retrieval the
// product serves. It deliberately contains no provider-specific code — a run
// is just a config plus a corpus — so comparing "Gemini Flash" against
// "Qwen3-VL-4B via llama.cpp" is a matter of running the harness twice with
// different config.json files.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExpectedShot is one ground-truth span: the clip (by corpus filename) and
// the time range in milliseconds where the query's content actually appears.
type ExpectedShot struct {
	Asset   string `json:"asset"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// Query is one retrieval question plus its ground truth.
type Query struct {
	Query    string         `json:"query"`
	Language string         `json:"language,omitempty"`
	Expected []ExpectedShot `json:"expected"`
}

// Corpus is the on-disk evaluation corpus: clips/ plus ground_truth.json.
type Corpus struct {
	Dir     string
	Clips   []Clip
	Queries []Query
}

type Clip struct {
	Name string
	Path string
}

// LoadCorpus reads corpus/ground_truth.json and lists corpus/clips.
func LoadCorpus(dir string) (*Corpus, error) {
	clipsDir := filepath.Join(dir, "clips")
	entries, err := os.ReadDir(clipsDir)
	if err != nil {
		return nil, fmt.Errorf("read corpus clips: %w", err)
	}
	corpus := &Corpus{Dir: dir}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		corpus.Clips = append(corpus.Clips, Clip{Name: name, Path: filepath.Join(clipsDir, name)})
	}
	if len(corpus.Clips) == 0 {
		return nil, fmt.Errorf("corpus %q has no clips under clips/", dir)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ground_truth.json"))
	if err != nil {
		return nil, fmt.Errorf("read ground truth: %w", err)
	}
	if err := json.Unmarshal(raw, &corpus.Queries); err != nil {
		return nil, fmt.Errorf("decode ground truth: %w", err)
	}
	if len(corpus.Queries) == 0 {
		return nil, fmt.Errorf("corpus %q has no queries in ground_truth.json", dir)
	}
	return corpus, nil
}

// ClipByName resolves a ground-truth asset reference to its corpus clip.
func (c *Corpus) ClipByName(name string) (*Clip, bool) {
	for i := range c.Clips {
		if c.Clips[i].Name == name {
			return &c.Clips[i], true
		}
	}
	return nil, false
}

// AssetReport is the per-clip outcome of one run.
type AssetReport struct {
	Clip            string  `json:"clip"`
	AssetID         string  `json:"asset_id"`
	DurationMS      int64   `json:"duration_ms"`
	ProcessingMS    int64   `json:"processing_ms"`
	RealTimeFactor  float64 `json:"real_time_factor"`
	Shots           int     `json:"shots"`
	FramesProcessed int     `json:"frames_processed"`
	Analyzed        bool    `json:"analyzed"`
}

// RunReport is everything the score step needs about a run, plus the raw
// timing the operator wants to see next to the retrieval metrics.
type RunReport struct {
	Label     string        `json:"label"`
	Provider  string        `json:"provider"`
	Model     string        `json:"model"`
	StartedAt time.Time     `json:"started_at"`
	Assets    []AssetReport `json:"assets"`
}

// Save writes the report into the run directory.
func (r *RunReport) Save(dataDir, label string) error {
	dir := filepath.Join(dataDir, "runs", label)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.json"), raw, 0o600)
}

// LoadReport reads a run's saved report.
func LoadReport(dataDir, label string) (*RunReport, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "runs", label, "report.json"))
	if err != nil {
		return nil, err
	}
	var report RunReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, err
	}
	return &report, nil
}
