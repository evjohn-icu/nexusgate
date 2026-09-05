package main

import (
	"context"
	"encoding/json"
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

	"github.com/evjohn-icu/nexusgate/internal/api"
	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/buildinfo"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/hubauth"
	"github.com/evjohn-icu/nexusgate/internal/hubtls"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/providers"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	sqliterepo "github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
	"github.com/evjohn-icu/nexusgate/internal/secretstore"
	"github.com/evjohn-icu/nexusgate/internal/webdavspace"
	"github.com/evjohn-icu/nexusgate/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("nexusgate stopped", "error", err)
		os.Exit(1)
	}
}

// setupLogging installs the process-wide slog handler from environment
// variables. NEXUSGATE_LOG_FORMAT selects text (default) or JSON output;
// NEXUSGATE_LOG_LEVEL selects debug|info|warn|error (default info). With no
// variables set the behaviour is byte-identical to the default slog output, so
// existing deployments see no change until they opt in. Invalid values fall
// back to the default and log a warning rather than aborting startup.
func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXUSGATE_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "", "info":
		level = slog.LevelInfo
	default:
		slog.Warn("ignoring invalid NEXUSGATE_LOG_LEVEL; using info", "level", truncateEnv("NEXUSGATE_LOG_LEVEL", 20))
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXUSGATE_LOG_FORMAT"))) {
	case "", "text":
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	default:
		slog.Warn("ignoring invalid NEXUSGATE_LOG_FORMAT; using text", "format", truncateEnv("NEXUSGATE_LOG_FORMAT", 20))
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

	if os.Args[1] != "worker" && os.Args[1] != "doctor" {
		// Fail fast on a selected provider whose key never resolved (see
		// providers.ValidateProviderConfig). config.Load already resolved
		// api_key_env into api_key, so this catches an enabled ASR/vision/
		// repurpose route with an unset key env before serve claims to be
		// healthy. doctor is exempt so a broken legacy providers.* config can
		// still be diagnosed (the report carries the reason instead); the
		// worker pulls provider work (and keys) from the Hub and never runs
		// these providers itself, so it too is exempt.
		if err := providers.ValidateProviderConfig(cfg.Providers); err != nil {
			return fmt.Errorf("invalid provider config: %w", err)
		}
	}

	if os.Args[1] == "worker" {
		return runWorkerCommand(cfg)
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
		if err := config.ValidateContainerAdminAuth(detectContainer(), cfg); err != nil {
			return err
		}
		switch cfg.HubSecurity.AdminAuth {
		case "trusted_network":
			fmt.Fprintf(os.Stderr, "WARNING: hub_security.admin_auth=trusted_network — 管理写入不需要口令\n")
		case "off":
			fmt.Fprintf(os.Stderr, "WARNING: hub_security.admin_auth=off — 管理写入不需要口令，包括 provider 密钥和付费流水线运行\n")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		// Register this process in the executor registry BEFORE the startup
		// sweep so HealOnStartup reclaims only its dead predecessor's jobs,
		// never this process's (it has none yet, but the row doubles as the
		// heartbeat anchor for every pass the supervisor runs from here).
		executorStop, err := service.StartExecutor(ctx)
		if err != nil {
			return fmt.Errorf("register pipeline executor: %w", err)
		}
		defer executorStop()
		// A previous Hub crash can leave jobs at state='running' with dead
		// leases; release them before anything can be looking at the queue, or
		// they sit on /progress forever while nobody runs the pipeline. The
		// executor registry lets this reclaim a killed process's still-valid
		// local leases immediately instead of waiting out the lease TTL.
		if err := service.HealOnStartup(ctx); err != nil {
			slog.Warn("self-heal sweep failed; stale running jobs will be reclaimed on the next lease attempt", "error", err)
		}
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
		service.SetPipelineContext(ctx)
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
		setupWebDAVDelivery(service, server, repo, cfg.DataDir)
		serveErr := server.Run(ctx)
		// A listen failure does not cancel the signal context. Cancel it here as
		// well so background Pipeline work cannot outlive the failed server.
		stop()
		// Joined, not abandoned: the supervisor may be mid-pass, and the point
		// of running the pipeline inline in it is that this wait is what makes
		// "the process exited" mean "no job and no Provider call is still
		// running".
		stopSupervisor()
		<-supervisorDone
		service.WaitPipeline()
		return serveErr

	case "root":
		if len(os.Args) < 3 {
			return errors.New("usage: nexusgate root add|list|scan ...")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return runRootCommand(ctx, service, os.Args[2:])

	case "pipeline":
		if len(os.Args) < 3 {
			return errors.New("usage: nexusgate pipeline run|retry-failed")
		}
		switch os.Args[2] {
		case "run":
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			executorStop, err := service.StartExecutor(ctx)
			if err != nil {
				return fmt.Errorf("register pipeline executor: %w", err)
			}
			defer executorStop()
			// Same startup sweep as `serve`: release stale running jobs from
			// a crashed process before the pipeline can lease or reclaim.
			if err := service.HealOnStartup(ctx); err != nil {
				slog.Warn("self-heal sweep failed; stale running jobs will be reclaimed on the next lease attempt", "error", err)
			}
			return service.RunPipeline(ctx)
		case "retry-failed":
			requeued, err := service.RequeueFailedJobs(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("requeued %d failed job(s); run `nexusgate pipeline run` to process them\n", requeued)
			return nil
		default:
			return errors.New("usage: nexusgate pipeline run|retry-failed")
		}

	case "reanalyze":
		return runReanalyzeCommand(service, os.Args[2:])

	case "search":
		if len(os.Args) < 3 {
			return errors.New("usage: nexusgate search rebuild|rebuild-embeddings")
		}
		switch os.Args[2] {
		case "rebuild":
			fmt.Println("rebuilding asset search index...")
			rebuilt, failures, err := repo.RebuildAllSearch(context.Background())
			if err != nil {
				return err
			}
			for _, failure := range failures {
				fmt.Printf("failed: %s\n", failure)
			}
			fmt.Printf("rebuilt %d asset search index(es); %d failure(s)\n", rebuilt, len(failures))
			if len(failures) > 0 {
				return errors.New("one or more asset search indexes failed to rebuild")
			}
			return nil
		case "rebuild-embeddings":
			rebuilt, err := service.RebuildShotTextEmbeddings(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("rebuilt %d shot text embedding(s); run `nexusgate pipeline run` if jobs are queued\n", rebuilt)
			return nil
		default:
			return errors.New("usage: nexusgate search rebuild|rebuild-embeddings")
		}

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		jsonOut := fs.Bool("json", false, "print the report as JSON")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		if !*jsonOut {
			fmt.Printf("database: %s\n", cfg.DatabasePath)
			fmt.Printf("cache:    %s\n", cfg.CacheDir)
			fmt.Printf("listen:   %s\n", cfg.ListenAddress)
			return service.Doctor(context.Background(), os.Stdout)
		}
		report, err := service.DoctorReport(context.Background())
		if err != nil {
			return fmt.Errorf("doctor report: %w", err)
		}
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode doctor report: %w", err)
		}
		fmt.Println(string(encoded))
		// The JSON path reports integrity as a field, but the process exit
		// must agree with the text path: a broken database is a failed
		// diagnostic, not a healthy one. This mirrors Service.Doctor's
		// non-zero exit on a failed integrity check.
		if !report.DB.IntegrityOK {
			return fmt.Errorf("doctor: database integrity check failed")
		}
		return nil

	case "secrets":
		return runSecretsCommand(cfg)

	case "cache":
		if len(os.Args) < 3 {
			return errors.New("usage: nexusgate cache inspect|gc|verify")
		}
		return runCacheCommand(context.Background(), repo, cfg, os.Args[2:])

	case "support":
		if len(os.Args) < 3 || os.Args[2] != "bundle" {
			return errors.New("usage: nexusgate support bundle [-out path]")
		}
		return runSupportBundleCommand(service, cfg, os.Args[3:])

	default:
		return usage()
	}
}

// detectContainer reports whether this process is running inside a container.
// It uses the cheap, dependency-free checks: the presence of /.dockerenv, or a
// cgroup hierarchy naming docker/containerd. The result only feeds
// config.ValidateContainerAdminAuth, which decides whether the admin-auth mode
// is safe for the detected environment.
func detectContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(data)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}

// runReanalyzeCommand forces the analyze stage to run again for selected
// assets — after a prompt/schema/validator or provider change, canonical
// analysis must be refreshable without deleting the database. Old model runs
// stay immutable and auditable; the new run switches the canonical analysis
// and rebuilds search when it lands.
func runReanalyzeCommand(service *app.Service, args []string) error {
	flags := flag.NewFlagSet("reanalyze", flag.ContinueOnError)
	assetID := flags.String("asset", "", "reanalyze a single asset by id")
	rootID := flags.String("root", "", "reanalyze every asset under a library root")
	all := flags.Bool("all", false, "reanalyze every asset in the library")
	reason := flags.String("reason", "", "why this reanalysis is happening (recorded for audit)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ids, err := service.ResolveReanalysisAssets(context.Background(), app.ReanalysisSelector{AssetID: *assetID, RootID: *rootID, All: *all, Reason: *reason})
	if err != nil {
		return err
	}
	enqueued, err := service.ReanalyzeAssets(context.Background(), ids, *reason)
	if err != nil {
		return err
	}
	effective := *reason
	if effective == "" {
		effective = "reanalysis-v1"
	}
	fmt.Printf("enqueued reanalysis for %d asset(s) (reason: %s); run `nexusgate pipeline run` to process them\n", enqueued, effective)
	return nil
}

// runSecretsCommand exposes secretstore operations on the CLI. Currently the
// only subcommand is `rekey`, which rotates the data-encryption key and
// re-encrypts every stored provider secret (see secretstore.Store.Rekey). It
// needs the Hub administrator token the same way the serving process derives
// it, so an operator does not have to copy the key material out of the store
// or the running process.
func runSecretsCommand(cfg config.Config) error {
	if len(os.Args) < 3 {
		return errors.New("usage: nexusgate secrets rekey")
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
		return errors.New("usage: nexusgate secrets rekey")
	}
}

// setupWebDAVDelivery wires the on-demand WebDAV footage-delivery feature
// into the service and server: a repository-backed account store (bcrypt
// hashes), the space manager whose linker resolves assets through the
// service, and the /spaces/ route on the API server. The feature is always
// compiled in; the admin endpoints gate creation of accounts and spaces.
func setupWebDAVDelivery(service *app.Service, server *api.Server, repo *sqliterepo.Repository, dataDir string) {
	accounts := sqliterepo.WebDAVAccountStore{Repo: repo}
	linker := app.WebDAVLinker{Service: service}
	manager := webdavspace.NewManager(linker, accounts, dataDir)
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
			return "", "", errors.New("NEXUSGATE_TLS_CERT_FILE and NEXUSGATE_TLS_KEY_FILE are required for TLS files mode")
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
			return errors.New("usage: nexusgate root add <path>")
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
			return errors.New("usage: nexusgate root scan <root-id>")
		}
		result, err := service.ScanLibraryRoot(ctx, args[1])
		if err != nil {
			// Reconciliation may fail after this scan has queued changed assets.
			// Keep the scan error, but do not leave the queue idle.
			if _, pipelineErr := service.TryRunPipeline(ctx); pipelineErr != nil {
				return fmt.Errorf("scan: %w; run pipeline after scan: %v", err, pipelineErr)
			}
			return err
		}
		fmt.Printf("discovered=%d linked=%d missing=%d skipped=%d errors=%d supported=%s\n",
			result.Discovered, result.Linked, result.Missing, result.SkippedFiles, len(result.Errors),
			strings.Join(result.SupportedExtensions, ","))
		if result.SkippedFiles > 0 {
			exts := strings.Join(result.SkippedExtensions, ", ")
			if result.SkippedOther > 0 {
				exts += fmt.Sprintf(" (+%d more types)", result.SkippedOther)
			}
			fmt.Printf("skipped: %d non-video file(s) (%s)\n", result.SkippedFiles, exts)
		}
		if result.Discovered == 0 && result.SkippedFiles > 0 {
			fmt.Printf("warning: no footage discovered in this root; every regular file was skipped (supported formats: %s)\n",
				strings.Join(result.SupportedExtensions, ", "))
		}
		for _, scanErr := range result.Errors {
			fmt.Printf("warning: %s\n", scanErr)
		}
		ran, err := service.TryRunPipeline(ctx)
		if err != nil {
			return fmt.Errorf("run pipeline after scan: %w", err)
		}
		if ran {
			fmt.Println("pipeline=started")
		} else {
			fmt.Println("pipeline=already_running")
		}
		return nil
	default:
		return errors.New("usage: nexusgate root add|list|scan ...")
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `NexusGate Footage

Usage:
  nexusgate serve [-addr 127.0.0.1:8787]
  nexusgate root add <path>
  nexusgate root list
  nexusgate root scan <root-id>
  nexusgate pipeline run
  nexusgate pipeline retry-failed
  nexusgate reanalyze [-asset <asset-id> | -root <root-id> | -all] [-reason <text>]
  nexusgate search rebuild
  nexusgate search rebuild-embeddings
  nexusgate cache inspect
  nexusgate cache gc
  nexusgate cache verify
  nexusgate cache repair-derived
  nexusgate doctor [-json]
  nexusgate secrets rekey
  nexusgate support bundle [-out path]
  nexusgate worker enroll [--root <path>] [--cache <path>] [--config <path>] --hub https://nas:8787 --fingerprint <sha256> --pairing <token> [--name worker] [--mount root-id=/mounted/path] [--provider-operation video_analysis]
  nexusgate worker run [--config path] [--tray]
  nexusgate worker doctor [--config path]
  nexusgate worker revoke <worker-id>`)
	return errors.New("invalid command")
}

func runWorkerCommand(cfg config.Config) error {
	if len(os.Args) < 3 {
		return errors.New("usage: nexusgate worker enroll|run|doctor|revoke")
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
		if !worker.ValidFingerprint(*fingerprint) {
			return errors.New("--fingerprint is required and must be a SHA-256 hex fingerprint")
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
		registration := remote.WorkerRegistration{Name: *name, Platform: runtime.GOOS + "-" + runtime.GOARCH, Version: buildinfo.VersionString(), Capabilities: capabilities}
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
	case "revoke":
		// Revoking a Worker is a Hub administration action, so this runs
		// against the Hub's own database with the same in-process authority
		// every other Hub CLI command (pipeline run, root add, reanalyze,
		// rekey) uses: app.NewService resolves the Hub administrator token
		// from the standard config/env (NEXUSGATE_HUB_ADMIN_TOKEN or the
		// mode-0600 DATA_DIR/admin-token file), which is what authorizes this
		// write. Running it on a Worker node without the Hub database is a
		// clear "open database" error.
		if len(os.Args) < 4 {
			return errors.New("usage: nexusgate worker revoke <worker-id>")
		}
		workerID := strings.TrimSpace(os.Args[3])
		if workerID == "" {
			return errors.New("usage: nexusgate worker revoke <worker-id>")
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
		if err := service.RevokeWorker(context.Background(), workerID); err != nil {
			return fmt.Errorf("revoke worker %s: %w", workerID, err)
		}
		fmt.Printf("revoked worker %s\n", workerID)
		return nil
	default:
		return errors.New("usage: nexusgate worker enroll|run|doctor|revoke")
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
		name = "NexusGate Worker"
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
	if value := os.Getenv("NEXUSGATE_WORKER_CONFIG"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "nexusgate-worker.json"
	}
	return filepath.Join(home, ".nexusgate", "worker.json")
}
func truncateEnv(name string, maxLen int) string {
	v := os.Getenv(name)
	if len(v) <= maxLen {
		return v
	}
	return v[:maxLen] + "..."
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil || value == "" {
		return "nexusgate-worker"
	}
	return value
}
