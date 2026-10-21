//go:build linux

package runner

import (
	"os/exec"
	"strconv"

	"golang-sandbox/internal/config"
)

// applyLimits uses util-linux prlimit to set limits before exec of the child.
// Go's SysProcAttr has no Rlimit field. A missing prlimit fails child startup
// rather than executing user code without limits. Docker mode does not use it.
// RLIMIT_AS is omitted because it breaks Go's virtual address reservation.
func applyLimits(cmd *exec.Cmd, cfg config.Config) {
	wrapper := exec.Command("prlimit",
		"--cpu="+strconv.Itoa(cfg.Limits.ExecTimeoutSec),
		"--fsize="+strconv.FormatUint(uint64(cfg.Limits.TmpfsMB)<<20, 10),
		"--nofile=256", "--nproc="+strconv.Itoa(cfg.Limits.PidsLimit), "--", cmd.Path)
	cmd.Path = wrapper.Path
	cmd.Args = append(wrapper.Args, cmd.Args[1:]...)
	cmd.Err = wrapper.Err
}
