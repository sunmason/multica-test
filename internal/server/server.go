// Package server is the HTTP surface: run submission/polling/cancellation,
// formatting, health and metrics. It is deliberately thin — all policy
// lives in sched/config, all sandbox mechanics in runner.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/metrics"
	"golang-sandbox/internal/run"
	"golang-sandbox/internal/sched"
	"golang-sandbox/web"
)

const maxBodyBytes = 256 << 10 // hard request cap; code/stdin caps checked separately

var runIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Server holds the HTTP dependencies.
type Server struct {
	cfg     config.Config
	sched   *sched.Scheduler
	metrics *metrics.Registry
	log     *slog.Logger
	started time.Time
	healthy atomic.Bool
	goVer   atomic.Value // string
	mux     *http.ServeMux
}

// New builds the handler tree.
func New(cfg config.Config, sc *sched.Scheduler, m *metrics.Registry, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, sched: sc, metrics: m, log: log, started: time.Now(), mux: http.NewServeMux()}
	s.goVer.Store("")
	s.healthy.Store(true)

	s.mux.HandleFunc("POST /api/run", s.handleRun)
	s.mux.HandleFunc("GET /api/run/{id}", s.handlePoll)
	s.mux.HandleFunc("DELETE /api/run/{id}", s.handleCancel)
	s.mux.HandleFunc("POST /api/format", s.handleFormat)
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/meta", s.handleMeta)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.Handle("GET /", http.FileServer(http.FS(web.Files)))
	return s
}

// Handler returns the fully-wrapped root handler.
func (s *Server) Handler() http.Handler {
	return s.accessLog(s.securityHeaders(s.mux))
}

// SetGoVersion caches the sandbox toolchain version for /api/meta & health.
func (s *Server) SetGoVersion(v string) { s.goVer.Store(v) }

// StartGoVersionRefresh re-reads the sandbox Go version periodically (the
// docker image can be upgraded under a running server).
func (s *Server) StartGoVersionRefresh(ctx context.Context, r runnerVersionProbe) {
	fetch := func() {
		tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if v, err := r.GoVersion(tctx); err == nil {
			s.SetGoVersion(v)
		}
		cancel()
	}
	fetch()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fetch()
			}
		}
	}()
}

// runnerVersionProbe is the slice of runner.Runner this package needs.
type runnerVersionProbe interface {
	GoVersion(ctx context.Context) (string, error)
}

// ---- handlers ----

type runRequest struct {
	Code  string `json:"code"`
	Stdin string `json:"stdin"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req runRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"status": run.StatusRejected, "reason_code": "bad_request",
			"reason": "请求格式错误：" + briefError(err),
		})
		return
	}

	reject := func(code, human string, httpStatus int) {
		s.metrics.Rejects().Inc(code)
		s.writeJSON(w, httpStatus, map[string]any{
			"status": run.StatusRejected, "reason_code": code, "reason": human,
		})
	}
	if strings.TrimSpace(req.Code) == "" {
		reject("empty_code", "代码为空，写点什么再运行吧", http.StatusBadRequest)
		return
	}
	if len(req.Code) > s.cfg.Limits.CodeMaxBytes {
		reject("code_too_large", fmt.Sprintf("代码超过 %dKB 上限", s.cfg.Limits.CodeMaxBytes>>10), http.StatusRequestEntityTooLarge)
		return
	}
	if !utf8.ValidString(req.Code) {
		reject("invalid_encoding", "代码必须是有效的 UTF-8 文本", http.StatusBadRequest)
		return
	}
	if len(req.Stdin) > s.cfg.Limits.StdinMaxBytes {
		reject("stdin_too_large", fmt.Sprintf("标准输入超过 %dKB 上限", s.cfg.Limits.StdinMaxBytes>>10), http.StatusRequestEntityTooLarge)
		return
	}

	rec, reason, ok := s.sched.Submit(s.sourceIP(r), run.Request{Code: req.Code, Stdin: req.Stdin})
	if !ok {
		switch reason {
		case sched.RejectRateLimited:
			reject(reason, "操作过于频繁，请稍后再试", http.StatusTooManyRequests)
		case sched.RejectSourceBusy:
			reject(reason, "同一时间只能运行一个程序，请等待当前运行结束或先停止它", http.StatusTooManyRequests)
		default:
			reject(reason, "当前排队已满，请稍后再试", http.StatusServiceUnavailable)
		}
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{"run_id": rec.ID, "status": rec.Status()})
}

type pollView struct {
	RunID         string             `json:"run_id"`
	Status        run.Status         `json:"status"`
	QueuePosition int                `json:"queue_position,omitempty"`
	EstimatedWait int64              `json:"estimated_wait_ms,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	Stdout        string             `json:"stdout,omitempty"`
	Stderr        string             `json:"stderr,omitempty"`
	CompileErrors []run.CompileError `json:"compile_errors,omitempty"`
	ExitCode      int                `json:"exit_code,omitempty"`
	DurationMS    int64              `json:"duration_ms,omitempty"`
	Truncated     bool               `json:"truncated,omitempty"`
	DroppedBytes  int64              `json:"dropped_bytes,omitempty"`
	Runner        string             `json:"runner,omitempty"`
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runIDRe.MatchString(id) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_run_id"})
		return
	}
	rec := s.sched.Get(id)
	if rec == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "run_not_found"})
		return
	}
	st, pos, est, res := rec.Snapshot()
	v := pollView{RunID: rec.ID, Status: st, QueuePosition: pos, EstimatedWait: est}
	if res != nil {
		v.Reason = res.Reason
		v.Stdout = res.Stdout
		v.Stderr = res.Stderr
		v.CompileErrors = res.CompileErrors
		v.ExitCode = res.ExitCode
		v.DurationMS = res.DurationMS
		v.Truncated = res.Truncated
		v.DroppedBytes = res.DroppedBytes
		v.Runner = res.Runner
	}
	s.writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runIDRe.MatchString(id) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_run_id"})
		return
	}
	rec := s.sched.Get(id)
	if rec == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "run_not_found"})
		return
	}
	if !s.sched.Cancel(id) {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "not_cancellable", "status": rec.Status()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

type formatRequest struct {
	Code string `json:"code"`
}

type formatResponse struct {
	OK        bool   `json:"ok"`
	Formatted string `json:"formatted,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleFormat gofmts code in-process. This never executes user code —
// format.Source only parses — so no sandbox is needed.
func (s *Server) handleFormat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req formatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, formatResponse{OK: false, Error: "请求格式错误"})
		return
	}
	if len(req.Code) > s.cfg.Limits.CodeMaxBytes {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, formatResponse{OK: false, Error: "代码超过大小上限"})
		return
	}
	out, err := format.Source([]byte(req.Code))
	if err != nil {
		s.writeJSON(w, http.StatusOK, formatResponse{OK: false, Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, formatResponse{OK: true, Formatted: string(out)})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	queued, running := s.sched.Stats()
	ver, _ := s.goVer.Load().(string)
	state := "ok"
	if !s.healthy.Load() {
		state = "degraded"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status":      state,
		"runner":      s.cfg.Runner,
		"go_version":  ver,
		"queued":      queued,
		"running":     running,
		"max_running": s.cfg.Limits.MaxConcurrency,
		"uptime_s":    int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ver, _ := s.goVer.Load().(string)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"go_version":      ver,
		"runner":          s.cfg.Runner,
		"exec_timeout_s":  s.cfg.Limits.ExecTimeoutSec,
		"memory_mb":       s.cfg.Limits.MemoryMB,
		"cpu_cores":       s.cfg.Limits.CPUCores,
		"code_max_kb":     s.cfg.Limits.CodeMaxBytes >> 10,
		"queue_capacity":  s.cfg.Limits.QueueCapacity,
		"max_concurrency": s.cfg.Limits.MaxConcurrency,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.metrics.Render()))
}

// ---- plumbing ----

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// sourceIP extracts the client IP; X-Forwarded-For is honored only when the
// operator declared a trusted proxy in config.
func (s *Server) sourceIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			if i := strings.IndexByte(xf, ','); i > 0 {
				xf = xf[:i]
			}
			return strings.TrimSpace(xf)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/metrics" && r.URL.Path != "/healthz" {
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"source", s.sourceIP(r),
		)
	})
}

func briefError(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "请求体过大"
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}
