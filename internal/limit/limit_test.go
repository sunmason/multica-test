package limit

import (
	"testing"
	"time"
)

func TestRateLimitRefusesOverRPM(t *testing.T) {
	l := New(2, 5)
	for i := 0; i < 2; i++ {
		if reason, ok := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("call %d rejected: %s", i, reason)
		}
		l.Release("1.2.3.4")
	}
	if reason, ok := l.Allow("1.2.3.4"); ok {
		t.Errorf("3rd call within window should be rate limited, got ok (reason=%q)", reason)
	}
	// Another source is unaffected.
	if _, ok := l.Allow("5.6.7.8"); !ok {
		t.Error("independent source must not be limited")
	}
}

func TestConcurrentLimitPerSource(t *testing.T) {
	l := New(100, 1)
	if _, ok := l.Allow("a"); !ok {
		t.Fatal("first allow failed")
	}
	if reason, ok := l.Allow("a"); ok || reason != ReasonBusy {
		t.Errorf("second concurrent allow: ok=%v reason=%s, want busy", ok, reason)
	}
	if _, ok := l.Allow("b"); !ok {
		t.Error("other source must be allowed")
	}
	l.Release("a")
	if _, ok := l.Allow("a"); !ok {
		t.Error("after release the source must be allowed again")
	}
}

func TestRefillsOverTime(t *testing.T) {
	// concMax deliberately huge: this test must exercise the token bucket,
	// not the concurrent cap (calls below never Release).
	l := New(60, 1000)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 60; i++ {
		if _, ok := l.Allow("x"); !ok {
			t.Fatalf("call %d rejected unexpectedly", i)
		}
	}
	if _, ok := l.Allow("x"); ok {
		t.Fatal("bucket should be drained")
	}
	now = now.Add(time.Second) // 60/min => 1 token per second
	if _, ok := l.Allow("x"); !ok {
		t.Error("token should refill after 1s")
	}
}

func TestReleaseIgnoresUnknownSource(t *testing.T) {
	l := New(10, 1)
	l.Release("ghost") // must not panic or underflow
	if _, ok := l.Allow("ghost"); !ok {
		t.Error("release of unknown source must not consume budget")
	}
}
