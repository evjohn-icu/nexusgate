package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/ev/timingdex/internal/api"
	"github.com/ev/timingdex/internal/app"
	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/hubtls"
	"github.com/ev/timingdex/internal/media"
	"github.com/ev/timingdex/internal/remote"
	sqliterepo "github.com/ev/timingdex/internal/repository/sqlite"
	"github.com/ev/timingdex/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("timingdex stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return usage()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if os.Args[1] == "worker" {
		return runWorkerCommand()
	}
	repo, err := sqliterepo.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer repo.Close()

	if err := repo.Migrate(context.Background()); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	service, err := app.NewService(repo, cfg)
	if err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}

	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		addr := fs.String("addr", cfg.ListenAddress, "HTTP listen address")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		certificate, key, err := hubTLSFiles(cfg)
		if err != nil {
			return err
		}
		return api.NewTLSServer(*addr, service, certificate, key).Run(ctx)

	case "root":
		if len(os.Args) < 3 {
			return errors.New("usage: timingdex root add|list|scan ...")
		}
		return runRootCommand(context.Background(), service, os.Args[2:])

	case "pipeline":
		if len(os.Args) < 3 || os.Args[2] != "run" {
			return errors.New("usage: timingdex pipeline run")
		}
		return service.RunPipeline(context.Background())

	case "doctor":
		fmt.Printf("database: %s\n", cfg.DatabasePath)
		fmt.Printf("cache:    %s\n", cfg.CacheDir)
		fmt.Printf("listen:   %s\n", cfg.ListenAddress)
		return service.Doctor(context.Background(), os.Stdout)

	default:
		return usage()
	}
}

func hubTLSFiles(cfg config.Config) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.HubTLS.Mode)) {
	case "", "auto":
		certificate, key, fingerprint, err := hubtls.EnsureSelfSigned(cfg.DataDir)
		if err != nil {
			return "", "", err
		}
		fmt.Printf("Hub HTTPS fingerprint: %s\n", fingerprint)
		return certificate, key, nil
	case "files":
		if cfg.HubTLS.CertificateFile == "" || cfg.HubTLS.KeyFile == "" {
			return "", "", errors.New("TIMINGDEX_TLS_CERT_FILE and TIMINGDEX_TLS_KEY_FILE are required for TLS files mode")
		}
		return cfg.HubTLS.CertificateFile, cfg.HubTLS.KeyFile, nil
	case "off":
		return "", "", nil
	default:
		return "", "", fmt.Errorf("unsupported Hub TLS mode %q", cfg.HubTLS.Mode)
	}
}

func runRootCommand(ctx context.Context, service *app.Service, args []string) error {
	switch args[0] {
	case "add":
		if len(args) != 2 {
			return errors.New("usage: timingdex root add <path>")
		}
		root, err := service.AddLibraryRoot(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("added root %s: %s\n", root.ID, root.Path)
		return nil
	case "list":
		roots, err := service.ListLibraryRoots(ctx)
		if err != nil {
			return err
		}
		for _, root := range roots {
			fmt.Printf("%s\t%s\n", root.ID, root.Path)
		}
		return nil
	case "scan":
		if len(args) != 2 {
			return errors.New("usage: timingdex root scan <root-id>")
		}
		result, err := service.ScanLibraryRoot(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("discovered=%d linked=%d missing=%d errors=%d\n", result.Discovered, result.Linked, result.Missing, len(result.Errors))
		for _, scanErr := range result.Errors {
			fmt.Printf("warning: %s\n", scanErr)
		}
		return nil
	default:
		return errors.New("usage: timingdex root add|list|scan ...")
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `Timingdex Footage

Usage:
  timingdex serve [-addr 127.0.0.1:8787]
  timingdex root add <path>
  timingdex root list
  timingdex root scan <root-id>
  timingdex pipeline run
  timingdex worker enroll --hub https://nas:8787 --pairing <token> [--name worker] [--mount root-id=/mounted/path] [--provider-operation video_analysis]
  timingdex worker run [--config path]
  timingdex worker doctor [--config path]
  timingdex doctor`)
	return errors.New("invalid command")
}

func runWorkerCommand() error {
	if len(os.Args) < 3 {
		return errors.New("usage: timingdex worker enroll|run|doctor")
	}
	switch os.Args[2] {
	case "enroll":
		fs := flag.NewFlagSet("worker enroll", flag.ContinueOnError)
		hub := fs.String("hub", "", "Hub HTTPS URL")
		pairing := fs.String("pairing", "", "one-time Hub pairing token")
		name := fs.String("name", hostname(), "worker name")
		fingerprint := fs.String("fingerprint", "", "Hub SHA-256 certificate fingerprint")
		root := fs.String("root", "", "Hub library root id available to this Worker")
		var mountArgs repeatedFlag
		fs.Var(&mountArgs, "mount", "local library mapping root-id=/mounted/path; repeatable")
		var providerOperations repeatedFlag
		fs.Var(&providerOperations, "provider-operation", "declared direct/proxy Provider operation: video_analysis or asr; repeatable")
		cacheDir := fs.String("cache", "", "Worker-local cache directory")
		configPath := fs.String("config", defaultWorkerConfigPath(), "worker config path")
		if err := fs.Parse(os.Args[3:]); err != nil {
			return err
		}
		if *hub == "" || *pairing == "" {
			return errors.New("--hub and --pairing are required")
		}
		if !strings.HasPrefix(strings.ToLower(*hub), "https://") {
			return errors.New("Worker enrollment requires an https Hub URL")
		}
		report, _ := media.DetectHardware(context.Background(), media.HardwareConfig{Mode: "auto", AllowFallback: true})
		capabilities := remote.WorkerCapabilities{Proxy: report.FFmpegFound, Thumbnail: report.FFmpegFound, AudioExtract: report.FFmpegFound, MaxParallelProxyJobs: 1, MaxProxyHeight: 720, SpeedClass: "standard"}
		if runtime.GOARCH == "arm64" {
			capabilities.SpeedClass = "slow"
		}
		mounts, err := parseWorkerMounts(mountArgs)
		if err != nil {
			return err
		}
		rootIDs := make(map[string]struct{}, len(mounts)+1)
		for rootID := range mounts {
			rootIDs[rootID] = struct{}{}
		}
		if *root != "" {
			rootIDs[*root] = struct{}{}
		}
		for rootID := range rootIDs {
			capabilities.LibraryRoots = append(capabilities.LibraryRoots, rootID)
		}
		for _, operation := range providerOperations {
			operation = strings.ToLower(strings.TrimSpace(operation))
			if operation != "video_analysis" && operation != "asr" {
				return fmt.Errorf("unsupported --provider-operation %q; use video_analysis or asr", operation)
			}
			capabilities.ProviderOperations = append(capabilities.ProviderOperations, operation)
		}
		sort.Strings(capabilities.LibraryRoots)
		sort.Strings(capabilities.ProviderOperations)
		registration := remote.WorkerRegistration{Name: *name, Platform: runtime.GOOS + "-" + runtime.GOARCH, Capabilities: capabilities}
		client := worker.NewClient(*hub, *fingerprint)
		enrollment, err := client.Enroll(context.Background(), *pairing, registration)
		if err != nil {
			return err
		}
		if strings.TrimSpace(*cacheDir) == "" {
			*cacheDir = filepath.Join(filepath.Dir(*configPath), "cache")
		}
		if err := worker.SaveConfig(*configPath, worker.Config{HubURL: *hub, CertificateFingerprint: *fingerprint, Token: enrollment.Token, CacheDir: *cacheDir, Mounts: mounts, Registration: registration}); err != nil {
			return err
		}
		fmt.Printf("enrolled worker %s; configuration saved to %s\n", enrollment.Worker.ID, *configPath)
		return nil
	case "run":
		fs := flag.NewFlagSet("worker run", flag.ContinueOnError)
		configPath := fs.String("config", defaultWorkerConfigPath(), "worker config path")
		if err := fs.Parse(os.Args[3:]); err != nil {
			return err
		}
		config, err := worker.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		client := worker.NewClient(config.HubURL, config.CertificateFingerprint)
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		_, plan := media.DetectHardware(ctx, media.HardwareConfig{Mode: "auto", AllowFallback: true})
		deriver := worker.NewFFmpegDeriver(plan)
		runtime := worker.NewRuntime(client, config, deriver)
		return runtime.Run(ctx, worker.RunOptions{})
	case "doctor":
		fs := flag.NewFlagSet("worker doctor", flag.ContinueOnError)
		configPath := fs.String("config", defaultWorkerConfigPath(), "worker config path")
		if err := fs.Parse(os.Args[3:]); err != nil {
			return err
		}
		config, err := worker.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		fmt.Printf("hub: %s\nplatform: %s\nworker name: %s\n", config.HubURL, config.Registration.Platform, config.Registration.Name)
		return nil
	default:
		return errors.New("usage: timingdex worker enroll|run|doctor")
	}
}

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func parseWorkerMounts(values []string) (map[string]string, error) {
	mounts := make(map[string]string, len(values))
	for _, value := range values {
		rootID, localPath, ok := strings.Cut(value, "=")
		rootID, localPath = strings.TrimSpace(rootID), strings.TrimSpace(localPath)
		if !ok || rootID == "" || localPath == "" {
			return nil, fmt.Errorf("invalid --mount %q; expected root-id=/mounted/path", value)
		}
		if previous, exists := mounts[rootID]; exists && previous != localPath {
			return nil, fmt.Errorf("worker root %q is mapped more than once", rootID)
		}
		mounts[rootID] = filepath.Clean(localPath)
	}
	return mounts, nil
}

func defaultWorkerConfigPath() string {
	if value := os.Getenv("TIMINGDEX_WORKER_CONFIG"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "timingdex-worker.json"
	}
	return filepath.Join(home, ".timingdex", "worker.json")
}
func hostname() string {
	value, err := os.Hostname()
	if err != nil || value == "" {
		return "timingdex-worker"
	}
	return value
}
