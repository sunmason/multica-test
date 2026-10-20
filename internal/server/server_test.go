package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
	"golang-sandbox/internal/runner"
	"golang-sandbox/internal/sched"
)

func newTestServer(t *testing.T, mutate func(*config.Config)) (*httptest.Server, *metrics.Registry) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ""
	cfg.Limits.MaxConcurrency = 2
	cfg.Limits.QueueCapacity = 2
	cfg.Limits.QueueWaitTimeoutSec = 2
	cfg.Limits.ExecTimeoutSec = 3
	cfg.RateLimit.PerSourceRPM = 100
	cfg.ExecTimeout = 3 * time.Second
	cfg.QueueWait = 2 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.Runner = "local" // what runner.Select would resolve to on this host
	r, err := runner.NewLocal(cfg)
	if err != nil {
		t.Skipf("local runner unavailable: %v", err)
	}
	m := metrics.New()
	sc := sched.New(cfg, r, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := New(cfg, sc, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, m
}

func postJSON(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()
	return resp, out
}

func waitForRun(t *testing.T, baseURL, runID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/run/" + runID)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		var v map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			resp.Body.Close()
			t.Fatalf("decode poll: %v", err)
		}
		resp.Body.Close()
		st, _ := v["status"].(string)
		if st != string(run.StatusQueued) && st != string(run.StatusRunning) {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run never reached a terminal status")
	return nil
}

func TestRunValidationRejects(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"empty code", `{"code":"   "}`, http.StatusBadRequest, "empty_code"},
		{"bad json", `{code}`, http.StatusBadRequest, "bad_request"},
		{"code too large", `{"code":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusRequestEntityTooLarge, "code_too_large"},
		{"stdin too large", `{"code":"package main","stdin":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusRequestEntityTooLarge, "stdin_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := postJSON(t, ts.URL+"/api/run", tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("http = %d, want %d (%v)", resp.StatusCode, tc.wantStatus, out)
			}
			if out["reason_code"] != tc.wantCode {
				t.Errorf("reason_code = %v, want %v", out["reason_code"], tc.wantCode)
			}
			if out["status"] != "rejected" {
				t.Errorf("status = %v, want rejected", out["status"])
			}
		})
	}
}

func TestFullRunLifecycleOverHTTP(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, out := postJSON(t, ts.URL+"/api/run",
		`{"code":"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"e2e\") }\n","stdin":""}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit http = %d: %v", resp.StatusCode, out)
	}
	runID, _ := out["run_id"].(string)
	if len(runID) != 32 {
		t.Fatalf("run_id = %q", runID)
	}

	v := waitForRun(t, ts.URL, runID)
	if v["status"] != "success" || v["stdout"] != "e2e\n" {
		t.Errorf("final view = %v", v)
	}

	// Cancelling a finished run must be a clean 409, not an error page.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/run/"+runID, nil)
	cResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusConflict {
		t.Errorf("cancel terminal http = %d, want 409", cResp.StatusCode)
	}
}

func TestCompileErrorViewOverHTTP(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, out := postJSON(t, ts.URL+"/api/run", `{"code":"package main\n\nfunc main() {\n\tnope()\n}\n"}`)
	v := waitForRun(t, ts.URL, out["run_id"].(string))
	if v["status"] != "compile_failed" {
		t.Fatalf("status = %v", v["status"])
	}
	errs, ok := v["compile_errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("compile_errors missing: %v", v)
	}
	first := errs[0].(map[string]any)
	if first["line"] != float64(4) {
		t.Errorf("first error line = %v, want 4", first["line"])
	}
}

func TestTimeoutOverHTTP(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, out := postJSON(t, ts.URL+"/api/run", `{"code":"package main\n\nfunc main() { for {} }\n"}`)
	v := waitForRun(t, ts.URL, out["run_id"].(string))
	if v["status"] != "timeout" {
		t.Fatalf("status = %v, want timeout", v["status"])
	}
	if v["reason"] == "" {
		t.Error("timeout must carry a human reason")
	}
}

func TestFormatEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	resp, out := postJSON(t, ts.URL+"/api/format", `{"code":"package main\nfunc main(){\nfmt.Println(1)}\n"}`)
	if resp.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("format ok: %d %v", resp.StatusCode, out)
	}
	if f, _ := out["formatted"].(string); !strings.Contains(f, "func main() {") {
		t.Errorf("gofmt output not normalized: %q", f)
	}

	_, out = postJSON(t, ts.URL+"/api/format", `{"code":"package main\nfunc main({\n"}`)
	if out["ok"] != false || out["error"] == "" {
		t.Errorf("format of broken code: %v", out)
	}
}

func TestPollGuards(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := http.Get(ts.URL + "/api/run/NOTANID")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid id = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/api/run/0123456789abcdef0123456789abcdef")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMetaAndHealth(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := http.Get(ts.URL + "/api/meta")
	var meta map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if meta["max_concurrency"] != float64(2) || meta["memory_mb"] != float64(256) {
		t.Errorf("meta = %v", meta)
	}

	resp, _ = http.Get(ts.URL + "/api/health")
	var h map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if h["status"] != "ok" || h["runner"] != "local" {
		t.Errorf("health = %v", h)
	}
}

func TestMetricsExposed(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "gosandbox_queue_depth") {
		t.Errorf("metrics missing queue depth:\n%s", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("missing CSP on static content")
	}
}

func TestSourceIPTieldsForwardedFor(t *testing.T) {
	cfg := config.Default()
	cfg.TrustProxy = true
	s := &Server{cfg: cfg}
	r := httptest.NewRequest(http.MethodPost, "/api/run", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.2")
	if got := s.sourceIP(r); got != "203.0.113.7" {
		t.Errorf("sourceIP = %q, want 203.0.113.7", got)
	}
	cfg2 := config.Default() // TrustProxy false
	s2 := &Server{cfg: cfg2}
	if got := s2.sourceIP(r); got != "10.0.0.1" {
		t.Errorf("sourceIP without proxy trust = %q, want RemoteAddr host", got)
	}
}
