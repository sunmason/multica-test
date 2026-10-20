// Package run defines the shared vocabulary of a sandboxed execution:
// the full user-visible status enum, results, and the thread-safe record
// the scheduler keeps for every submission.
package run

import (
	"fmt"
	"sync"
	"time"
)

// Status is the complete, user-visible run lifecycle. The doc (LUOT-4 V2,
// section 四 B1 / 六) requires these to stay distinct so the UI can tell
// "my code broke" from "the sandbox stopped me".
type Status string

const (
	StatusQueued         Status = "queued"
	StatusRunning        Status = "running"
	StatusSuccess        Status = "success"
	StatusCompileFailed  Status = "compile_failed"
	StatusRunFailed      Status = "run_failed"
	StatusTimeout        Status = "timeout"
	StatusMemoryExceeded Status = "memory_exceeded"
	StatusQueueTimeout   Status = "queue_timeout"
	StatusRejected       Status = "rejected"
	StatusCancelled      Status = "cancelled" // manual stop (B4)
)

// Terminal reports whether the status ends the run's lifecycle.
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusCompileFailed, StatusRunFailed, StatusTimeout,
		StatusMemoryExceeded, StatusQueueTimeout, StatusRejected, StatusCancelled:
		return true
	}
	return false
}

// CompileError is one located diagnostics entry for B2: the frontend renders
// the message and jumps to Line in the editor on click.
type CompileError struct {
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
	Text    string `json:"text"` // raw compiler line, path already normalized
}

// Result is the terminal payload of one sandboxed execution.
type Result struct {
	Status        Status         `json:"status"`
	Reason        string         `json:"reason,omitempty"` // one-sentence human explanation per section 六
	Stdout        string         `json:"stdout"`
	Stderr        string         `json:"stderr"`
	CompileErrors []CompileError `json:"compile_errors,omitempty"`
	ExitCode      int            `json:"exit_code,omitempty"`
	DurationMS    int64          `json:"duration_ms"` // slot-held time (compile + run)
	Truncated     bool           `json:"truncated,omitempty"`
	DroppedBytes  int64          `json:"dropped_bytes,omitempty"`
	Runner        string         `json:"runner"` // docker | local — so the UI can label the execution mode
}

// Request is a validated submission.
type Request struct {
	Code  string
	Stdin string
}

// Record is the scheduler-owned state of one submission. All reads and
// writes go through Snapshot/mutators so HTTP handlers never race the
// executor goroutine.
type Record struct {
	ID        string
	CreatedAt time.Time
	SourceIP  string

	mu        sync.Mutex
	status    Status
	payload   Request // immutable submission carried until dequeue
	queuePos  int     // 1-based position while queued; 0 once running/terminal
	estWaitMS int64   // scheduler-refreshed estimate while queued
	result    *Result
	updatedAt time.Time
}

// NewRecord creates a record in queued state carrying its payload.
func NewRecord(id, sourceIP string, payload Request, now time.Time) *Record {
	return &Record{ID: id, CreatedAt: now, SourceIP: sourceIP, payload: payload, status: StatusQueued, updatedAt: now}
}

// Payload returns the immutable submission.
func (r *Record) Payload() Request {
	return r.payload
}

// UpdatedAt is the last terminal transition (or creation time if still live).
func (r *Record) UpdatedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updatedAt
}

// Snapshot returns a consistent view of the record.
func (r *Record) Snapshot() (status Status, queuePos int, estWaitMS int64, result *Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.queuePos, r.estWaitMS, r.result
}

// Status returns the current status.
func (r *Record) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// SetQueued marks the run as waiting at a 1-based position.
func (r *Record) SetQueued(pos int, estWaitMS int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = StatusQueued
	r.queuePos = pos
	r.estWaitMS = estWaitMS
}

// SetRunning marks the run as holding a slot. Returns false (a no-op) when
// the record already went terminal — the queue-timeout timer may have fired
// while the run sat at the queue head.
func (r *Record) SetRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Terminal() {
		return false
	}
	r.status = StatusRunning
	r.queuePos = 0
	r.estWaitMS = 0
	return true
}

// Finish stores the terminal result. Returns false when the record was
// already terminal (a late executor result must never overwrite a cancel).
func (r *Record) Finish(res *Result, now time.Time) bool {
	if !res.Status.Terminal() {
		panic(fmt.Sprintf("run: Finish with non-terminal status %q", res.Status))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Terminal() {
		return false
	}
	r.status = res.Status
	r.result = res
	r.queuePos = 0
	r.estWaitMS = 0
	r.updatedAt = now
	return true
}
