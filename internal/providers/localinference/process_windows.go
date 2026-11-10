//go:build windows

package localinference

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func configureProcessTree(*exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	args := []string{"/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)}
	taskkill := filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe")
	if os.Getenv("SystemRoot") == "" {
		taskkill = "taskkill.exe"
	}
	// taskkill is invoked directly, not through a shell. Its tree mode includes
	// descendants started by the dispatcher, unlike os.Process.Kill alone.
	_ = exec.Command(taskkill, args...).Run()
	_ = cmd.Process.Kill()
}
