// Command lazyloader is the entry point.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/egomarker/docker-lazyloader/internal/config"
	"github.com/egomarker/docker-lazyloader/internal/docker"
	"github.com/egomarker/docker-lazyloader/internal/lifecycle"
	"github.com/egomarker/docker-lazyloader/internal/server"
)

const version = "0.1.1"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	cfgPath := flag.String("config", envOr("LAZYLOADER_CONFIG", defaultConfigPath()), "path to config file")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("config load failed", "err", err)
		os.Exit(1)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("lazyloader starting", "config", *cfgPath, "services", len(cfg.Services))

	mgr := lifecycle.NewManager(log)
	for _, sc := range cfg.Services {
		comp := docker.Compose{
			Bin:     cfg.DockerBin,
			Dir:     sc.ComposeDir,
			Project: sc.ComposeProject,
		}
		svc := lifecycle.NewService(lifecycle.ServiceConfig{
			Logger:             log.With("host", sc.Host),
			Compose:            comp,
			HealthURL:          sc.Health,
			HealthExpectStatus: sc.HealthExpectStatus,
			HealthExpectBody:   sc.HealthExpectBody,
			HealthTimeout:      sc.HealthTimeout.Std(),
			StartTimeout:       sc.StartTimeout.Std(),
			IdleTimeout:        sc.IdleTimeout.Std(),
			MinUptime:          sc.MinUptime.Std(),
			Upstream:           sc.Upstream,
		})
		mgr.Add(sc.Host, svc)
		log.Info("registered service", "host", sc.Host, "upstream", sc.Upstream, "compose_dir", sc.ComposeDir)
	}

	// Set initial state from reality (is it already up?).
	mgr.Reconcile()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.IdleLoop(rootCtx, cfg.PollInterval.Std())

	srv := server.New(log, mgr)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server stopped", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
	cancel()
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "lazyloader", "lazyloader.yaml")
	}
	return "lazyloader.yaml"
}
