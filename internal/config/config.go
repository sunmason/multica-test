// Package config loads sandbox runtime configuration.
//
// All capacity knobs from the requirements baseline (LUOT-4 V2, section 五/八)
// live here so they can be tuned (and rolled back) without code changes.
// Defaults mirror the confirmed baseline: concurrency 3, CPU 1 core,
// memory 256MB, queue 10, queue wait 30s, exec timeout 10s, network off.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Limits holds per-execution and capacity limits.
type Limits struct {
	MaxConcurrency      int     `json:"max_concurrency"`        // global running slots (hard cap, confirmed: 3)
	QueueCapacity       int     `json:"queue_capacity"`         // bounded queue length (default 10)
	QueueWaitTimeoutSec int     `json:"queue_wait_timeout_sec"` // queued run gives up after this (default 30)
	ExecTimeoutSec      int     `json:"exec_timeout_sec"`       // wall-clock execution limit (default 10)
	CompileTimeoutSec   int     `json:"compile_timeout_sec"`    // compile phase budget (default 20)
	CPUCores            float64 `json:"cpu_cores"`              // per-run CPU quota (confirmed: 1)
	MemoryMB            int     `json:"memory_mb"`              // per-run memory limit (confirmed: 256)
	CompileMemoryMB     int     `json:"compile_memory_mb"`      // compile container memory (platform overhead, default 512)
	PidsLimit           int     `json:"pids_limit"`             // process/thread cap against fork bombs (default 64)
	OutputCapBytes      int     `json:"output_cap_bytes"`       // per-stream stdout/stderr cap (default 1MB)
	TmpfsMB             int     `json:"tmpfs_mb"`               // writable tmpfs size in sandbox (default 16)
	CodeMaxBytes        int     `json:"code_max_bytes"`         // submission size cap (default 64KB)
	StdinMaxBytes       int     `json:"stdin_max_bytes"`        // stdin size cap (default 64KB)
}

// RateLimit holds per-source (client IP) admission control.
type RateLimit struct {
	PerSourceRPM        int `json:"per_source_rpm"`        // submissions per minute per source (default 10)
	PerSourceConcurrent int `json:"per_source_concurrent"` // simultaneous queued+running per source (default 1)
}

// Config is the full server configuration.
type Config struct {
	Listen      string    `json:"listen"`
	Runner      string    `json:"runner"`       // "auto" | "docker" | "local"
	DockerImage string    `json:"docker_image"` // Go toolchain image for compile+run
	TrustProxy  bool      `json:"trust_proxy"`  // honor X-Forwarded-For (only behind a trusted proxy)
	Limits      Limits    `json:"limits"`
	RateLimit   RateLimit `json:"rate_limit"`

	// Derived durations, filled by Load.
	ExecTimeout    time.Duration `json:"-"`
	CompileTimeout time.Duration `json:"-"`
	QueueWait      time.Duration `json:"-"`
}

// Default returns the documented baseline defaults.
func Default() Config {
	return Config{
		Listen:      ":8080",
		Runner:      "auto",
		DockerImage: "golang:1.26-alpine",
		TrustProxy:  false,
		Limits: Limits{
			MaxConcurrency:      3,
			QueueCapacity:       10,
			QueueWaitTimeoutSec: 30,
			ExecTimeoutSec:      10,
			CompileTimeoutSec:   20,
			CPUCores:            1,
			MemoryMB:            256,
			CompileMemoryMB:     512,
			PidsLimit:           64,
			OutputCapBytes:      1 << 20,
			TmpfsMB:             16,
			CodeMaxBytes:        64 << 10,
			StdinMaxBytes:       64 << 10,
		},
		RateLimit: RateLimit{
			PerSourceRPM:        10,
			PerSourceConcurrent: 1,
		},
		ExecTimeout:    10 * time.Second,
		CompileTimeout: 20 * time.Second,
		QueueWait:      30 * time.Second,
	}
}

// Load builds the effective config: defaults <- JSON file <- GSB_ env vars.
// Unknown fields in the file are rejected so typos never silently disable a limit.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	cfg.ExecTimeout = time.Duration(cfg.Limits.ExecTimeoutSec) * time.Second
	cfg.CompileTimeout = time.Duration(cfg.Limits.CompileTimeoutSec) * time.Second
	cfg.QueueWait = time.Duration(cfg.Limits.QueueWaitTimeoutSec) * time.Second
	return cfg, nil
}

func (c Config) validate() error {
	l := c.Limits
	if l.MaxConcurrency < 1 {
		return fmt.Errorf("limits.max_concurrency must be >= 1, got %d", l.MaxConcurrency)
	}
	if l.QueueCapacity < 0 {
		return fmt.Errorf("limits.queue_capacity must be >= 0, got %d", l.QueueCapacity)
	}
	if l.ExecTimeoutSec < 1 || l.CompileTimeoutSec < 1 || l.QueueWaitTimeoutSec < 1 {
		return fmt.Errorf("timeouts must be >= 1 second")
	}
	if l.CPUCores <= 0 || l.MemoryMB < 32 || l.CompileMemoryMB < 64 {
		return fmt.Errorf("cpu/memory limits are implausibly low")
	}
	if l.PidsLimit < 1 || l.OutputCapBytes < 1 || l.TmpfsMB < 1 {
		return fmt.Errorf("pids/output/tmpfs limits must be positive")
	}
	if l.CodeMaxBytes < 1 || l.StdinMaxBytes < 0 {
		return fmt.Errorf("code/stdin size caps are invalid")
	}
	if c.RateLimit.PerSourceRPM < 1 || c.RateLimit.PerSourceConcurrent < 1 {
		return fmt.Errorf("rate limits must be >= 1")
	}
	switch c.Runner {
	case "auto", "docker", "local":
	default:
		return fmt.Errorf("runner must be auto|docker|local, got %q", c.Runner)
	}
	return nil
}

// applyEnv overlays flat GSB_* environment variables; ops can flip any
// headline knob (limits rollback, E4) without editing the config file.
func applyEnv(c *Config) {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
		}
	}
	intVar := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	str("GSB_LISTEN", &c.Listen)
	str("GSB_RUNNER", &c.Runner)
	str("GSB_DOCKER_IMAGE", &c.DockerImage)
	intVar("GSB_MAX_CONCURRENCY", &c.Limits.MaxConcurrency)
	intVar("GSB_QUEUE_CAPACITY", &c.Limits.QueueCapacity)
	intVar("GSB_QUEUE_WAIT_TIMEOUT_SEC", &c.Limits.QueueWaitTimeoutSec)
	intVar("GSB_EXEC_TIMEOUT_SEC", &c.Limits.ExecTimeoutSec)
	intVar("GSB_COMPILE_TIMEOUT_SEC", &c.Limits.CompileTimeoutSec)
	intVar("GSB_MEMORY_MB", &c.Limits.MemoryMB)
	intVar("GSB_PIDS_LIMIT", &c.Limits.PidsLimit)
	intVar("GSB_OUTPUT_CAP_BYTES", &c.Limits.OutputCapBytes)
	intVar("GSB_CODE_MAX_BYTES", &c.Limits.CodeMaxBytes)
	intVar("GSB_PER_SOURCE_RPM", &c.RateLimit.PerSourceRPM)
	intVar("GSB_PER_SOURCE_CONCURRENT", &c.RateLimit.PerSourceConcurrent)
}
