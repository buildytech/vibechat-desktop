//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
)

func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func terminateGroup(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }

func killGroup(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

// attach: on Unix the group is set at fork (Setpgid), nothing to do after Start.
func attach(*exec.Cmd) {}

// hideConsole: Unix children have no console window.
func hideConsole(*exec.Cmd) {}
