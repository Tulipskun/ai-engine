//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import (
	"os/exec"
	"syscall"
)

func configureCommandProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func shellCommand(line string) *exec.Cmd { return exec.Command("bash", "-c", line) }

func killCommandProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// The command starts its own process group. Kill the group so children
	// cannot keep stdout/stderr pipes open after the parent is terminated.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
