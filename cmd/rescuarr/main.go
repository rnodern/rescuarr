package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/config"
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
		fmt.Println("usage: rescuarr [serve|doctor|version] [--listen host:port] [--data-dir path] [--base-path path]")
		return nil
	}
	if command == "version" {
		if len(args) != 0 {
			return errors.New("version takes no arguments")
		}
		fmt.Printf("rescuarr %s (%s)\n", version, commit)
		return nil
	}
	if command != "serve" && command != "doctor" {
		return fmt.Errorf("unknown command %q", command)
	}
	cfg, err := config.Parse(args, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if command == "doctor" {
		fmt.Printf("configuration valid, listen source %s, data directory source %s\n",
			cfg.Sources["RESCUARR_LISTEN"], cfg.Sources["RESCUARR_DATA_DIR"])
		return nil
	}
	return serve(ctx, cfg)
}

func serve(ctx context.Context, cfg config.Config) error {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("open listener: %w", err)
	}
	server := &http.Server{
		Handler:           app.Handler(cfg.BasePath, nil),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	finished := make(chan error, 1)
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
		return nil
	}
}
