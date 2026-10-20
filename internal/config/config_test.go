package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultMatchesBaseline(t *testing.T) {
	cfg := Default()
	if cfg.Limits.MaxConcurrency != 3 {
		t.Errorf("MaxConcurrency = %d, want 3 (confirmed hard cap)", cfg.Limits.MaxConcurrency)
	}
	if cfg.Limits.MemoryMB != 256 || cfg.Limits.CPUCores != 1 {
		t.Errorf("memory/cpu = %d/%v, want 256MB/1 core", cfg.Limits.MemoryMB, cfg.Limits.CPUCores)
	}
	if cfg.Limits.QueueCapacity != 10 || cfg.QueueWait != 30*time.Second {
		t.Errorf("queue = %d/%s, want 10/30s", cfg.Limits.QueueCapacity, cfg.QueueWait)
	}
	if cfg.ExecTimeout != 10*time.Second {
		t.Errorf("ExecTimeout = %s, want 10s", cfg.ExecTimeout)
	}
	if cfg.RateLimit.PerSourceRPM != 10 || cfg.RateLimit.PerSourceConcurrent != 1 {
		t.Errorf("rate limit = %v", cfg.RateLimit)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	body := `{
		"listen": ":9999",
		"limits": {"max_concurrency": 5, "exec_timeout_sec": 7},
		"rate_limit": {"per_source_rpm": 2}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9999" || cfg.Limits.MaxConcurrency != 5 || cfg.Limits.ExecTimeoutSec != 7 {
		t.Errorf("file overrides not applied: %+v", cfg)
	}
	if cfg.Limits.MemoryMB != 256 {
		t.Errorf("unset field should keep default, got %d", cfg.Limits.MemoryMB)
	}
	if cfg.ExecTimeout != 7*time.Second {
		t.Errorf("derived duration = %s, want 7s", cfg.ExecTimeout)
	}
	if cfg.RateLimit.PerSourceRPM != 2 {
		t.Errorf("per_source_rpm = %d, want 2", cfg.RateLimit.PerSourceRPM)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"limits":{"max_concurrency":"3"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("GSB_MAX_CONCURRENCY", "7")
	t.Setenv("GSB_EXEC_TIMEOUT_SEC", "3")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.MaxConcurrency != 7 || cfg.Limits.ExecTimeoutSec != 3 {
		t.Errorf("env override failed: %+v", cfg.Limits)
	}
}

func TestValidateRejectsNonsense(t *testing.T) {
	cfg := Default()
	cfg.Limits.MaxConcurrency = 0
	if err := cfg.validate(); err == nil {
		t.Error("max_concurrency=0 must be rejected")
	}
	cfg = Default()
	cfg.Runner = "k8s"
	if err := cfg.validate(); err == nil {
		t.Error("unknown runner must be rejected")
	}
}
