package sched

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
	"golang-sandbox/internal/runner"
)

// The scheduler tests run real programs through the local runner (skip when
// no Go toolchain). They exercise the queue/limit/cancel machinery under
// real concurrency.

const sleeperSrc = "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(60 * time.Second) }\n"

func testSetup(t *testing.T, mutate func(*config.Config)) (*Scheduler, *metrics.Registry) {
	t.Helper()
	cfg := config.Default()
	cfg.Limits.MaxConcurrency = 2
	cfg.Limits.QueueCapacity = 2
	cfg.Limits.QueueWaitTimeoutSec = 2
	cfg.Limits.ExecTimeoutSec = 3
	cfg.Limits.CompileTimeoutSec = 60
	cfg.Limits.CodeMaxBytes = 64 << 10
	cfg.RateLimit.PerSourceRPM = 100
	cfg.RateLimit.PerSourceConcurrent = 10
	cfg.ExecTimeout = 3 * time.Second
	cfg.CompileTimeout = 60 * time.Second
	cfg.QueueWait = 2 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := runner.NewLocal(cfg)
	if err != nil {
		t.Skipf("local runner unavailable: %v", err)
	}
	m := metrics.New()
	s := New(cfg, r, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return s, m
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestSubmitRunsImmediatelyWithFreeSlot(t *testing.T) {
	s, _ := testSetup(t, nil)
	rec, reason, ok := s.Submit(context.Background(), "ip1", run.Request{Code: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"ok\") }\n"})
	if !ok {
		t.Fatalf("rejected: %s", reason)
	}
	waitFor(t, 15*time.Second, func() bool {
		st, _, _, res := rec.Snapshot()
		return st.Terminal() && res != nil
	}, "run never finished")
	st, _, _, res := rec.Snapshot()
	if st != run.StatusSuccess || res.Stdout != "ok\n" {
		t.Errorf("status=%s stdout=%q stderr=%s", st, res.Stdout, res.Stderr)
	}
}

func TestQueueThenDequeueOnSlotRelease(t *testing.T) {
	// Queue wait must outlast the blockers so this tests dequeue, not timeout.
	s, _ := testSetup(t, func(c *config.Config) { c.Limits.QueueWaitTimeoutSec = 30; c.QueueWait = 30 * time.Second })
	b1, _, ok1 := s.Submit(context.Background(), "ip1", run.Request{Code: sleeperSrc})
	b2, _, ok2 := s.Submit(context.Background(), "ip2", run.Request{Code: sleeperSrc})
	if !ok1 || !ok2 {
		t.Fatalf("blockers rejected: %v %v", ok1, ok2)
	}
	waitFor(t, 5*time.Second, func() bool { return b1.Status() == run.StatusRunning && b2.Status() == run.StatusRunning },
		"blockers never started")

	q1, _, ok3 := s.Submit(context.Background(), "ip3", run.Request{Code: "package main\n\nfunc main() {}\n"})
	if !ok3 || q1.Status() != run.StatusQueued {
		t.Fatalf("expected queued, got ok=%v status=%s", ok3, q1.Status())
	}
	_, pos, _, _ := q1.Snapshot()
	if pos != 1 {
		t.Errorf("position = %d, want 1", pos)
	}

	// Free one slot; the queued run must take it and finish.
	s.Cancel(context.Background(), b1.ID)
	waitFor(t, 20*time.Second, func() bool {
		st, _, _, res := q1.Snapshot()
		return st.Terminal() && res != nil && res.Status == run.StatusSuccess
	}, "queued run never dequeued/finished")
	s.Cancel(context.Background(), b2.ID)
}

func TestQueueFullIsRejected(t *testing.T) {
	s, _ := testSetup(t, func(c *config.Config) {
		c.Limits.QueueCapacity = 1
		c.Limits.QueueWaitTimeoutSec = 30
		c.QueueWait = 30 * time.Second
	})
	for _, ip := range []string{"a", "b"} {
		if _, _, ok := s.Submit(context.Background(), ip, run.Request{Code: sleeperSrc}); !ok {
			t.Fatalf("blocker for %s rejected", ip)
		}
	}
	if _, _, ok := s.Submit(context.Background(), "c", run.Request{Code: sleeperSrc}); !ok {
		t.Fatal("first queued run should fit")
	}
	_, reason, ok := s.Submit(context.Background(), "d", run.Request{Code: sleeperSrc})
	if ok || reason != RejectQueueFull {
		t.Errorf("expected queue_full rejection, got ok=%v reason=%s", ok, reason)
	}
}

func TestQueueWaitTimeout(t *testing.T) {
	s, _ := testSetup(t, nil)
	b1, _, _ := s.Submit(context.Background(), "a", run.Request{Code: sleeperSrc})
	b2, _, _ := s.Submit(context.Background(), "b", run.Request{Code: sleeperSrc})
	if b1.Status() != run.StatusRunning || b2.Status() != run.StatusRunning {
		t.Fatal("blockers not running")
	}
	q, _, ok := s.Submit(context.Background(), "c", run.Request{Code: sleeperSrc})
	if !ok || q.Status() != run.StatusQueued {
		t.Fatalf("expected queued, got ok=%v status=%s", ok, q.Status())
	}
	waitFor(t, 6*time.Second, func() bool { return q.Status() == run.StatusQueueTimeout },
		"queue timeout never fired")
	s.Cancel(context.Background(), b1.ID)
	s.Cancel(context.Background(), b2.ID)
}

func TestCancelRunningReleasesSlot(t *testing.T) {
	s, _ := testSetup(t, nil)
	b, _, ok := s.Submit(context.Background(), "a", run.Request{Code: sleeperSrc})
	if !ok {
		t.Fatal("blocker rejected")
	}
	waitFor(t, 5*time.Second, func() bool { return b.Status() == run.StatusRunning }, "blocker never started")

	if !s.Cancel(context.Background(), b.ID) {
		t.Fatal("Cancel returned false for a running run")
	}
	waitFor(t, 10*time.Second, func() bool { return b.Status() == run.StatusCancelled }, "cancel never landed")

	// Slot must be free: a new run goes straight to running, not queued.
	n, _, ok := s.Submit(context.Background(), "z", run.Request{Code: "package main\n\nfunc main() {}\n"})
	if !ok {
		t.Fatal("follow-up run rejected")
	}
	if n.Status() != run.StatusRunning {
		t.Errorf("slot not released: status = %s", n.Status())
	}
	waitFor(t, 15*time.Second, func() bool { return n.Status().Terminal() }, "follow-up never finished")
}

func TestCancelQueued(t *testing.T) {
	s, _ := testSetup(t, nil)
	b1, _, _ := s.Submit(context.Background(), "a", run.Request{Code: sleeperSrc})
	b2, _, _ := s.Submit(context.Background(), "b", run.Request{Code: sleeperSrc})
	waitFor(t, 5*time.Second, func() bool { return b1.Status() == run.StatusRunning && b2.Status() == run.StatusRunning },
		"blockers never started")
	q, _, ok := s.Submit(context.Background(), "c", run.Request{Code: sleeperSrc})
	if !ok {
		t.Fatal("queue submission failed")
	}
	if !s.Cancel(context.Background(), q.ID) {
		t.Fatal("Cancel returned false for queued run")
	}
	if q.Status() != run.StatusCancelled {
		t.Errorf("status = %s, want cancelled", q.Status())
	}
	s.Cancel(context.Background(), b1.ID)
	s.Cancel(context.Background(), b2.ID)
}

func TestPerSourceRateLimit(t *testing.T) {
	s, _ := testSetup(t, func(c *config.Config) { c.RateLimit.PerSourceRPM = 1 })
	r1, _, ok1 := s.Submit(context.Background(), "a", run.Request{Code: "package main\n\nfunc main() {}\n"})
	if !ok1 {
		t.Fatal("first submit rejected")
	}
	_, reason, ok2 := s.Submit(context.Background(), "a", run.Request{Code: "package main\n\nfunc main() {}\n"})
	if ok2 || reason != RejectRateLimited {
		t.Errorf("expected rate_limited, got ok=%v reason=%s", ok2, reason)
	}
	// Different source unaffected.
	if _, _, ok := s.Submit(context.Background(), "b", run.Request{Code: "package main\n\nfunc main() {}\n"}); !ok {
		t.Error("other source should be allowed")
	}
	waitFor(t, 15*time.Second, func() bool { return r1.Status().Terminal() }, "first run never finished")
}

func TestPerSourceConcurrencyLimit(t *testing.T) {
	s, _ := testSetup(t, func(c *config.Config) {
		c.RateLimit.PerSourceConcurrent = 1
		c.Limits.QueueWaitTimeoutSec = 30
		c.QueueWait = 30 * time.Second
	})
	b, _, ok := s.Submit(context.Background(), "a", run.Request{Code: sleeperSrc})
	if !ok {
		t.Fatal("blocker rejected")
	}
	_, reason, ok := s.Submit(context.Background(), "a", run.Request{Code: sleeperSrc})
	if ok || reason != RejectSourceBusy {
		t.Errorf("expected source_busy, got ok=%v reason=%s", ok, reason)
	}
	s.Cancel(context.Background(), b.ID)
}

func TestCountedMetricsPerTerminalStatus(t *testing.T) {
	s, m := testSetup(t, nil)
	rec, _, ok := s.Submit(context.Background(), "a", run.Request{Code: "package main\n\nfunc main() {}\n"})
	if !ok {
		t.Fatal("submit rejected")
	}
	waitFor(t, 15*time.Second, func() bool { return rec.Status().Terminal() }, "run never finished")
	waitFor(t, 2*time.Second, func() bool {
		_, running := s.Stats(context.Background())
		return running == 0
	}, "slot never released")
	// The render includes the success counter line.
	out := m.Render()
	if !contains(out, `gosandbox_runs_total{status="success"}`) {
		t.Errorf("metrics missing success counter:\n%s", out)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
