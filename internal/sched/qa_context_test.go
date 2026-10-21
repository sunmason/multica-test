package sched_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/logctx"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
	"golang-sandbox/internal/sched"
)

type qaKey struct{}
type qaCall struct {
	ctx context.Context
	id  string
	req run.Request
}
type qaRunner struct {
	calls   chan qaCall
	release chan struct{}
}

func (r *qaRunner) Name() string                              { return "qa" }
func (r *qaRunner) Probe(context.Context) error               { return nil }
func (r *qaRunner) GoVersion(context.Context) (string, error) { return "qa", nil }
func (r *qaRunner) Sweep(context.Context)                     {}
func (r *qaRunner) Warmup(context.Context)                    {}
func (r *qaRunner) Execute(ctx context.Context, id string, req run.Request) *run.Result {
	r.calls <- qaCall{ctx, id, req}
	select {
	case <-ctx.Done():
		return &run.Result{Status: run.StatusCancelled}
	case <-r.release:
		return &run.Result{Status: run.StatusSuccess, Stdout: req.Code, Stderr: req.Stdin}
	}
}
func qaSetup(t *testing.T, wait time.Duration, logger *slog.Logger) (*sched.Scheduler, *qaRunner) {
	t.Helper()
	cfg := config.Default()
	cfg.Limits.MaxConcurrency = 1
	cfg.Limits.QueueCapacity = 2
	cfg.QueueWait = wait
	cfg.RateLimit.PerSourceRPM = 1000
	cfg.RateLimit.PerSourceConcurrent = 1
	r := &qaRunner{make(chan qaCall, 128), make(chan struct{})}
	s := sched.New(cfg, r, metrics.New(), logger)
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return s, r
}
func qaNext(t *testing.T, r *qaRunner) qaCall {
	t.Helper()
	select {
	case c := <-r.calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("runner not started")
		return qaCall{}
	}
}
func qaEventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if f() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("observable state did not settle")
		case <-tick.C:
		}
	}
}

// AC1/AC3: an accepted task detaches even an already expired request deadline.
// Expectations use only exported input/output and runner-visible context.
func TestQAExpiredRequestDeadlinePreservesAcceptedWork(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprint(queued), func(t *testing.T) {
			s, r := qaSetup(t, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
			var blocker *run.Record
			if queued {
				var ok bool
				if blocker, _, ok = s.Submit(context.Background(), "blocker", run.Request{}); !ok {
					t.Fatal("blocker rejected")
				}
				qaNext(t, r)
			}
			ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), qaKey{}, "payload"), time.Unix(1, 0))
			defer cancel()
			rec, reason, ok := s.Submit(ctx, "target", run.Request{Code: "stdout", Stdin: "stderr"})
			if !ok {
				t.Fatalf("rejected: %s", reason)
			}
			if queued {
				if rec.Status() != run.StatusQueued {
					t.Fatal("not queued")
				}
				if !s.Cancel(context.Background(), blocker.ID) {
					t.Fatal("blocker cancellation failed")
				}
			}
			call := qaNext(t, r)
			if call.ctx.Err() != nil || call.ctx.Value(qaKey{}) != "payload" || logctx.TraceID(call.ctx) != rec.ID {
				t.Fatal("lost context or inherited expired deadline")
			}
			if _, ok := call.ctx.Deadline(); ok {
				t.Fatal("request deadline retained")
			}
			close(r.release)
			qaEventually(t, func() bool { return rec.Status().Terminal() })
			st, _, _, res := rec.Snapshot()
			if st != run.StatusSuccess || res.Stdout != "stdout" || res.Stderr != "stderr" {
				t.Fatalf("result altered: %s %+v", st, res)
			}
			s.Shutdown(context.Background())
		})
	}
}

// AC2/AC3: timeout log must identify the queued run and use the injected logger;
// timed out work must never reach the runner, and its source budget is reusable.
func TestQAQueueTimeoutCorrelationAndReadmission(t *testing.T) {
	var logs bytes.Buffer
	s, r := qaSetup(t, 10*time.Millisecond, slog.New(slog.NewJSONHandler(&logs, nil)).With("qa_logger", true))
	_, _, ok := s.Submit(context.Background(), "blocker", run.Request{})
	if !ok {
		t.Fatal("blocker rejected")
	}
	qaNext(t, r)
	rec, _, ok := s.Submit(context.WithValue(context.Background(), qaKey{}, "timeout"), "target", run.Request{})
	if !ok {
		t.Fatal("target rejected")
	}
	qaEventually(t, func() bool { return rec.Status() == run.StatusQueueTimeout })
	next, reason, ok := s.Submit(context.Background(), "target", run.Request{})
	if !ok {
		t.Fatalf("source budget leaked: %s", reason)
	}
	close(r.release)
	c := qaNext(t, r)
	if c.id != next.ID {
		t.Fatalf("timed out work executed: %s", c.id)
	}
	s.Shutdown(context.Background())
	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		if e["run_id"] == rec.ID {
			found = true
			if e["trace_id"] != rec.ID || e["status"] != string(run.StatusQueueTimeout) || e["qa_logger"] != true {
				t.Fatalf("uncorrelated timeout: %v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing timeout log")
	}
	select {
	case c := <-r.calls:
		t.Fatalf("unexpected execution %s", c.id)
	default:
	}
}

// AC3: race promotion/completion, queue expiry, independent Cancel and Shutdown.
// No winner is prescribed; every accepted record must terminate and all public
// slots/queue counts must return to zero after Shutdown.
func TestQAPromotionCancelTimeoutShutdownRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		s, r := qaSetup(t, time.Microsecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		first, _, ok := s.Submit(context.Background(), "first", run.Request{})
		if !ok {
			t.Fatal("first rejected")
		}
		qaNext(t, r)
		second, _, ok := s.Submit(context.Background(), "second", run.Request{})
		if !ok {
			t.Fatal("second rejected")
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, f := range []func(){func() { close(r.release) }, func() { s.Cancel(context.Background(), first.ID); s.Cancel(context.Background(), second.ID) }, func() { s.Shutdown(context.Background()) }, func() {
			if rec, _, accepted := s.Submit(context.Background(), "third", run.Request{}); accepted {
				qaEventually(t, func() bool { return rec.Status().Terminal() })
			}
		}} {
			wg.Add(1)
			go func(f func()) { defer wg.Done(); <-start; f() }(f)
		}
		close(start)
		wg.Wait()
		s.Shutdown(context.Background())
		if !first.Status().Terminal() || !second.Status().Terminal() {
			t.Fatalf("iteration %d: live record after shutdown", i)
		}
		q, n := s.Stats(context.Background())
		if q != 0 || n != 0 {
			t.Fatalf("iteration %d: leaked queue=%d running=%d", i, q, n)
		}
		if _, _, accepted := s.Submit(context.Background(), "after", run.Request{}); accepted {
			t.Fatal("accepted work after shutdown")
		}
	}
}
