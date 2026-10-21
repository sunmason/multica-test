//go:build linux

package runner

import (
	"os/exec"
	"strings"
	"testing"

	"golang-sandbox/internal/config"
)

func TestLinuxChildFileLimit(t *testing.T) {
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Fatal("Linux local runner requires util-linux prlimit:", err)
	}
	cmd := exec.Command("sh", "-c", "ulimit -n; printf '%s' \"$1\"", "sh", "argument with spaces")
	cmd.SysProcAttr = procGroupAttr()
	applyLimits(cmd, config.Default())
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "256\nargument with spaces" {
		t.Fatalf("child limits/arguments: output=%q err=%v", out, err)
	}
}
