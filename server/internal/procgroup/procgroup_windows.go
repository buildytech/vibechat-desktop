//go:build windows

package procgroup

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNewProcessGroup = 0x00000200
const createNoWindow = 0x08000000

// jobs holds the Job Object of each started child (by pid). The job has KILL_ON_JOB_CLOSE, so if VibeChat itself
// dies, Windows closes the handle and takes the whole tree down with it.
var jobs sync.Map // int -> windows.Handle

func prepare(cmd *exec.Cmd) {
	hideConsole(cmd)
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}

// hideConsole: a console child of the windowsgui app would otherwise open its own console window for a moment.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}

// attach puts a started child into its own Job Object. Its descendants are created inside the job (no breakaway).
// A grandchild spawned in the instant between Start and the assignment is still covered by taskkill /T below.
func attach(cmd *exec.Cmd) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	jobs.Store(cmd.Process.Pid, job)
}

// terminateGroup: Windows has no SIGTERM for a group; taskkill /T without /F asks the tree to close.
func terminateGroup(cmd *exec.Cmd) { taskkill(cmd, false) }

// killGroup ends every process of the child's job, then sweeps the tree by pid as a fallback.
func killGroup(cmd *exec.Cmd) {
	if v, ok := jobs.LoadAndDelete(cmd.Process.Pid); ok {
		job := v.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
	}
	taskkill(cmd, true)
}

func taskkill(cmd *exec.Cmd, force bool) {
	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	_ = Command("taskkill", args...).Run()
}
