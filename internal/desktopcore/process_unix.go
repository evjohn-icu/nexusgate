//go:build !windows

package desktopcore

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group so a stop
// reaches every process it spawned, not just the Hub itself.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the whole group.
func killProcessGroup(process *os.Process) {
	if process == nil {
		return
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil {
		_ = process.Kill()
	}
}
