//go:build windows

package desktopcore

import (
	"os"
	"os/exec"
	"strconv"
)

// configureProcessGroup is a no-op on Windows: process-tree termination is
// done by taskkill /T at kill time.
func configureProcessGroup(_ *exec.Cmd) {}

// killProcessGroup terminates the child and every descendant.
func killProcessGroup(process *os.Process) {
	if process == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(process.Pid)).Run(); err != nil {
		_ = process.Kill()
	}
}
