// Package sched orchestrates submissions: per-source admission, the global
// 3-slot hard cap, bounded FIFO queueing with wait timeout and cancel, and
// result bookkeeping (in-memory only — the platform keeps no persistent
// state; finished records are purged after a short TTL).
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/limit"
	"golang-sandbox/internal/logctx"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
	"golang-sandbox/internal/runner"
)

// Rejection reasons surfaced to clients as status "rejected".
const (
	RejectRateLimited = "rate_limited" // over per-source submissions/minute
	RejectSourceBusy  = "source_busy"  // over per-source simultaneous runs
	RejectQueueFull   = "queue_full"   // bounded queue is full
)

const (
	recordTTL    = 10 * time.Minute // how long terminal results stay pollable
	ewmaAlpha    = 0.3
	initEstimate = 2000 // ms, first guess before any measurement
	sweepEvery   = time.Minute
	shutdownWait = 20 * time.Second
)

// Scheduler is safe for concurrent use.
type Scheduler struct {
	cfg     config.Config
	runner  runner.Runner
	limiter *limit.Limiter
	metrics *metrics.Registry
	log     *slog.Logger

	mu     sync.Mutex
	slots  chan struct{}
	queue  []*run.Record
	runs   map[string]*run.Record
	ewmaMS float64
	full   bool // shutdown: refuse new submissions

	tasks map[string]*task

	sweepCancel context.CancelFunc
	sweepWG     sync.WaitGroup
	execWG      sync.WaitGroup
}

// task owns only live work; terminal records never retain request contexts.
type task struct {
	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
}

// New wires a scheduler around the chosen runner.
func New(cfg config.Config, r runner.Runner, m *metrics.Registry, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		runner:  r,
		limiter: limit.New(cfg.RateLimit.PerSourceRPM, cfg.RateLimit.PerSourceConcurrent),
		metrics: m,
		log:     log,
		slots:   make(chan struct{}, cfg.Limits.MaxConcurrency),
		runs:    make(map[string]*run.Record),
		ewmaMS:  initEstimate,
		tasks:   make(map[string]*task),
	}
}

// Start launches the initial orphan sweep, the periodic janitor, and the
// build-cache warmup.
func (s *Scheduler) Start(ctx context.Context) {
	ctx = logctx.WithLogger(ctx, s.log)
	ctx, cancel := context.WithCancel(ctx)
	s.sweepCancel = cancel
	s.sweepWG.Add(3)
	go func() {
		defer s.sweepWG.Done()
		s.runner.Sweep(ctx)
	}()
	go func() {
		defer s.sweepWG.Done()
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runner.Sweep(ctx)
			}
		}
	}()
	go func() {
		defer s.sweepWG.Done()
		s.runner.Warmup(ctx)
	}()
}

// Shutdown stops intake before cancelling work. All worker registrations
// happen under mu, so none can be added after shutdown begins waiting.
func (s *Scheduler) Shutdown(ctx context.Context) {
	ctx = logctx.WithLogger(ctx, s.log)
	s.mu.Lock()
	s.full = true
	for _, rec := range s.queue {
		t := s.tasks[rec.ID]
		s.finishAs(t.ctx, rec, &run.Result{Status: run.StatusCancelled, Reason: "服务正在重启，本次运行已取消", Runner: s.runner.Name()})
		s.releaseTaskLocked(rec)
		s.limiter.Release(rec.SourceIP)
	}
	s.queue = nil
	for _, t := range s.tasks {
		t.cancel()
	}
	s.updateGaugesLocked()
	s.mu.Unlock()
	if s.sweepCancel != nil {
		s.sweepCancel()
	}
	done := make(chan struct{})
	go func() {
		s.sweepWG.Wait()
		s.execWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(shutdownWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		logctx.Logger(ctx).WarnContext(ctx, "shutdown interrupted", "err", ctx.Err())
	case <-timer.C:
		logctx.Logger(ctx).WarnContext(ctx, "shutdown: some executions still tearing down", "waited", shutdownWait)
	}
}

// Submit admits one run: per-source limits first, then the slot/queue
// decision from section 五. On rejection the returned reason is one of the
// Reject* constants.
func (s *Scheduler) Submit(ctx context.Context, sourceIP string, req run.Request) (*run.Record, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return nil, RejectQueueFull, false
	}
	s.purgeExpiredLocked(time.Now())
	if reason, ok := s.limiter.Allow(sourceIP); !ok {
		s.metrics.Rejects().Inc(string(reason))
		return nil, string(reason), false
	}
	rec := run.NewRecord(logctx.NewID(), sourceIP, req, time.Now())
	select {
	case s.slots <- struct{}{}:
		rec.SetRunning()
	default:
		if len(s.queue) >= s.cfg.Limits.QueueCapacity {
			s.limiter.Release(sourceIP)
			s.metrics.Rejects().Inc(RejectQueueFull)
			return nil, RejectQueueFull, false
		}
		s.queue = append(s.queue, rec)
		rec.SetQueued(len(s.queue), s.estimateLocked(len(s.queue)))
	}
	// Acceptance transfers lifetime to the scheduler, preserving values while
	// detaching the HTTP deadline/cancellation. Cancel and Shutdown own cancel.
	ctx = logctx.WithTraceID(logctx.WithLogger(context.WithoutCancel(ctx), s.log), rec.ID)
	ctx, cancel := context.WithCancel(ctx)
	s.tasks[rec.ID] = &task{ctx: ctx, cancel: cancel}
	s.runs[rec.ID] = rec
	if rec.Status() == run.StatusRunning {
		s.launchLocked(rec)
	} else {
		s.tasks[rec.ID].timer = time.AfterFunc(s.cfg.QueueWait, func() { s.queueTimeout(rec) })
	}
	s.updateGaugesLocked()
	return rec, "", true
}

// Get returns the record for id, or nil.
func (s *Scheduler) Get(ctx context.Context, id string) *run.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

// Stats returns current queue depth and running count (health/status page).
func (s *Scheduler) Stats(ctx context.Context) (queued, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue), len(s.slots)
}

// Cancel stops a queued or running run and returns whether a cancellation
// was initiated.
func (s *Scheduler) Cancel(ctx context.Context, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.runs[id]
	if rec == nil || rec.Status().Terminal() {
		return false
	}
	ctx = logctx.WithTraceID(logctx.WithLogger(ctx, s.log), id)
	if rec.Status() == run.StatusQueued {
		s.removeQueuedLocked(rec)
		s.finishAs(ctx, rec, &run.Result{Status: run.StatusCancelled, Reason: "排队已取消，槽位已释放", Runner: s.runner.Name()})
		s.releaseTaskLocked(rec)
		s.limiter.Release(rec.SourceIP)
		s.updateGaugesLocked()
	} else {
		s.finishAs(ctx, rec, &run.Result{Status: run.StatusCancelled, Reason: "运行已被用户停止", Runner: s.runner.Name()})
		s.tasks[id].cancel()
	}
	return true
}

// launchLocked registers workers before releasing mu, making immediate
// cancellation and shutdown safe even before the executor goroutine starts.
func (s *Scheduler) launchLocked(rec *run.Record) {
	t := s.tasks[rec.ID]
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	s.execWG.Add(1)
	go func() {
		defer s.execWG.Done()
		s.execute(t.ctx, rec)
	}()
}

// releaseTaskLocked drops all context/cancel/timer references for completed work.
func (s *Scheduler) releaseTaskLocked(rec *run.Record) {
	t := s.tasks[rec.ID]
	if t.timer != nil {
		t.timer.Stop()
	}
	t.cancel()
	delete(s.tasks, rec.ID)
}

// execute runs the sandbox and then releases the slot and pumps the queue.
func (s *Scheduler) execute(ectx context.Context, rec *run.Record) {
	// Cancel/shutdown may have landed between slot grant and this goroutine.
	// Neither of those paths releases the per-source budget (they expected
	// this executor to), so it is released here exactly once.
	if rec.Status().Terminal() {
		s.mu.Lock()
		s.releaseTaskLocked(rec)
		s.mu.Unlock()
		s.limiter.Release(rec.SourceIP)
		<-s.slots
		s.pump()
		return
	}

	started := time.Now()
	res := s.runner.Execute(ectx, rec.ID, rec.Payload())

	if rec.Finish(res, time.Now()) {
		s.metrics.Runs().Inc(string(res.Status))
	}
	s.metrics.Duration().Observe(string(res.Status), time.Since(started).Seconds())
	s.limiter.Release(rec.SourceIP)

	logctx.Logger(ectx).InfoContext(ectx, "run finished",
		"run_id", rec.ID,
		"source", rec.SourceIP,
		// Finish may have lost to Cancel: status always reflects the stored
		// business outcome; runner_status preserves the executor's result.
		"status", rec.Status(),
		"runner_status", res.Status,
		"duration_ms", res.DurationMS,
		"queue_ms", started.Sub(rec.CreatedAt).Milliseconds(),
		"truncated", res.Truncated,
		"dropped_bytes", res.DroppedBytes,
		"exit_code", res.ExitCode,
		"runner", res.Runner,
	)

	s.mu.Lock()
	s.releaseTaskLocked(rec)
	if res.DurationMS > 0 {
		s.ewmaMS = (1-ewmaAlpha)*s.ewmaMS + ewmaAlpha*float64(res.DurationMS)
	}
	s.mu.Unlock()

	<-s.slots // release the slot token
	s.pump()
}

// pump starts queued runs while a slot is free. Called after every release;
// each released token starts at most one run.
func (s *Scheduler) pump() {
	for {
		s.mu.Lock()
		select {
		case s.slots <- struct{}{}:
		default:
			s.mu.Unlock()
			return
		}
		if len(s.queue) == 0 {
			s.mu.Unlock()
			<-s.slots // nothing to run: hand the token straight back
			return
		}
		rec := s.queue[0]
		s.queue = s.queue[1:]
		if !rec.SetRunning() {
			// Cancelled/timed out while waiting: hand the unused
			// reservation back before moving on, or the token budget
			// leaks and later queued runs may never start.
			s.mu.Unlock()
			<-s.slots
			continue
		}
		s.refreshPositionsLocked()
		s.updateGaugesLocked()
		s.launchLocked(rec)
		s.mu.Unlock()
	}
}

// queueTimeout fires when a queued run exceeded its wait budget (D6).
func (s *Scheduler) queueTimeout(rec *run.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Status() != run.StatusQueued {
		return
	}
	s.removeQueuedLocked(rec)
	s.updateGaugesLocked()
	res := &run.Result{
		Status: run.StatusQueueTimeout,
		Reason: fmt.Sprintf("等待执行超过 %d 秒，已自动取消排队，代码仍保留在编辑器中", s.cfg.Limits.QueueWaitTimeoutSec),
		Runner: s.runner.Name(),
	}
	s.finishAs(s.tasks[rec.ID].ctx, rec, res)
	s.metrics.Runs().Inc(string(res.Status))
	s.releaseTaskLocked(rec)
	s.limiter.Release(rec.SourceIP)
}

// finishAs logs only the winning transition, preserving the run correlation.
func (s *Scheduler) finishAs(ctx context.Context, rec *run.Record, res *run.Result) {
	if rec.Finish(res, time.Now()) {
		logctx.Logger(ctx).InfoContext(ctx, "run stopped", "run_id", rec.ID, "status", res.Status)
	}
}

func (s *Scheduler) removeQueuedLocked(rec *run.Record) {
	for i, q := range s.queue {
		if q == rec {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.refreshPositionsLocked()
			return
		}
	}
}

func (s *Scheduler) refreshPositionsLocked() {
	for i, q := range s.queue {
		q.SetQueued(i+1, s.estimateLocked(i+1))
	}
}

func (s *Scheduler) estimateLocked(position int) int64 {
	return int64(s.ewmaMS * float64(position))
}

func (s *Scheduler) updateGaugesLocked() {
	s.metrics.Running().Set("", float64(len(s.slots)))
	s.metrics.QueueDepth().Set("", float64(len(s.queue)))
}

// purgeExpiredLocked drops terminal records older than the TTL so the
// in-memory map stays bounded under anonymous traffic.
func (s *Scheduler) purgeExpiredLocked(now time.Time) {
	for id, rec := range s.runs {
		if st, _, _, res := rec.Snapshot(); st.Terminal() && res != nil && now.Sub(rec.UpdatedAt()) > recordTTL {
			delete(s.runs, id)
		}
	}
}
