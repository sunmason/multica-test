// Command server runs the golang-sandbox web platform: static frontend +
// sandboxed Go execution API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/logctx"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/runner"
	"golang-sandbox/internal/sched"
	"golang-sandbox/internal/server"
)

func main() {
	configPath := flag.String("config", os.Getenv("GSB_CONFIG"), "path to JSON config file (optional)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	ctx := logctx.WithLogger(logctx.WithTraceID(context.Background(), logctx.NewID()), log)
	ctx, cancelLifecycle := context.WithCancel(ctx)
	defer cancelLifecycle()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logctx.Logger(ctx).ErrorContext(ctx, "config load failed", "err", err)
		os.Exit(1)
	}

	reg := metrics.New()
	r, err := runner.Select(ctx, cfg)
	if err != nil {
		logctx.Logger(ctx).ErrorContext(ctx, "no usable runner", "err", err)
		os.Exit(1)
	}
	cfg.Runner = r.Name() // health/meta must report the ACTIVE backend, not the requested one
	logctx.Logger(ctx).InfoContext(ctx, "runner selected", "runner", r.Name(), "image", cfg.DockerImage)

	sch := sched.New(cfg, r, reg, log)
	api := server.New(cfg, sch, reg, log)
	sch.Start(ctx)
	api.StartGoVersionRefresh(ctx, r)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-stop
		logctx.Logger(ctx).InfoContext(ctx, "shutting down")
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logctx.Logger(ctx).WarnContext(ctx, "HTTP shutdown failed", "err", err)
		}
		// HTTP draining must not consume the sandbox teardown budget.
		sch.Shutdown(context.WithoutCancel(ctx))
		cancelLifecycle()
	}()

	logctx.Logger(ctx).InfoContext(ctx, "golang-sandbox listening", "addr", cfg.Listen, "runner", r.Name())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logctx.Logger(ctx).ErrorContext(ctx, "server failed", "err", err)
		sch.Shutdown(ctx)
		os.Exit(1)
	}
	<-shutdownDone
	logctx.Logger(ctx).InfoContext(ctx, "bye")
}
