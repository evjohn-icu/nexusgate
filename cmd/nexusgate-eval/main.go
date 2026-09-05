// Command nexusgate-eval is the offline benchmark harness for comparing video
// understanding providers on NexusGate's own workload.
//
//	nexusgate-eval run --corpus ./corpus --data-dir ./eval/gemini --label gemini-flash
//	nexusgate-eval run --corpus ./corpus --data-dir ./eval/qwen --label qwen3vl-4b
//	nexusgate-eval score --corpus ./corpus --data-dir ./eval --labels gemini-flash,qwen3vl-4b
//
// A run is one providers configuration over one corpus. The config is the
// data dir's config.json (the same file a real nexusgate Hub would use), so
// comparing models is comparing data dirs. Each run gets its own SQLite
// database under the data dir; score opens each run's database and evaluates
// it against corpus/ground_truth.json with the product's hybrid retrieval.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/eval"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "score":
		err = scoreCmd(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexusgate-eval: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `nexusgate-eval — offline provider benchmark harness

Usage:
  nexusgate-eval run --corpus <dir> --data-dir <dir> --label <name>
  nexusgate-eval score --corpus <dir> --data-dir <dir> --labels <a,b,...>

The data dir holds the providers config.json (the same file a nexusgate Hub
uses) and receives its own nexusgate.db. Set NEXUSGATE_DATA_DIR instead of
--data-dir if you prefer.

Corpus layout:
  <corpus>/clips/*.mp4              the footage
  <corpus>/ground_truth.json        [{"query":"...","language":"zh",
                                     "expected":[{"asset":"clip.mp4",
                                     "start_ms":0,"end_ms":5000}]}]
`)
}

func parseRunArgs(args []string) (corpus, dataDir, label string, err error) {
	fs := flag.NewFlagSet("nexusgate-eval run", flag.ContinueOnError)
	corpusFlag := fs.String("corpus", "", "corpus directory (clips/ + ground_truth.json)")
	dataDirFlag := fs.String("data-dir", "", "evaluation data directory")
	labelFlag := fs.String("label", "", "run label")
	if err := fs.Parse(args); err != nil {
		return "", "", "", err
	}
	if *corpusFlag == "" {
		return "", "", "", fmt.Errorf("--corpus is required")
	}
	if *dataDirFlag == "" {
		return "", "", "", fmt.Errorf("--data-dir is required")
	}
	if *labelFlag == "" {
		return "", "", "", fmt.Errorf("--label is required")
	}
	return *corpusFlag, *dataDirFlag, *labelFlag, nil
}

func parseScoreArgs(args []string) (corpus, dataDir, labels string, err error) {
	fs := flag.NewFlagSet("nexusgate-eval score", flag.ContinueOnError)
	corpusFlag := fs.String("corpus", "", "corpus directory (clips/ + ground_truth.json)")
	dataDirFlag := fs.String("data-dir", "", "evaluation data directory")
	labelsFlag := fs.String("labels", "", "comma-separated run labels")
	if err := fs.Parse(args); err != nil {
		return "", "", "", err
	}
	if *corpusFlag == "" {
		return "", "", "", fmt.Errorf("--corpus is required")
	}
	if *dataDirFlag == "" {
		return "", "", "", fmt.Errorf("--data-dir is required")
	}
	if *labelsFlag == "" {
		return "", "", "", fmt.Errorf("--labels is required")
	}
	return *corpusFlag, *dataDirFlag, *labelsFlag, nil
}

func runCmd(ctx context.Context, args []string) error {
	corpus, dataDir, label, err := parseRunArgs(args)
	if err != nil {
		return err
	}
	loaded, err := eval.LoadCorpus(corpus)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	// config.Load reads NEXUSGATE_DATA_DIR for its defaults and the data
	// dir's config.json for overrides — exactly the Hub's own resolution.
	if err := os.Setenv("NEXUSGATE_DATA_DIR", dataDir); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config from %s: %w", dataDir, err)
	}
	report, err := eval.Run(ctx, cfg, loaded, dataDir, label)
	if err != nil {
		return err
	}
	fmt.Printf("run %q complete: %s / %s, %d clip(s)\n", report.Label, report.Provider, report.Model, len(report.Assets))
	for _, asset := range report.Assets {
		status := "analyzed"
		if !asset.Analyzed {
			status = "NOT analyzed"
		}
		fmt.Printf("  %-28s %6.1fs duration  %8dms process  %5.1fx RT  %3d shots  %4d frames  %s\n",
			asset.Clip, float64(asset.DurationMS)/1000, asset.ProcessingMS, asset.RealTimeFactor, asset.Shots, asset.FramesProcessed, status)
	}
	return nil
}

func scoreCmd(ctx context.Context, args []string) error {
	corpus, dataDir, labels, err := parseScoreArgs(args)
	if err != nil {
		return err
	}
	loaded, err := eval.LoadCorpus(corpus)
	if err != nil {
		return err
	}
	scores := make([]*eval.RunScore, 0, len(labels))
	for _, label := range strings.Split(labels, ",") {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		score, err := eval.Score(ctx, dataDir, label, loaded)
		if err != nil {
			return fmt.Errorf("score %s: %w", label, err)
		}
		if err := eval.SaveScore(score, dataDir); err != nil {
			return err
		}
		scores = append(scores, score)
	}
	if len(scores) == 0 {
		return fmt.Errorf("no runs to score")
	}
	printTable(scores)
	for _, score := range scores {
		printDetail(score)
	}
	return nil
}

func printTable(scores []*eval.RunScore) {
	fmt.Printf("%-24s %6s %6s %6s %4s %6s %6s\n", "Model", "R@10", "P@5", "P@10", "FP", "RT", "frames")
	for _, s := range scores {
		rt := "-"
		if s.MeanRTFactor > 0 {
			rt = fmt.Sprintf("%.1fx", s.MeanRTFactor)
		}
		frames := "-"
		if s.Frames > 0 {
			frames = fmt.Sprintf("%d", s.Frames)
		}
		fmt.Printf("%-24s %6.3f %6.3f %6.3f %4d %6s %6s\n",
			s.Label, s.Recall10, s.Precision5, s.Precision10, s.FalsePositives, rt, frames)
	}
}

func printDetail(score *eval.RunScore) {
	fmt.Printf("\n== %s (%s / %s): %d relevant in top-10 of %d queries ==\n",
		score.Label, score.Provider, score.Model, score.RelevantInTop10, score.TotalQueries)
	for _, q := range score.QueryDetail {
		missed := ""
		if len(q.Missed) > 0 {
			missed = "  missed: " + strings.Join(q.Missed, ", ")
		}
		fp := ""
		if q.FalsePositives > 0 {
			fp = fmt.Sprintf("  FP=%d", q.FalsePositives)
		}
		fmt.Printf("  %-40s hit %d/%d%s%s\n", q.Query, q.HitRelevant, q.TotalRelevant, fp, missed)
	}
}
