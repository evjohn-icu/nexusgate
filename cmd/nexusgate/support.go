package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/support"
)

// runSupportBundleCommand assembles the `nexusgate support bundle` archive:
// a zip of Hub diagnostics plus a sanitized configuration, safe to hand to an
// operator or upstream support. Safety is the point of the feature — the
// bundle redacts every secret-bearing configuration value and reduces every
// absolute path to its basename, and it never reads provider-secrets/,
// source media, transcripts or raw model responses — so the command states
// both what it included and what it provably excluded.
func runSupportBundleCommand(service *app.Service, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("support bundle", flag.ContinueOnError)
	out := flags.String("out", "", "output zip path (default ./nexusgate-support-<timestamp>.zip)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	report, err := service.DoctorReport(ctx)
	if err != nil {
		return fmt.Errorf("collect doctor report: %w", err)
	}
	outPath := *out
	if outPath == "" {
		outPath = filepath.Join(".", fmt.Sprintf("nexusgate-support-%s.zip", time.Now().Format("20060102-150405")))
	}
	if err := support.Generate(ctx, report, cfg, service, outPath); err != nil {
		return fmt.Errorf("generate support bundle: %w", err)
	}
	fmt.Printf("support bundle written to %s\n", outPath)
	fmt.Println("included: system, database schema and integrity, storage, library roots (basenames only), ffmpeg/ffprobe/exiftool, GPU, provider channel counts, search index, workers, queue summary, recent failure codes, sanitized config, doctor text")
	fmt.Println("excluded: API keys, admin and agent tokens, provider secrets, source media, transcript content, raw model responses, absolute source paths")
	return nil
}
