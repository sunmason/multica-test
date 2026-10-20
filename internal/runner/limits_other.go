//go:build !linux

package runner

import (
	"os/exec"

	"golang-sandbox/internal/config"
)

// applyLimits is a no-op off Linux: SysProcAttr.Rlimit does not exist on
// darwin and the portable limits (CPU seconds) are not worth a second
// platform-specific implementation for a dev-fallback runner. Wall-clock
// timeout and output caps still apply on every path.
func applyLimits(cmd *exec.Cmd, cfg config.Config) {}
