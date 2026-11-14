package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/store"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command == "help" || len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: rescuarr [serve|doctor|version|backup|restore|owner-recovery] [options]")
		fmt.Println("  --listen host:port  --data-dir path  --base-path /prefix")
		fmt.Println("  --public-url URL    --trusted-proxies CIDR[,CIDR]")
		fmt.Println("  backup --output file   restore --input file")
		return nil
	}
	if command == "version" {
		if len(args) != 0 {
			return errors.New("version takes no arguments")
		}
		fmt.Printf("rescuarr %s (%s)\n", version, commit)
		return nil
	}
	switch command {
	case "serve", "doctor", "backup", "restore", "owner-recovery":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	cfg, err := config.Parse(args, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if (command == "backup") != (cfg.Output != "") || (command == "restore") != (cfg.Input != "") {
		return errors.New("backup requires --output and restore requires --input")
	}
	if command == "restore" {
		previous, err := store.Restore(ctx, cfg.DataDir, cfg.Input, time.Now())
		if previous != "" {
			fmt.Printf("Previous local database saved to %s\n", previous)
		}
		if err == nil {
			fmt.Println("Restored in Observe mode with reconciliation required.")
		}
		return err
	}
	if command == "doctor" {
		return doctor(ctx, cfg)
	}

	local, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer local.Close()
	if command == "backup" {
		info, err := local.Backup(ctx, cfg.Output)
		if err != nil {
			return err
		}
		fmt.Printf("Verified SQLite backup with schema revision %04d\n", info.SchemaRevision)
		return nil
	}

	auth := access.New(local, cfg.DataDir, app.SystemClock{}.Now)
	if command == "owner-recovery" {
		if err := auth.IssueRecovery(ctx); err != nil {
			return err
		}
		fmt.Println("Owner sessions revoked. Retrieve the private setup-token file and submit it on the setup page within 15 minutes.")
		return nil
	}
	if err := auth.PrepareSetup(ctx); err != nil {
		return fmt.Errorf("prepare local owner setup: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return serve(ctx, cfg, local, auth, logger)
}

func doctor(ctx context.Context, cfg config.Config) error {
	path := store.DatabasePath(cfg.DataDir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Configuration valid. Local database will be initialised on first start.")
		return nil
	} else if err != nil {
		return err
	}
	info, err := store.InspectBackup(ctx, path)
	if err != nil {
		return fmt.Errorf("check local database: %w", err)
	}
	fmt.Printf("Configuration valid. Local database integrity verified at schema revision %04d.\n", info.SchemaRevision)
	return nil
}

func serve(ctx context.Context, cfg config.Config, local *store.Store, auth *access.Auth, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("open listener: %w", err)
	}
	server := &http.Server{
		Handler:           browserHandler(cfg, auth, local),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	workers, stopWorkers := context.WithCancel(ctx)
	workerStopped := make(chan struct{})
	go func() {
		defer close(workerStopped)
		maintain(workers, local, auth, logger)
	}()
	defer func() {
		stopWorkers()
		<-workerStopped
	}()
	finished := make(chan error, 1)
	logger.Info("service_started", "component", "http", "version", version)
	go func() { finished <- server.Serve(listener) }()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		logger.Info("service_stopped", "component", "http")
		return nil
	}
}

func maintain(ctx context.Context, local *store.Store, auth *access.Auth, logger *slog.Logger) {
	const cadence = 30 * time.Second
	ticks := time.NewTicker(cadence)
	defer ticks.Stop()
	var degraded bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks.C:
			probe, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := local.Probe(probe)
			cancel()
			switch {
			case err != nil && !degraded:
				logger.Warn("local_store_degraded", "component", "store")
			case err == nil && degraded:
				logger.Info("local_store_recovered", "component", "store")
			}
			degraded = err != nil
			if !degraded {
				work, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := auth.PrepareSetup(work); err != nil && !errors.Is(err, context.Canceled) {
					logger.Warn("local_setup_maintenance_failed", "component", "access")
				}
				_ = auth.PruneSessions(work)
				cancel()
			}
		}
	}
}
