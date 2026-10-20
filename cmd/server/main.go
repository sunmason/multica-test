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

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("config load failed", "err", err)
		os.Exit(1)
	}

	reg := metrics.New()
	r, err := runner.Select(context.Background(), cfg)
	if err != nil {
		log.Error("no usable runner", "err", err)
		os.Exit(1)
	}
	cfg.Runner = r.Name() // health/meta must report the ACTIVE backend, not the requested one
	log.Info("runner selected", "runner", r.Name(), "image", cfg.DockerImage)

	sch := sched.New(cfg, r, reg, log)
	api := server.New(cfg, sch, reg, log)
	sch.Start()
	api.StartGoVersionRefresh(context.Background(), r)

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
	go func() {
		<-stop
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		sch.Shutdown()
	}()

	log.Info("golang-sandbox listening", "addr", cfg.Listen, "runner", r.Name())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		sch.Shutdown()
		os.Exit(1)
	}
	log.Info("bye")
}
