// Package runner hides sandbox execution mechanics behind the Runner
// interface. Two implementations exist:
//
//   - Docker: two-phase container execution (compile, then run) with the
//     confirmed hard limits — 1 CPU, 256MB, no network, read-only rootfs,
//     non-root, pid cap. Production path.
//   - Local: plain host processes with best-effort rlimits. Development
//     fallback for hosts without Docker; it CANNOT isolate the network and
//     is labelled as such everywhere it surfaces.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/run"
)

// Runner executes single-file Go programs under sandbox limits.
// Implementations must be safe for concurrent use.
type Runner interface {
	// Name identifies the backend ("docker" / "local") in API payloads.
	Name() string
	// Probe reports whether the backend is usable right now.
	Probe(ctx context.Context) error
	// Compile+run one submission. Always returns a terminal Result; it
	// never returns a non-nil error alongside a Result — errors mean the
	// backend itself failed before any classification was possible.
	Execute(ctx context.Context, id string, req run.Request) *run.Result
	// GoVersion returns the toolchain version user code runs against (E3).
	GoVersion(ctx context.Context) (string, error)
	// Warmup pre-builds the toolchain cache so the first user run is fast.
	Warmup(ctx context.Context)
	// Sweep force-removes containers/temp dirs orphaned by crashes.
	Sweep(ctx context.Context)
}

// Select resolves the configured runner choice; "auto" prefers Docker and
// falls back to Local when the daemon is unreachable.
func Select(ctx context.Context, cfg config.Config) (Runner, error) {
	if cfg.Runner == "local" {
		return NewLocal(cfg)
	}
	d, err := NewDocker(cfg)
	if err != nil {
		if cfg.Runner == "docker" {
			return nil, fmt.Errorf("docker runner forced but unavailable: %w", err)
		}
		return NewLocal(cfg)
	}
	if err := d.Probe(ctx); err != nil {
		if cfg.Runner == "docker" {
			return nil, fmt.Errorf("docker runner forced but probe failed: %w", err)
		}
		return NewLocal(cfg)
	}
	return d, nil
}

// cappedBuffer collects an output stream up to cap bytes, counting anything
// beyond it (D4: truncate and report dropped bytes instead of buffering a
// stream bomb).
type cappedBuffer struct {
	buf     bytes.Buffer
	cap     int
	dropped int64
}

func newCappedBuffer(capBytes int) *cappedBuffer {
	return &cappedBuffer{cap: capBytes}
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.cap - w.buf.Len(); room > 0 {
		if room >= len(p) {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
			w.dropped += int64(len(p) - room)
		}
	} else {
		w.dropped += int64(n)
	}
	return n, nil
}

func (w *cappedBuffer) String() string { return w.buf.String() }

var compileLineRe = regexp.MustCompile(`^(?:.*/)?main\.go:(\d+):(\d+):\s?(.*)$`)

// parseCompileErrors extracts located diagnostics from `go build` stderr and
// normalizes the source path to "main.go" so no sandbox path leaks (section
// 六: never expose sandbox internals).
func parseCompileErrors(stderr string) []run.CompileError {
	var out []run.CompileError
	for _, line := range splitLines(stderr) {
		m := compileLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNo, _ := strconv.Atoi(m[1])
		colNo, _ := strconv.Atoi(m[2])
		out = append(out, run.CompileError{
			Line:    lineNo,
			Col:     colNo,
			Message: m[3],
			Text:    "main.go:" + m[1] + ":" + m[2] + ": " + m[3],
		})
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			end := i
			if end > start && s[end-1] == '\r' {
				end--
			}
			lines = append(lines, s[start:end])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// totalDropped sums dropped bytes across both streams.
func totalDropped(out, errOut *cappedBuffer) (bool, int64) {
	dropped := out.dropped + errOut.dropped
	return dropped > 0, dropped
}
