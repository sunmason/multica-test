package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/run"
)

const (
	compileContainerPrefix = "gsb-compile-"
	runContainerPrefix     = "gsb-run-"
	outVolumePrefix        = "gsb-out-"
	cacheVolume            = "gsb-gocache"
	dockerOpTimeout        = 30 * time.Second // management calls (volume create/rm, inspect)
)

// Docker runs each submission in ephemeral containers:
//
//	compile: full toolchain image, shared read-mostly build cache volume,
//	         writes the static binary into a per-run volume.
//	run:     executes that binary read-only as uid 65534 with the confirmed
//	         hard limits (1 CPU / 256MB / no network / read-only rootfs /
//	         pid cap / 16MB tmpfs). Nothing is left on host or in Docker
//	         after the run: containers are removed, the per-run volume is
//	         deleted, only the shared build cache survives (toolchain data,
//	         never user data).
type Docker struct {
	cfg config.Config
}

// NewDocker returns a Docker runner; it does not touch the daemon — call
// Probe for that.
func NewDocker(cfg config.Config) (*Docker, error) {
	normalizeDurations(&cfg)
	return &Docker{cfg: cfg}, nil
}

// normalizeDurations fills derived durations so a hand-built Config (tests)
// cannot produce an instantly-expiring context.
func normalizeDurations(cfg *config.Config) {
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 10 * time.Second
	}
	if cfg.CompileTimeout <= 0 {
		cfg.CompileTimeout = 20 * time.Second
	}
}

func (d *Docker) Name() string { return "docker" }

// dockerExec runs a management docker command (volume create, inspect, ...)
// and returns combined output.
func (d *Docker) dockerExec(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Probe verifies the daemon, the toolchain image, and the shared cache volume.
func (d *Docker) Probe(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, dockerOpTimeout)
	defer cancel()
	if _, err := d.dockerExec(pctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	if _, err := d.dockerExec(pctx, "image", "inspect", d.cfg.DockerImage); err != nil {
		return fmt.Errorf("toolchain image %s missing (pull it first): %w", d.cfg.DockerImage, err)
	}
	if _, err := d.dockerExec(pctx, "volume", "inspect", cacheVolume); err != nil {
		if _, cerr := d.dockerExec(pctx, "volume", "create", cacheVolume); cerr != nil {
			return fmt.Errorf("create build-cache volume: %w", cerr)
		}
	}
	return nil
}

// Warmup builds a hello world once so the first real submission does not
// pay the cold stdlib compile (which can take tens of seconds).
func (d *Docker) Warmup(ctx context.Context) {
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	start := time.Now()
	res := d.Execute(wctx, "warmup", run.Request{
		Code: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"warmup\") }\n",
	})
	if res.Status == run.StatusSuccess {
		fmt.Printf("[runner] docker build cache warmed in %s\n", time.Since(start).Round(time.Millisecond))
	} else {
		fmt.Printf("[runner] docker warmup failed: status=%s reason=%s\n", res.Status, res.Reason)
	}
}

// Sweep kills and removes any gsb-* container or per-run volume older than
// the run horizon — the orphan janitor required by section 五. The shared
// build cache volume is never touched.
func (d *Docker) Sweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, dockerOpTimeout)
	defer cancel()
	horizon := time.Duration(d.cfg.Limits.ExecTimeoutSec+d.cfg.Limits.CompileTimeoutSec+120) * time.Second

	out, err := d.dockerExec(sctx, "ps", "-a", "--filter", "name=gsb-", "--format", "{{.Names}}")
	if err == nil {
		for _, name := range strings.Fields(out) {
			if created, terr := d.containerAge(sctx, name); terr == nil && time.Since(created) > horizon {
				_, _ = d.dockerExec(sctx, "rm", "-f", name)
			} else if terr != nil {
				_, _ = d.dockerExec(sctx, "rm", "-f", name) // uninspectable: treat as orphan
			}
		}
	}
	vols, err := d.dockerExec(sctx, "volume", "ls", "--filter", "name="+outVolumePrefix, "--format", "{{.Name}}")
	if err == nil {
		for _, v := range strings.Fields(vols) {
			if strings.HasPrefix(v, cacheVolume) {
				continue
			}
			_, _ = d.dockerExec(sctx, "volume", "rm", v)
		}
	}
}

func (d *Docker) containerAge(ctx context.Context, name string) (time.Time, error) {
	out, err := d.dockerExec(ctx, "inspect", "-f", "{{.Created}}", name)
	if err != nil {
		return time.Time{}, err
	}
	created, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if err != nil {
		return time.Time{}, err
	}
	return created, nil
}

// GoVersion asks the toolchain image once and caches the answer (E3).
func (d *Docker) GoVersion(ctx context.Context) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, dockerOpTimeout)
	defer cancel()
	out, err := d.dockerExec(gctx, "run", "--rm", "--network", "none", d.cfg.DockerImage, "go", "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Execute runs the two-phase pipeline. All teardown paths remove both
// containers and the per-run volume.
func (d *Docker) Execute(ctx context.Context, id string, req run.Request) *run.Result {
	start := time.Now()

	// Lifecycle: volume removed last, run container next, compile first.
	defer func() { _, _ = d.dockerExec(context.Background(), "volume", "rm", outVolumePrefix+id) }()
	defer func() { _, _ = d.dockerExec(context.Background(), "rm", "-f", runContainerPrefix+id) }()
	defer func() { _, _ = d.dockerExec(context.Background(), "rm", "-f", compileContainerPrefix+id) }()

	vctx, vcancel := context.WithTimeout(ctx, dockerOpTimeout)
	_, err := d.dockerExec(vctx, "volume", "create", outVolumePrefix+id)
	vcancel()
	if err != nil {
		return d.backendFailure(err)
	}

	// ---- Phase 1: compile ----
	cctx, ccancel := context.WithTimeout(ctx, d.cfg.CompileTimeout)
	cStdout := newCappedBuffer(d.cfg.Limits.OutputCapBytes)
	cStderr := newCappedBuffer(d.cfg.Limits.OutputCapBytes)
	cCmd := d.compileCommand(cctx, id)
	cCmd.Stdin = strings.NewReader(req.Code)
	cCmd.Stdout = cStdout
	cCmd.Stderr = cStderr
	if err := cCmd.Run(); err != nil {
		if ctx.Err() != nil {
			ccancel()
			return d.cancelled(start, cStdout, cStderr)
		}
		if cctx.Err() == context.DeadlineExceeded {
			ccancel()
			return &run.Result{
				Status:     run.StatusCompileFailed,
				Reason:     fmt.Sprintf("编译超过 %d 秒未完成，代码可能过于复杂", d.cfg.Limits.CompileTimeoutSec),
				Stdout:     cStdout.String(),
				Stderr:     cStderr.String(),
				DurationMS: ms(time.Since(start)),
				Runner:     d.Name(),
			}
		}
		ccancel()
		errs := parseCompileErrors(cStderr.String())
		if len(errs) == 0 {
			return d.backendFailure(fmt.Errorf("compile phase failed without diagnostics: %v; stderr: %s", err, truncateForLog(cStderr.String())))
		}
		return &run.Result{
			Status:        run.StatusCompileFailed,
			Reason:        "代码未通过编译，请检查第 " + itoa(errs[0].Line) + " 行附近的错误",
			Stdout:        cStdout.String(),
			Stderr:        normalizePaths(cStderr.String()),
			CompileErrors: errs,
			DurationMS:    ms(time.Since(start)),
			Runner:        d.Name(),
		}
	}
	ccancel()

	// ---- Phase 2: run ----
	rctx, rcancel := context.WithTimeout(ctx, d.cfg.ExecTimeout)
	rStdout := newCappedBuffer(d.cfg.Limits.OutputCapBytes)
	rStderr := newCappedBuffer(d.cfg.Limits.OutputCapBytes)
	rCmd := d.runCommand(rctx, id)
	rCmd.Stdin = strings.NewReader(req.Stdin)
	rCmd.Stdout = rStdout
	rCmd.Stderr = rStderr
	runErr := rCmd.Run()
	timedOut := isTimeout(rctx) && ctx.Err() == nil
	rcancel()

	if runErr != nil && ctx.Err() != nil {
		return d.cancelled(start, rStdout, rStderr)
	}

	exitCode := exitCodeOf(rCmd, runErr)
	// Inspect before removal to learn whether the kernel OOM-killed the run.
	oom := d.inspectOOM(context.Background(), runContainerPrefix+id)

	res := &run.Result{
		Stdout:     rStdout.String(),
		Stderr:     rStderr.String(),
		ExitCode:   exitCode,
		DurationMS: ms(time.Since(start)),
		Runner:     d.Name(),
	}
	res.Truncated, res.DroppedBytes = totalDropped(rStdout, rStderr)

	switch {
	case oom:
		res.Status = run.StatusMemoryExceeded
		res.Reason = fmt.Sprintf("程序使用内存超过 %dMB 限制，已被终止", d.cfg.Limits.MemoryMB)
	case timedOut:
		res.Status = run.StatusTimeout
		res.Reason = fmt.Sprintf("程序运行超过 %d 秒，已被强制终止", d.cfg.Limits.ExecTimeoutSec)
		if res.Truncated {
			res.Reason += "（输出可能不完整）"
		}
	case exitCode == 0 && runErr == nil:
		res.Status = run.StatusSuccess
	case exitCode == -1:
		return d.backendFailure(fmt.Errorf("run phase failed to start: %v", runErr))
	default:
		res.Status = run.StatusRunFailed
		res.Reason = "程序运行时异常退出（非零退出码），请检查 stderr 中的 panic 或错误输出"
	}
	return res
}

func (d *Docker) cancelled(start time.Time, out, errOut *cappedBuffer) *run.Result {
	res := &run.Result{
		Status:     run.StatusCancelled,
		Reason:     "运行已被用户停止",
		Stdout:     out.String(),
		Stderr:     errOut.String(),
		DurationMS: ms(time.Since(start)),
		Runner:     d.Name(),
	}
	res.Truncated, res.DroppedBytes = totalDropped(out, errOut)
	return res
}

func (d *Docker) backendFailure(err error) *run.Result {
	return &run.Result{
		Status:     run.StatusRunFailed,
		Reason:     "沙箱内部错误，请稍后重试",
		Stderr:     "",
		DurationMS: 0,
		Runner:     d.Name(),
		ExitCode:   -1,
	}
}

// compileCommand pipes the source in via stdin (no host bind mounts): the
// container reads it into its private tmpfs, builds against the shared
// cache volume, and drops the binary into the per-run volume.
func (d *Docker) compileCommand(ctx context.Context, id string) *exec.Cmd {
	args := []string{
		"run", "-i",
		"--name", compileContainerPrefix + id,
		"--network", "none",
		"--cpus", fmt.Sprintf("%g", d.cfg.Limits.CPUCores),
		"--memory", fmt.Sprintf("%dm", d.cfg.Limits.CompileMemoryMB),
		"--memory-swap", fmt.Sprintf("%dm", d.cfg.Limits.CompileMemoryMB), // no swap
		"--pids-limit", fmt.Sprint(d.cfg.Limits.PidsLimit),
		"--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:size=%dm,noexec,nosuid", d.cfg.Limits.TmpfsMB),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"-e", "CGO_ENABLED=0",
		"-e", "GOCACHE=/gocache",
		"-e", "GOTMPDIR=/out", // cold stdlib builds exceed the 16MB /tmp tmpfs
		"-e", "HOME=/tmp",
		"-v", cacheVolume + ":/gocache",
		"-v", outVolumePrefix + id + ":/out",
		d.cfg.DockerImage,
		"sh", "-c", "cat > /tmp/main.go && go build -o /out/prog /tmp/main.go",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// runCommand executes the compiled binary as uid 65534 with the confirmed
// hard limits. The binary and source are mounted read-only; the only
// writable path is the size-capped noexec tmpfs.
func (d *Docker) runCommand(ctx context.Context, id string) *exec.Cmd {
	args := []string{
		"run", "-i",
		"--name", runContainerPrefix + id,
		"--network", "none",
		"--cpus", fmt.Sprintf("%g", d.cfg.Limits.CPUCores),
		"--memory", fmt.Sprintf("%dm", d.cfg.Limits.MemoryMB),
		"--memory-swap", fmt.Sprintf("%dm", d.cfg.Limits.MemoryMB), // no swap
		"--pids-limit", fmt.Sprint(d.cfg.Limits.PidsLimit),
		"--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:size=%dm,noexec,nosuid", d.cfg.Limits.TmpfsMB),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"-v", outVolumePrefix + id + ":/out:ro",
		d.cfg.DockerImage,
		"/out/prog",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// inspectOOM reports the container's OOMKilled flag. Must run before the
// container is removed; on any inspect error it conservatively reports false
// (timeout/exit classification still applies).
func (d *Docker) inspectOOM(ctx context.Context, name string) bool {
	ictx, cancel := context.WithTimeout(ctx, dockerOpTimeout)
	defer cancel()
	out, err := d.dockerExec(ictx, "inspect", name)
	if err != nil {
		return false
	}
	var states []struct {
		State struct {
			OOMKilled bool `json:"OOMKilled"`
		} `json:"State"`
	}
	if json.Unmarshal([]byte(out), &states) != nil || len(states) == 0 {
		return false
	}
	return states[0].State.OOMKilled
}

// isTimeout reports whether ctx expired exactly at its deadline (as opposed
// to the parent context being cancelled).
func isTimeout(ctx context.Context) bool {
	return ctx.Err() == context.DeadlineExceeded
}

func exitCodeOf(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// normalizePaths rewrites sandbox paths to main.go in raw stderr so no
// container layout leaks to the user.
func normalizePaths(s string) string {
	s = strings.ReplaceAll(s, "/tmp/main.go", "main.go")
	return strings.ReplaceAll(s, "/src/main.go", "main.go")
}

func truncateForLog(s string) string {
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func itoa(n int) string {
	return fmt.Sprint(n)
}
