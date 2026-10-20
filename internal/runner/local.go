package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/run"
)

// Local executes submissions as plain host processes. It exists so the
// platform runs (and its tests pass) on machines without Docker.
//
// Honest security posture, surfaced via /api/health and the UI banner:
//   - NO network isolation,
//   - memory is NOT enforced on darwin (no cgroups; RLIMIT_AS breaks the Go
//     runtime's address-space reservation),
//   - pid cap applies only on Linux.
//
// Wall-clock timeout, output caps, temp-dir wipe and process-group kill are
// enforced everywhere.
type Local struct {
	cfg      config.Config
	workRoot string
}

// NewLocal checks the host Go toolchain and prepares the temp workspace.
func NewLocal(cfg config.Config) (*Local, error) {
	normalizeDurations(&cfg)
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("local runner needs a Go toolchain on PATH: %w", err)
	}
	root := filepath.Join(os.TempDir(), "gsb-local")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create work root: %w", err)
	}
	return &Local{cfg: cfg, workRoot: root}, nil
}

func (l *Local) Name() string { return "local" }

func (l *Local) Probe(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := l.GoVersion(pctx)
	return err
}

// Warmup builds once to populate the host build cache.
func (l *Local) Warmup(ctx context.Context) {
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	start := time.Now()
	res := l.Execute(wctx, "warmup", run.Request{
		Code: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"warmup\") }\n",
	})
	if res.Status == run.StatusSuccess {
		fmt.Printf("[runner] local build cache warmed in %s\n", time.Since(start).Round(time.Millisecond))
	}
}

// Sweep removes temp dirs left behind by crashes (the janitor from section 五).
func (l *Local) Sweep(ctx context.Context) {
	entries, err := os.ReadDir(l.workRoot)
	if err != nil {
		return
	}
	horizon := time.Duration(l.cfg.Limits.ExecTimeoutSec+l.cfg.Limits.CompileTimeoutSec+120) * time.Second
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= horizon {
			continue
		}
		_ = os.RemoveAll(filepath.Join(l.workRoot, e.Name()))
	}
}

func (l *Local) GoVersion(ctx context.Context) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(gctx, "go", "version").Output()
	if err != nil {
		return "", fmt.Errorf("go version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Execute compiles into a fresh temp dir, runs the binary with best-effort
// limits, and wipes the dir. Host residue is zero on every path.
func (l *Local) Execute(ctx context.Context, id string, req run.Request) *run.Result {
	start := time.Now()
	dir, err := os.MkdirTemp(l.workRoot, "run-"+id+"-")
	if err != nil {
		return l.backendFailure(err)
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(req.Code), 0o600); err != nil {
		return l.backendFailure(err)
	}

	// ---- Phase 1: compile ----
	cctx, ccancel := context.WithTimeout(ctx, l.cfg.CompileTimeout)
	defer ccancel()
	cStderr := newCappedBuffer(l.cfg.Limits.OutputCapBytes)
	cCmd := exec.CommandContext(cctx, "go", "build", "-o", filepath.Join(dir, "prog"), src)
	cCmd.Dir = dir
	cCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	cCmd.Stderr = cStderr
	if err := cCmd.Run(); err != nil {
		if ctx.Err() != nil {
			return l.cancelled(start, "", cStderr.String())
		}
		if cctx.Err() == context.DeadlineExceeded {
			return &run.Result{
				Status:     run.StatusCompileFailed,
				Reason:     fmt.Sprintf("编译超过 %d 秒未完成，代码可能过于复杂", l.cfg.Limits.CompileTimeoutSec),
				Stderr:     cStderr.String(),
				DurationMS: ms(time.Since(start)),
				Runner:     l.Name(),
			}
		}
		errs := parseCompileErrors(cStderr.String())
		if len(errs) == 0 {
			return l.backendFailure(fmt.Errorf("compile failed without diagnostics: %v; stderr: %s", err, truncateForLog(cStderr.String())))
		}
		return &run.Result{
			Status:        run.StatusCompileFailed,
			Reason:        "代码未通过编译，请检查第 " + itoa(errs[0].Line) + " 行附近的错误",
			Stderr:        normalizePaths(cStderr.String()),
			CompileErrors: errs,
			DurationMS:    ms(time.Since(start)),
			Runner:        l.Name(),
		}
	}

	// ---- Phase 2: run ----
	rctx, rcancel := context.WithTimeout(ctx, l.cfg.ExecTimeout)
	defer rcancel()
	rStdout := newCappedBuffer(l.cfg.Limits.OutputCapBytes)
	rStderr := newCappedBuffer(l.cfg.Limits.OutputCapBytes)
	rCmd := exec.CommandContext(rctx, filepath.Join(dir, "prog"))
	rCmd.Dir = dir // the only writable path the program gets
	rCmd.Stdin = strings.NewReader(req.Stdin)
	rCmd.Stdout = rStdout
	rCmd.Stderr = rStderr
	rCmd.SysProcAttr = procGroupAttr()
	rCmd.WaitDelay = 5 * time.Second
	// CommandContext kills only the direct child; user code may have spawned
	// children, so on timeout kill the whole process group instead.
	rCmd.Cancel = func() error {
		if rCmd.Process != nil {
			return syscall.Kill(-rCmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	applyLimits(rCmd, l.cfg)
	runErr := rCmd.Run()
	timedOut := isTimeout(rctx) && ctx.Err() == nil

	if runErr != nil && ctx.Err() != nil {
		return l.cancelled(start, rStdout.String(), rStderr.String())
	}

	exitCode := exitCodeOf(rCmd, runErr)
	res := &run.Result{
		Stdout:     rStdout.String(),
		Stderr:     rStderr.String(),
		ExitCode:   exitCode,
		DurationMS: ms(time.Since(start)),
		Runner:     l.Name(),
	}
	res.Truncated, res.DroppedBytes = totalDropped(rStdout, rStderr)

	switch {
	case timedOut:
		res.Status = run.StatusTimeout
		res.Reason = fmt.Sprintf("程序运行超过 %d 秒，已被强制终止", l.cfg.Limits.ExecTimeoutSec)
		if res.Truncated {
			res.Reason += "（输出可能不完整）"
		}
	case exitCode == 0 && runErr == nil:
		res.Status = run.StatusSuccess
	case exitCode == -1:
		return l.backendFailure(fmt.Errorf("run phase failed to start: %v", runErr))
	default:
		res.Status = run.StatusRunFailed
		res.Reason = "程序运行时异常退出（非零退出码），请检查 stderr 中的 panic 或错误输出"
	}
	return res
}

func (l *Local) cancelled(start time.Time, stdout, stderr string) *run.Result {
	return &run.Result{
		Status:     run.StatusCancelled,
		Reason:     "运行已被用户停止",
		Stdout:     stdout,
		Stderr:     stderr,
		DurationMS: ms(time.Since(start)),
		Runner:     l.Name(),
	}
}

func (l *Local) backendFailure(err error) *run.Result {
	return &run.Result{
		Status:   run.StatusRunFailed,
		Reason:   "沙箱内部错误，请稍后重试",
		Runner:   l.Name(),
		ExitCode: -1,
	}
}

// procGroupAttr puts the child in its own process group so a timeout kill
// reaps user-spawned children too.
func procGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
