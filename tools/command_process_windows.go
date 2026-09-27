//go:build windows

package tools

import "os/exec"

func configureCommandProcess(_ *exec.Cmd) {}

func shellCommand(line string) *exec.Cmd { return exec.Command("cmd", "/C", line) }

func killCommandProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
