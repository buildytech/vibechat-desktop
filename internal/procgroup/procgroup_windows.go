//go:build windows

package procgroup

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNewProcessGroup = 0x00000200
const createNoWindow = 0x08000000

func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup | createNoWindow
	cmd.SysProcAttr.HideWindow = true
}

// terminateGroup: Windows has no SIGTERM for a group; taskkill /T without /F asks the tree to close.
func terminateGroup(cmd *exec.Cmd) { taskkill(cmd, false) }

func killGroup(cmd *exec.Cmd) { taskkill(cmd, true) }

func taskkill(cmd *exec.Cmd, force bool) {
	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	k := exec.Command("taskkill", args...)
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = k.Run()
}
