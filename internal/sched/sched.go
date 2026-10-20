// Package sched orchestrates submissions: per-source admission, the global
// 3-slot hard cap, bounded FIFO queueing with wait timeout and cancel, and
// result bookkeeping (in-memory only — the platform keeps no persistent
// state; finished records are purged after a short TTL).
package sched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/limit"
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

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc

	sweepCancel context.CancelFunc
	sweepWG     sync.WaitGroup
	execWG      sync.WaitGroup
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
		cancels: make(map[string]context.CancelFunc),
	}
}

// Start launches the initial orphan sweep, the periodic janitor, and the
// build-cache warmup.
func (s *Scheduler) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.sweepCancel = cancel
	s.sweepWG.Add(2)
	go func() {
		defer s.sweepWG.Done()
		s.runner.Sweep(ctx) // residue from a previous crashed process
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
	go s.runner.Warmup(ctx)
}

// Shutdown stops intake, cancels queued and running work, and waits (bounded)
// for executors to tear their sandboxes down, so no container outlives the
// process by more than the sweeper horizon.
func (s *Scheduler) Shutdown() {
	if s.sweepCancel != nil {
		s.sweepCancel()
	}
	s.sweepWG.Wait()

	s.mu.Lock()
	s.full = true
	queued := append([]*run.Record(nil), s.queue...)
	s.queue = nil
	s.updateGaugesLocked()
	s.mu.Unlock()

	for _, rec := range queued {
		s.finishAs(rec, &run.Result{Status: run.StatusCancelled, Reason: "服务正在重启，本次运行已取消", Runner: s.runner.Name()})
		s.limiter.Release(rec.SourceIP)
	}

	s.cancelMu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.cancelMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.execWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		s.log.Warn("shutdown: some executions still tearing down", "waited", shutdownWait)
	}
}

// Submit admits one run: per-source limits first, then the slot/queue
// decision from section 五. On rejection the returned reason is one of the
// Reject* constants.
func (s *Scheduler) Submit(sourceIP string, req run.Request) (*run.Record, string, bool) {
	s.mu.Lock()
	if s.full {
		s.mu.Unlock()
		return nil, RejectQueueFull, false
	}
	s.purgeExpiredLocked(time.Now())
	s.mu.Unlock()

	if reason, ok := s.limiter.Allow(sourceIP); !ok {
		s.metrics.Rejects().Inc(string(reason))
		return nil, string(reason), false
	}

	rec := run.NewRecord(newID(), sourceIP, req, time.Now())
	s.mu.Lock()
	s.runs[rec.ID] = rec
	select {
	case s.slots <- struct{}{}:
		rec.SetRunning()
		s.updateGaugesLocked()
		s.mu.Unlock()
		s.launch(rec)
		return rec, "", true
	default:
		if len(s.queue) >= s.cfg.Limits.QueueCapacity {
			delete(s.runs, rec.ID)
			s.mu.Unlock()
			s.limiter.Release(sourceIP)
			s.metrics.Rejects().Inc(RejectQueueFull)
			return nil, RejectQueueFull, false
		}
		s.queue = append(s.queue, rec)
		rec.SetQueued(len(s.queue), s.estimateLocked(len(s.queue)))
		s.updateGaugesLocked()
		s.mu.Unlock()
		time.AfterFunc(s.cfg.QueueWait, func() { s.queueTimeout(rec) })
		return rec, "", true
	}
}

// Get returns the record for id, or nil.
func (s *Scheduler) Get(id string) *run.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

// Stats returns current queue depth and running count (health/status page).
func (s *Scheduler) Stats() (queued, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue), len(s.slots)
}

// Cancel stops a queued or running run and returns whether a cancellation
// was initiated.
func (s *Scheduler) Cancel(id string) bool {
	rec := s.Get(id)
	if rec == nil {
		return false
	}
	switch rec.Status() {
	case run.StatusQueued:
		s.mu.Lock()
		s.removeQueuedLocked(rec)
		s.updateGaugesLocked()
		s.mu.Unlock()
		s.finishAs(rec, &run.Result{Status: run.StatusCancelled, Reason: "排队已取消，槽位已释放", Runner: s.runner.Name()})
		s.limiter.Release(rec.SourceIP)
		return true
	case run.StatusRunning:
		// Terminal state goes up immediately; the executor's own (partial)
		// result is discarded, and it still frees the slot after the
		// container is confirmed dead.
		s.finishAs(rec, &run.Result{Status: run.StatusCancelled, Reason: "运行已被用户停止", Runner: s.runner.Name()})
		s.cancelMu.Lock()
		cancel, ok := s.cancels[id]
		s.cancelMu.Unlock()
		if ok {
			cancel()
		}
		return true
	default:
		return false
	}
}

// launch registers the execution context synchronously (so an immediate
// Cancel can always find the cancel func) and then spawns the executor.
func (s *Scheduler) launch(rec *run.Record) {
	ectx, ecancel := context.WithCancel(context.Background())
	s.cancelMu.Lock()
	s.cancels[rec.ID] = ecancel
	s.cancelMu.Unlock()
	s.execWG.Add(1)
	go func() {
		defer s.execWG.Done()
		s.execute(rec, ectx, ecancel)
	}()
}

// execute runs the sandbox and then releases the slot and pumps the queue.
func (s *Scheduler) execute(rec *run.Record, ectx context.Context, ecancel context.CancelFunc) {
	defer func() {
		s.cancelMu.Lock()
		delete(s.cancels, rec.ID)
		s.cancelMu.Unlock()
		ecancel()
	}()

	// Cancel/shutdown may have landed between slot grant and this goroutine.
	// Neither of those paths releases the per-source budget (they expected
	// this executor to), so it is released here exactly once.
	if rec.Status().Terminal() {
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

	slog.Info("run finished",
		"run_id", rec.ID,
		"source", rec.SourceIP,
		"status", res.Status,
		"duration_ms", res.DurationMS,
		"queue_ms", started.Sub(rec.CreatedAt).Milliseconds(),
		"truncated", res.Truncated,
		"dropped_bytes", res.DroppedBytes,
		"exit_code", res.ExitCode,
		"runner", res.Runner,
	)

	s.mu.Lock()
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
		s.mu.Unlock()
		s.launch(rec)
	}
}

// queueTimeout fires when a queued run exceeded its wait budget (D6).
func (s *Scheduler) queueTimeout(rec *run.Record) {
	if rec.Status() != run.StatusQueued {
		return
	}
	s.mu.Lock()
	s.removeQueuedLocked(rec)
	s.updateGaugesLocked()
	s.mu.Unlock()

	res := &run.Result{
		Status: run.StatusQueueTimeout,
		Reason: fmt.Sprintf("等待执行超过 %d 秒，已自动取消排队，代码仍保留在编辑器中", s.cfg.Limits.QueueWaitTimeoutSec),
		Runner: s.runner.Name(),
	}
	if rec.Finish(res, time.Now()) {
		s.metrics.Runs().Inc(string(res.Status))
	}
	s.limiter.Release(rec.SourceIP)
}

// finishAs stores a terminal result if the record is still live.
func (s *Scheduler) finishAs(rec *run.Record, res *run.Result) {
	rec.Finish(res, time.Now())
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

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("sched: entropy unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}
