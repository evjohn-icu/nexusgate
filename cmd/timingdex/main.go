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
	"time"

	"github.com/evjohn-icu/timingdex/internal/api"
	"github.com/evjohn-icu/timingdex/internal/app"
	"github.com/evjohn-icu/timingdex/internal/config"
	"github.com/evjohn-icu/timingdex/internal/hubauth"
	"github.com/evjohn-icu/timingdex/internal/hubtls"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/remote"
	sqliterepo "github.com/evjohn-icu/timingdex/internal/repository/sqlite"
	"github.com/evjohn-icu/timingdex/internal/secretstore"
	"github.com/evjohn-icu/timingdex/internal/webdavspace"
	"github.com/evjohn-icu/timingdex/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("timingdex stopped", "error", err)
		os.Exit(1)
	}
}

// setupLogging installs the process-wide slog handler from environment
// variables. TIMINGDEX_LOG_FORMAT selects text (default) or JSON output;
// TIMINGDEX_LOG_LEVEL selects debug|info|warn|error (default info). With no
// variables set the behaviour is byte-identical to the default slog output, so
// existing deployments see no change until they opt in. Invalid values fall
// back to the default and log a warning rather than aborting startup.
func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TIMINGDEX_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "", "info":
		level = slog.LevelInfo
	default:
		slog.Warn("ignoring invalid TIMINGDEX_LOG_LEVEL; using info", "level", os.Getenv("TIMINGDEX_LOG_LEVEL"))
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TIMINGDEX_LOG_FORMAT"))) {
	case "", "text":
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	default:
		slog.Warn("ignoring invalid TIMINGDEX_LOG_FORMAT; using text", "format", os.Getenv("TIMINGDEX_LOG_FORMAT"))
	}
	slog.SetDefault(slog.New(handler))
}

func run() error {
	setupLogging()
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
		// The supervisor gets its own cancel rather than only the signal
		// context: a listen failure returns from Run without the signal ever
		// firing, and `serve` must not then block forever joining a loop that
		// nothing will ever stop.
		supervisorCtx, stopSupervisor := context.WithCancel(ctx)
		defer stopSupervisor()
		supervisorDone := make(chan struct{})
		go func() {
			defer close(supervisorDone)
			if err := service.RunLibrarySupervisor(supervisorCtx); err != nil {
				slog.Error("library supervisor stopped", "error", err)
			}
		}()
		if status := service.LibrarySupervisorStatus(); status.Enabled {
			fmt.Printf("Library supervisor: rescanning every root every %s\n", (time.Duration(status.IntervalSeconds) * time.Second).String())
		}
		server := api.NewTLSServer(*addr, service, certificate, key)
		setupWebDAVDelivery(service, server, repo)
		serveErr := server.Run(ctx)
		// Joined, not abandoned: the supervisor may be mid-pass, and the point
		// of running the pipeline inline in it is that this wait is what makes
		// "the process exited" mean "no job and no Provider call is still
		// running".
		stopSupervisor()
		<-supervisorDone
		return serveErr

	case "root":
		if len(os.Args) < 3 {
			return errors.New("usage: timingdex root add|list|scan ...")
		}
		return runRootCommand(context.Background(), service, os.Args[2:])

	case "pipeline":
		if len(os.Args) < 3 {
			return errors.New("usage: timingdex pipeline run|retry-failed")
		}
		switch os.Args[2] {
		case "run":
			return service.RunPipeline(context.Background())
		case "retry-failed":
			requeued, err := service.RequeueFailedJobs(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("requeued %d failed job(s); run `timingdex pipeline run` to process them\n", requeued)
			return nil
		default:
			return errors.New("usage: timingdex pipeline run|retry-failed")
		}

	case "doctor":
		fmt.Printf("database: %s\n", cfg.DatabasePath)
		fmt.Printf("cache:    %s\n", cfg.CacheDir)
		fmt.Printf("listen:   %s\n", cfg.ListenAddress)
		return service.Doctor(context.Background(), os.Stdout)

	case "secrets":
		return runSecretsCommand(cfg)

	default:
		return usage()
	}
}

// runSecretsCommand exposes secretstore operations on the CLI. Currently the
// only subcommand is `rekey`, which rotates the data-encryption key and
// re-encrypts every stored provider secret (see secretstore.Store.Rekey). It
// needs the Hub administrator token the same way the serving process derives
// it, so an operator does not have to copy the key material out of the store
// or the running process.
func runSecretsCommand(cfg config.Config) error {
	if len(os.Args) < 3 {
		return errors.New("usage: timingdex secrets rekey")
	}
	switch os.Args[2] {
	case "rekey":
		adminToken, err := hubauth.EnsureAdminToken(cfg.DataDir, cfg.HubSecurity.AdminToken)
		if err != nil {
			return fmt.Errorf("resolve Hub administrator token: %w", err)
		}
		store, err := secretstore.Open(cfg.DataDir, adminToken)
		if err != nil {
			return fmt.Errorf("open secret store: %w", err)
		}
		if err := store.Rekey(); err != nil {
			return fmt.Errorf("rekey secret store: %w", err)
		}
		fmt.Printf("rekeyed provider secret store; previous key backed up to %s\n", filepath.Join(cfg.DataDir, "provider-secrets", "store.key.pre-rekey"))
		return nil
	default:
		return errors.New("usage: timingdex secrets rekey")
	}
}

// setupWebDAVDelivery wires the on-demand WebDAV footage-delivery feature
// into the service and server: a repository-backed account store (bcrypt
// hashes), the space manager whose linker resolves assets through the
// service, and the /spaces/ route on the API server. The feature is always
// compiled in; the admin endpoints gate creation of accounts and spaces.
func setupWebDAVDelivery(service *app.Service, server *api.Server, repo *sqliterepo.Repository) {
	accounts := sqliterepo.WebDAVAccountStore{Repo: repo}
	linker := app.WebDAVLinker{Service: service}
	manager := webdavspace.NewManager(linker, accounts)
	service.SetWebDAVSpaceManager(manager, accounts)
	server.SetWebDAVSpaceManager(manager)
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
  timingdex pipeline retry-failed
  timingdex worker enroll --hub https://nas:8787 --pairing <token> [--name worker] [--mount root-id=/mounted/path] [--provider-operation video_analysis]
  timingdex worker run [--config path] [--tray]
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
		capabilities := remote.WorkerCapabilities{Proxy: report.FFmpegFound, Thumbnail: report.FFmpegFound, AudioExtract: report.FFmpegFound, MaxParallelProxyJobs: 1, MaxProxyHeight: 720, SpeedClass: "standard", Hardware: report.SelectedBackends()}
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
		tray := fs.Bool("tray", false, "show a notification-area icon with a settings link and a quit item (Windows)")
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
		// Detected once, here, at process start — not per heartbeat. A device
		// becoming available (e.g. a NAS template edit adding /dev/dri) needs a
		// container recreate, which restarts this process anyway, so a
		// recurring re-probe would only spend a throwaway GPU encode every
		// heartbeat interval for information that cannot change out from under
		// a running process.
		hardwareReport, plan := media.DetectHardware(ctx, media.HardwareConfig{Mode: "auto", AllowFallback: true})
		capabilities := worker.MergeDetectedCapabilities(config.Registration.Capabilities, hardwareReport)
		deriver := worker.NewFFmpegDeriver(plan)
		runtime := worker.NewRuntime(client, config, deriver, capabilities)
		if !*tray {
			return runtime.Run(ctx, worker.RunOptions{})
		}
		return runWorkerWithTray(ctx, stop, runtime, *configPath, config)
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
		// The Worker, not the Hub, is the process that actually owns the GPU
		// and runs the encode, so this is the diagnosis an operator debugging
		// a broken accelerator on a Worker box needs to see — not just the
		// enrollment metadata above.
		report, _ := media.DetectHardware(context.Background(), media.HardwareConfig{Mode: "auto", AllowFallback: true})
		media.FormatHardwareReport(os.Stdout, report)
		return nil
	default:
		return errors.New("usage: timingdex worker enroll|run|doctor")
	}
}

// runWorkerWithTray runs the lease loop in the background while the tray owns the
// main thread, which Win32 requires: a message pump is bound to the thread that
// created its window.
//
// The tray is the reason the settings server exists at all — without a window
// there is nowhere to click "settings" — so the two start and stop together. The
// settings URL carries an access token and is deliberately not logged; the tray
// hands it to the browser directly.
func runWorkerWithTray(ctx context.Context, stop context.CancelFunc, runtime *worker.Runtime, configPath string, config worker.Config) error {
	admin, err := worker.NewLocalAdmin(configPath)
	if err != nil {
		return err
	}
	defer admin.Close()

	adminCtx, stopAdmin := context.WithCancel(ctx)
	defer stopAdmin()
	go func() {
		if err := admin.Serve(adminCtx); err != nil {
			slog.Error("worker settings server stopped", "error", err)
		}
	}()

	workerErr := make(chan error, 1)
	go func() { workerErr <- runtime.Run(ctx, worker.RunOptions{}) }()

	name := strings.TrimSpace(config.Registration.Name)
	if name == "" {
		name = "Timingdex Worker"
	}
	trayErr := worker.ShowTray(ctx, worker.TrayOptions{
		Tooltip:     name + " → " + config.HubURL,
		SettingsURL: admin.URL(),
		// Quitting from the menu has to stop the lease loop rather than kill the
		// process, so a job in flight is reported back instead of silently
		// timing out its lease on the Hub.
		OnQuit: stop,
	})
	if trayErr != nil {
		// Without a tray there is no way to quit and no way to reach settings, so
		// falling back to a headless run would strand the operator. Stop instead
		// and say why.
		stop()
		<-workerErr
		return trayErr
	}
	return <-workerErr
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
