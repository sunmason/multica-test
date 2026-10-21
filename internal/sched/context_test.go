package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/logctx"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
)

type execution struct {
	ctx     context.Context
	id      string
	release chan struct{}
}

type controlledRunner struct{ calls chan execution }

func (r *controlledRunner) Name() string                              { return "controlled" }
func (r *controlledRunner) Probe(context.Context) error               { return nil }
func (r *controlledRunner) GoVersion(context.Context) (string, error) { return "go test", nil }
func (r *controlledRunner) Sweep(context.Context)                     {}
func (r *controlledRunner) Warmup(context.Context)                    {}
func (r *controlledRunner) Execute(ctx context.Context, id string, _ run.Request) *run.Result {
	call := execution{ctx: ctx, id: id, release: make(chan struct{})}
	r.calls <- call
	select {
	case <-call.release:
		return &run.Result{Status: run.StatusSuccess, Runner: r.Name()}
	case <-ctx.Done():
		return &run.Result{Status: run.StatusCancelled, Runner: r.Name()}
	}
}
func nextExecution(t *testing.T, r *controlledRunner) execution {
	t.Helper()
	select {
	case call := <-r.calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("executor did not start")
		return execution{}
	}
}
func contextScheduler(t *testing.T, wait time.Duration) (*Scheduler, *controlledRunner, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.Limits.MaxConcurrency = 1
	cfg.Limits.QueueCapacity = 2
	cfg.RateLimit.PerSourceRPM = 100
	cfg.RateLimit.PerSourceConcurrent = 10
	cfg.QueueWait = wait
	r := &controlledRunner{calls: make(chan execution, 4)}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("component", "injected")
	s := New(cfg, r, metrics.New(), logger)
	t.Cleanup(func() { s.Shutdown(context.Background()) })
	return s, r, &logs
}

type contextValueKey struct{}

func TestExecutionContextSurvivesRequestCancelImmediateAndQueue(t *testing.T) {
	s, r, logs := contextScheduler(t, time.Hour)
	parent1, cancel1 := context.WithCancel(context.WithValue(context.Background(), contextValueKey{}, "direct"))
	defer cancel1()
	first, _, ok := s.Submit(parent1, "first", run.Request{})
	if !ok {
		t.Fatal("first rejected")
	}
	call1 := nextExecution(t, r)
	cancel1()
	parent2, cancel2 := context.WithTimeout(context.WithValue(context.Background(), contextValueKey{}, "queued"), time.Hour)
	defer cancel2()
	second, _, ok := s.Submit(parent2, "second", run.Request{})
	if !ok || second.Status() != run.StatusQueued {
		t.Fatal("second not queued")
	}
	cancel2()
	close(call1.release)
	call2 := nextExecution(t, r)
	for i, tc := range []struct {
		call  execution
		rec   *run.Record
		value string
	}{{call1, first, "direct"}, {call2, second, "queued"}} {
		if tc.call.ctx.Value(contextValueKey{}) != tc.value || logctx.TraceID(tc.call.ctx) != tc.rec.ID || tc.call.id != tc.rec.ID {
			t.Fatalf("call %d lost its own values/trace", i)
		}
		if _, hasDeadline := tc.call.ctx.Deadline(); hasDeadline {
			t.Fatal("request deadline leaked to execution")
		}
	}
	// The first has finished and its cancel must have been released. The
	// queued execution is still alive after both request parents were cancelled.
	if call2.ctx.Err() != nil {
		t.Fatalf("queued context cancelled by request: %v", call2.ctx.Err())
	}
	close(call2.release)
	s.execWG.Wait()
	for _, call := range []execution{call1, call2} {
		if call.ctx.Err() != context.Canceled {
			t.Fatal("completed context not released")
		}
	}
	s.mu.Lock()
	live := len(s.tasks)
	s.mu.Unlock()
	if live != 0 {
		t.Fatalf("retained %d live contexts", live)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["component"] != "injected" || entry["trace_id"] != entry["run_id"] {
			t.Fatalf("uncorrelated log: %v", entry)
		}
		seen[entry["run_id"].(string)] = true
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatal("missing execution log")
	}
}

func TestQueuedContextsReleasedOnCancelTimeoutAndShutdown(t *testing.T) {
	for _, action := range []string{"cancel", "timeout", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			wait := time.Hour
			if action == "timeout" {
				wait = 20 * time.Millisecond
			}
			s, r, _ := contextScheduler(t, wait)
			blocker, _, _ := s.Submit(context.Background(), "blocker", run.Request{})
			call := nextExecution(t, r)
			queued, _, ok := s.Submit(context.Background(), "queued", run.Request{})
			if !ok {
				t.Fatal("queued rejected")
			}
			s.mu.Lock()
			queuedCtx := s.tasks[queued.ID].ctx
			s.mu.Unlock()
			want := run.StatusCancelled
			switch action {
			case "cancel":
				if !s.Cancel(context.Background(), queued.ID) {
					t.Fatal("cancel failed")
				}
			case "timeout":
				want = run.StatusQueueTimeout
				waitFor(t, time.Second, func() bool { return queued.Status() == want }, "queue timeout did not fire")
			case "shutdown":
				s.Shutdown(context.Background())
			}
			waitTaskCancellation(t, queuedCtx)
			if queued.Status() != want || queuedCtx.Err() != context.Canceled {
				t.Fatalf("status=%s ctx=%v", queued.Status(), queuedCtx.Err())
			}
			if action != "shutdown" {
				if call.ctx.Err() != nil {
					t.Fatal("queued cancellation affected running work")
				}
				s.Cancel(context.Background(), blocker.ID)
			}
			s.execWG.Wait()
			if call.ctx.Err() != context.Canceled {
				t.Fatal("running cancellation not propagated")
			}
			s.mu.Lock()
			live := len(s.tasks)
			s.mu.Unlock()
			if live != 0 {
				t.Fatal("contexts retained after cancellation")
			}
		})
	}
}

type lifecycleRunner struct {
	*controlledRunner
	warmup chan context.Context
	sweep  chan context.Context
}

func (r *lifecycleRunner) Sweep(ctx context.Context) { r.sweep <- ctx }
func (r *lifecycleRunner) Warmup(ctx context.Context) {
	r.warmup <- ctx
	<-ctx.Done()
	logctx.Logger(ctx).InfoContext(ctx, "warmup stopped")
}
func TestLifecyclePreservesContextAndWaitsForWarmup(t *testing.T) {
	cfg := config.Default()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("component", "injected")
	r := &lifecycleRunner{controlledRunner: &controlledRunner{calls: make(chan execution, 1)}, warmup: make(chan context.Context, 1), sweep: make(chan context.Context, 1)}
	s := New(cfg, r, metrics.New(), logger)
	ctx := logctx.WithTraceID(context.WithValue(context.Background(), contextValueKey{}, "lifecycle"), "startup")
	s.Start(ctx)
	t.Cleanup(func() { s.Shutdown(ctx) })
	for _, ch := range []chan context.Context{r.warmup, r.sweep} {
		select {
		case received := <-ch:
			if received.Value(contextValueKey{}) != "lifecycle" || logctx.TraceID(received) != "startup" {
				t.Fatal("lifecycle lost context")
			}
		case <-time.After(time.Second):
			t.Fatal("lifecycle worker not started")
		}
	}
	s.Shutdown(ctx)
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["trace_id"] != "startup" || entry["component"] != "injected" || entry["msg"] != "warmup stopped" {
		t.Fatalf("wrong warmup log: %v", entry)
	}
}

func TestConcurrentSubmitCancelAndShutdownReleasesTasks(t *testing.T) {
	s, r, _ := contextScheduler(t, time.Millisecond)
	r.calls = make(chan execution, 64)
	first, _, ok := s.Submit(context.Background(), "first", run.Request{})
	if !ok {
		t.Fatal("initial submission rejected")
	}
	nextExecution(t, r)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, _, ok := s.Submit(context.Background(), "racing", run.Request{})
			if ok {
				s.Cancel(context.Background(), rec.ID)
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; s.Shutdown(context.Background()) }()
	close(start)
	s.Cancel(context.Background(), first.ID)
	wg.Wait()
	s.Shutdown(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) != 0 || len(s.queue) != 0 || len(s.slots) != 0 {
		t.Fatalf("shutdown retained work: tasks=%d queue=%d slots=%d", len(s.tasks), len(s.queue), len(s.slots))
	}
}

// delayedResultRunner models an executor that has computed success but is
// still cleaning up when the user cancels. It returns that result unchanged.
type delayedResultRunner struct {
	*controlledRunner
	release chan struct{}
}

func (r *delayedResultRunner) Execute(ctx context.Context, id string, _ run.Request) *run.Result {
	r.calls <- execution{ctx: ctx, id: id, release: r.release}
	<-r.release
	return &run.Result{Status: run.StatusSuccess, Runner: r.Name(), DurationMS: 42}
}

func TestCancelledRunLogPreservesBusinessStatusAfterRunnerSuccess(t *testing.T) {
	s, r, logs := contextScheduler(t, time.Hour)
	delayed := &delayedResultRunner{controlledRunner: r, release: make(chan struct{})}
	s.runner = delayed
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(delayed.release) }) }
	t.Cleanup(release)
	rec, _, ok := s.Submit(context.Background(), "cancel-before-return", run.Request{})
	if !ok {
		t.Fatal("submission rejected")
	}
	nextExecution(t, r)
	if !s.Cancel(context.Background(), rec.ID) {
		t.Fatal("cancellation rejected")
	}
	assertCancelled := func() {
		t.Helper()
		st, _, _, result := s.Get(context.Background(), rec.ID).Snapshot()
		if st != run.StatusCancelled || result == nil || result.Status != run.StatusCancelled {
			t.Fatalf("poll status=%s result=%+v, want cancelled", st, result)
		}
	}
	assertCancelled()
	release()
	s.execWG.Wait()
	assertCancelled()
	stopped, finished := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["run_id"] != rec.ID || entry["trace_id"] != rec.ID {
			t.Fatalf("wrong correlation: %v", entry)
		}
		if entry["status"] != string(run.StatusCancelled) {
			t.Errorf("log contradicts polled business status: %v", entry)
		}
		switch entry["msg"] {
		case "run stopped":
			stopped++
		case "run finished":
			finished++
			if entry["runner_status"] != string(run.StatusSuccess) || entry["duration_ms"] != float64(42) || entry["runner"] != delayed.Name() {
				t.Errorf("runner outcome lost: %v", entry)
			}
		}
	}
	if stopped != 1 || finished != 1 {
		t.Fatalf("stopped=%d finished=%d, want both events", stopped, finished)
	}
}

// timeoutLogGate passes the logging context to the test before waiting for
// release. The test never needs s.mu while the timeout callback holds it.
type timeoutLogGate struct {
	slog.Handler
	blocked chan context.Context
	release chan struct{}
}

func (g timeoutLogGate) WithAttrs(attrs []slog.Attr) slog.Handler {
	g.Handler = g.Handler.WithAttrs(attrs)
	return g
}

func (g timeoutLogGate) WithGroup(name string) slog.Handler {
	g.Handler = g.Handler.WithGroup(name)
	return g
}

func (g timeoutLogGate) Handle(ctx context.Context, record slog.Record) error {
	timeout := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "status" && attr.Value.String() == string(run.StatusQueueTimeout) {
			timeout = true
		}
		return true
	})
	if timeout {
		g.blocked <- ctx
		<-g.release
	}
	return g.Handler.Handle(ctx, record)
}

func TestQueueTimeoutReleaseWaitsForContextDone(t *testing.T) {
	s, r, _ := contextScheduler(t, 20*time.Millisecond)
	gate := timeoutLogGate{
		Handler: slog.NewJSONHandler(io.Discard, nil),
		blocked: make(chan context.Context, 1),
		release: make(chan struct{}),
	}
	s.log = slog.New(gate)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(unblock)
	if _, _, ok := s.Submit(context.Background(), "blocker", run.Request{}); !ok {
		t.Fatal("blocker rejected")
	}
	nextExecution(t, r)
	queued, _, ok := s.Submit(context.Background(), "queued", run.Request{})
	if !ok {
		t.Fatal("queued rejected")
	}
	// Force the adverse ordering: the callback is inside its gate before
	// obtaining queuedCtx, without taking the scheduler lock to obtain it.
	var queuedCtx context.Context
	select {
	case queuedCtx = <-gate.blocked:
	case <-time.After(time.Second):
		t.Fatal("timeout log was not reached")
	}
	if queued.Status() != run.StatusQueueTimeout {
		t.Fatal("timeout status not visible")
	}
	if queuedCtx.Err() != nil {
		t.Fatal("expected terminal record to be visible while context release is blocked")
	}
	unblock()
	waitTaskCancellation(t, queuedCtx)
	if queuedCtx.Err() != context.Canceled {
		t.Fatalf("queued context not cancelled: %v", queuedCtx.Err())
	}
	s.mu.Lock()
	_, retained := s.tasks[queued.ID]
	s.mu.Unlock()
	if retained {
		t.Fatal("released task retained its context")
	}
}

// A terminal record can become visible before the timeout handler releases
// its context. Synchronize on cancellation itself, with a bounded failure.
func waitTaskCancellation(t *testing.T, ctx context.Context) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		t.Fatal("task context was not released")
	}
}
