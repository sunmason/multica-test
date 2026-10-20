// Package limit implements per-source admission control: a token-bucket
// rate limit and a concurrent-submission cap. With only 3 global slots this
// is the only thing stopping one anonymous client from monopolizing the
// platform (requirements section 五/七).
package limit

import (
	"sync"
	"time"
)

// Reason explains why admission was denied.
type Reason string

const (
	ReasonRate Reason = "rate_limited" // over per-source submissions/minute
	ReasonBusy Reason = "source_busy"  // over per-source simultaneous runs
)

// Limiter is safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	rpm     int
	concMax int
	buckets map[string]*bucket
	active  map[string]int
	lastGC  time.Time
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New creates a limiter allowing rpm submissions per rolling minute and
// concMax simultaneous submissions per source.
func New(rpm, concMax int) *Limiter {
	return &Limiter{
		rpm:     rpm,
		concMax: concMax,
		buckets: make(map[string]*bucket),
		active:  make(map[string]int),
		lastGC:  time.Now(),
		now:     time.Now,
	}
}

// Allow checks admission for source; on success the source holds one
// concurrency slot until Release.
func (l *Limiter) Allow(source string) (Reason, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gcLocked(now)

	b, ok := l.buckets[source]
	if !ok {
		b = &bucket{tokens: float64(l.rpm), last: now}
		l.buckets[source] = b
	}
	// Refill continuously: rpm tokens spread across 60s.
	b.tokens += now.Sub(b.last).Seconds() * float64(l.rpm) / 60
	if b.tokens > float64(l.rpm) {
		b.tokens = float64(l.rpm)
	}
	b.last = now
	if b.tokens < 1 {
		return ReasonRate, false
	}
	if l.active[source] >= l.concMax {
		return ReasonBusy, false
	}
	b.tokens--
	l.active[source]++
	return "", true
}

// Release returns the concurrency slot held by source.
func (l *Limiter) Release(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[source] > 0 {
		l.active[source]--
	}
	if l.active[source] == 0 {
		delete(l.active, source)
	}
}

// gcLocked drops idle buckets so long-tailed source IPs don't accumulate.
func (l *Limiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	for src, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, src)
		}
	}
}
