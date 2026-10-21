package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/logctx"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
	"golang-sandbox/internal/sched"
)

type requestValueKey struct{}
type httpExecution struct {
	ctx     context.Context
	id      string
	release chan struct{}
}
type httpRunner struct{ calls chan httpExecution }

func (r *httpRunner) Name() string                              { return "controlled" }
func (r *httpRunner) Probe(context.Context) error               { return nil }
func (r *httpRunner) GoVersion(context.Context) (string, error) { return "go test", nil }
func (r *httpRunner) Sweep(context.Context)                     {}
func (r *httpRunner) Warmup(context.Context)                    {}
func (r *httpRunner) Execute(ctx context.Context, id string, _ run.Request) *run.Result {
	call := httpExecution{ctx: ctx, id: id, release: make(chan struct{})}
	r.calls <- call
	select {
	case <-call.release:
		return &run.Result{Status: run.StatusSuccess, Runner: r.Name()}
	case <-ctx.Done():
		return &run.Result{Status: run.StatusCancelled, Runner: r.Name()}
	}
}
func awaitHTTPExecution(t *testing.T, r *httpRunner) httpExecution {
	t.Helper()
	select {
	case call := <-r.calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not receive context")
		return httpExecution{}
	}
}

func TestHTTPContextAndLogsFollowRunAcrossQueuePollAndCancel(t *testing.T) {
	cfg := config.Default()
	cfg.Limits.MaxConcurrency = 1
	cfg.RateLimit.PerSourceRPM = 100
	cfg.RateLimit.PerSourceConcurrent = 10
	cfg.QueueWait = time.Hour
	r := &httpRunner{calls: make(chan httpExecution, 2)}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("owner", "injected")
	m := metrics.New()
	sc := sched.New(cfg, r, m, logger)
	api := New(cfg, sc, m, logger)
	handler := api.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Each request value must survive the HTTP server cancelling its context
		// on return, including when execution starts later from the queue.
		ctx := context.WithValue(req.Context(), requestValueKey{}, req.Header.Get("Test-Value"))
		handler.ServeHTTP(w, req.WithContext(ctx))
	}))
	t.Cleanup(func() { ts.Close(); sc.Shutdown(context.Background()) })
	submit := func(value string) string {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/run", strings.NewReader(`{"code":"package main"}`))
		req.Header.Set("Test-Value", value)
		req.Header.Set("X-Trace-Id", "untrusted")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("submit=%d %v", resp.StatusCode, body)
		}
		return body["run_id"].(string)
	}
	id1 := submit("direct")
	first := awaitHTTPExecution(t, r)
	id2 := submit("queued")
	if sc.Get(context.Background(), id2).Status() != run.StatusQueued {
		t.Fatal("second submission did not queue")
	}
	if first.ctx.Err() != nil {
		t.Fatal("returning 202 cancelled first run")
	}
	close(first.release)
	second := awaitHTTPExecution(t, r)
	if second.ctx.Err() != nil {
		t.Fatal("queued task inherited completed HTTP context cancellation")
	}
	for _, tc := range []struct {
		call      httpExecution
		id, value string
	}{{first, id1, "direct"}, {second, id2, "queued"}} {
		if tc.call.ctx.Value(requestValueKey{}) != tc.value || logctx.TraceID(tc.call.ctx) != tc.id || tc.call.id != tc.id {
			t.Fatal("HTTP to executor lost value or correlation")
		}
	}
	resp, err := http.Get(ts.URL + "/api/run/" + id2)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll=%d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/run/"+id2, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel=%d", resp.StatusCode)
	}
	ts.Close()
	sc.Shutdown(context.Background())
	if second.ctx.Err() != context.Canceled {
		t.Fatal("independent cancel did not reach runner")
	}
	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		id, _ := entry["trace_id"].(string)
		if entry["owner"] != "injected" || (id != id1 && id != id2) {
			t.Fatalf("wrong logger/trace: %v", entry)
		}
		if entry["msg"] == "http" {
			seen[entry["method"].(string)+":"+id]++
		} else {
			if entry["run_id"] != id {
				t.Fatalf("execution log not correlated: %v", entry)
			}
			seen[entry["msg"].(string)+":"+id]++
		}
	}
	for _, key := range []string{"POST:" + id1, "POST:" + id2, "GET:" + id2, "DELETE:" + id2, "run finished:" + id1, "run stopped:" + id2, "run finished:" + id2} {
		if seen[key] != 1 {
			t.Errorf("log %s count=%d, want 1", key, seen[key])
		}
	}
}
