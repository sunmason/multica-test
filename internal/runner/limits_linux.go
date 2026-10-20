//go:build linux

package runner

import (
	"os/exec"
	"syscall"

	"golang-sandbox/internal/config"
)

// applyLimits installs best-effort rlimits on Linux: CPU-seconds, file size,
// fds and per-uid process count. Memory is intentionally NOT limited via
// RLIMIT_AS — it breaks the Go runtime's virtual address reservation.
func applyLimits(cmd *exec.Cmd, cfg config.Config) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Rlimit = []syscall.Rlimit{
		{Resource: syscall.RLIMIT_CPU, Cur: uint64(cfg.Limits.ExecTimeoutSec), Max: uint64(cfg.Limits.ExecTimeoutSec)},
		{Resource: syscall.RLIMIT_FSIZE, Cur: uint64(cfg.Limits.TmpfsMB) << 20, Max: uint64(cfg.Limits.TmpfsMB) << 20},
		{Resource: syscall.RLIMIT_NOFILE, Cur: 256, Max: 256},
		{Resource: syscall.RLIMIT_NPROC, Cur: uint64(cfg.Limits.PidsLimit), Max: uint64(cfg.Limits.PidsLimit)},
	}
}
