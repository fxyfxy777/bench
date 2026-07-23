package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/app"
	"github.com/yzx/rl-router/internal/config"
	"github.com/yzx/rl-router/pkg/logger"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runWithWriters(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run contains the core application logic, extracted from main() for testability.
// It parses flags from args, loads config, builds the app, and blocks until ctx is done.
func run(ctx context.Context, args []string, stderr io.Writer) error {
	return runWithWriters(ctx, args, stderr, stderr)
}

func runWithWriters(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	fs.SetOutput(stderr)

	ptrs := &config.FlagPtrs{
		Mode:              fs.String("mode", "hybrid", "Run mode: gateway, scheduler, hybrid"),
		ListenAddr:        fs.String("listen", ":8080", "HTTP listen address (gateway)"),
		AdminAddr:         fs.String("admin-listen", ":8081", "Admin HTTP listen address (metrics, healthz, pprof; empty = share main port)"),
		GRPCAddr:          fs.String("grpc", ":9090", "gRPC listen address (scheduler)"),
		SchedulerAddr:     fs.String("scheduler-addr", "", "Remote scheduler gRPC address (gateway mode)"),
		Policy:            fs.String("policy", "min_load", "Scheduling policy: round_robin, min_load, min_request, session_aware"),
		LogLevel:          fs.String("log-level", "info", "Log level: debug, info, warn, error"),
		LogFormat:         fs.String("log-format", "console", "Log format: json, console"),
		LogDir:            fs.String("log-dir", "", "Directory for log files; empty = stderr-only dev mode"),
		AdvertiseAddr:     fs.String("advertise-addr", "", "Externally reachable address (default: same as --listen). Also used as gateway identity"),
		HeartbeatInterval: fs.Duration("heartbeat-interval", 5*time.Second, "Heartbeat interval for gateway registration"),
		HeartbeatTimeout:  fs.Duration("heartbeat-timeout", 15*time.Second, "Heartbeat timeout for scheduler expiry"),
		ShutdownGrace:     fs.Duration("shutdown-grace", 15*time.Second, "Max time to wait for graceful shutdown"),
		EnableH2C:         fs.Bool("enable-h2c", false, "Enable h2c (HTTP/2 cleartext) on frontend listener"),
		BackendH2C:        fs.Bool("backend-h2c", false, "Connect to backends via h2c"),
		Splitwise:         fs.Bool("splitwise", false, "Enable PD separation (prefill/decode disaggregation)"),
	}
	configFile := fs.String("config", "", "Path to YAML config file")
	showVersion := fs.Bool("version", false, "Print build version information and exit")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		return printVersion(stdout)
	}

	cfg := config.Defaults()

	if *configFile != "" {
		if err := cfg.LoadFromFile(*configFile); err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	config.ApplyCLIOverrides(cfg, fs, ptrs)

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	bundle, err := logger.NewBundle(logger.Config{
		Level:            cfg.Log.Level,
		AccessLevel:      cfg.Log.AccessLevel,
		Format:           cfg.Log.Format,
		SlowRequest:      cfg.Log.SlowRequest.Duration,
		LogDir:           cfg.Log.LogDir,
		MaxSizeMB:        cfg.Log.MaxSizeMB,
		MaxBackups:       cfg.Log.MaxBackups,
		MaxAgeDays:       cfg.Log.MaxAgeDays,
		Compress:         cfg.Log.Compress,
		SampleInitial:    cfg.Log.SampleInitial,
		SampleThereafter: cfg.Log.SampleThereafter,
		AuditMaxSizeMB:   cfg.Log.Audit.MaxSizeMB,
		AuditMaxBackups:  cfg.Log.Audit.MaxBackups,
		AuditMaxAgeDays:  cfg.Log.Audit.MaxAgeDays,
		AuditCompress:    cfg.Log.Audit.Compress,
	})
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	log := bundle.Root
	log.Info("starting rl-router",
		zap.String("version", Version),
		zap.String("commit", Commit),
		zap.String("build_date", BuildDate))

	application, err := app.New(cfg, log, app.WithBundle(bundle))
	if err != nil {
		return fmt.Errorf("create application: %w", err)
	}

	// Run app.Start in a goroutine so we can enforce the shutdown deadline.
	errCh := make(chan error, 1)
	go func() {
		errCh <- application.Start(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Error("application exited with error", zap.Error(err))
			return err
		}
		log.Info("application exited cleanly")
		return nil
	case <-ctx.Done():
		log.Info("shutdown signal received, waiting for graceful shutdown",
			zap.Duration("grace_period", cfg.ShutdownGrace.Duration))

		select {
		case err := <-errCh:
			if err != nil {
				log.Error("application exited with error during shutdown", zap.Error(err))
				return err
			}
			log.Info("graceful shutdown completed")
			return nil
		case <-time.After(cfg.ShutdownGrace.Duration):
			return fmt.Errorf("graceful shutdown timed out after %v", cfg.ShutdownGrace.Duration)
		}
	}
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "infer-router version=%s commit=%s build_date=%s go=%s platform=%s/%s\n",
		Version,
		Commit,
		BuildDate,
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
	return err
}
