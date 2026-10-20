package run

import (
	"testing"
	"time"
)

func TestRecordFinishIsTerminalAndIdempotent(t *testing.T) {
	rec := NewRecord("a", "ip", Request{}, time.Now())
	if rec.Status() != StatusQueued {
		t.Fatalf("initial status = %s", rec.Status())
	}
	res := &Result{Status: StatusSuccess, Stdout: "hi"}
	if !rec.Finish(res, time.Now()) {
		t.Fatal("first Finish must win")
	}
	if rec.Finish(&Result{Status: StatusTimeout}, time.Now()) {
		t.Error("second Finish must be rejected")
	}
	if _, _, _, got := rec.Snapshot(); got.Status != StatusSuccess || got.Stdout != "hi" {
		t.Errorf("result corrupted by late Finish: %+v", got)
	}
}

func TestSetRunningRefusesTerminal(t *testing.T) {
	rec := NewRecord("b", "ip", Request{}, time.Now())
	rec.Finish(&Result{Status: StatusQueueTimeout}, time.Now())
	if rec.SetRunning() {
		t.Error("SetRunning must refuse a queue-timed-out record")
	}
	if rec.Status() != StatusQueueTimeout {
		t.Errorf("status changed to %s", rec.Status())
	}
}

func TestSetRunningClearsQueueInfo(t *testing.T) {
	rec := NewRecord("c", "ip", Request{}, time.Now())
	rec.SetQueued(2, 4000)
	st, pos, est, _ := rec.Snapshot()
	if st != StatusQueued || pos != 2 || est != 4000 {
		t.Fatalf("queued view wrong: %s %d %d", st, pos, est)
	}
	rec.SetRunning()
	_, pos, est, _ = rec.Snapshot()
	if pos != 0 || est != 0 {
		t.Errorf("running record still reports queue data: pos=%d est=%d", pos, est)
	}
}

func TestTerminalCoversAllFinalStates(t *testing.T) {
	for _, s := range []Status{StatusSuccess, StatusCompileFailed, StatusRunFailed, StatusTimeout,
		StatusMemoryExceeded, StatusQueueTimeout, StatusRejected, StatusCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []Status{StatusQueued, StatusRunning} {
		if s.Terminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}
