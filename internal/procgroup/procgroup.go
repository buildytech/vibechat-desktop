// Package procgroup starts child processes in their own process group (Unix) or console process group (Windows) so
// the host can stop a process together with everything it spawned: the cursor-sdk-bridge and its sandboxed shells, a
// preview dev server and its watchers. Stop is graceful first, then forced, and never touches other processes.
package procgroup

import (
	"os/exec"
	"time"
)

// Prepare marks cmd to start in its own group. Call before cmd.Start.
func Prepare(cmd *exec.Cmd) { prepare(cmd) }

// Stop asks the group of a started cmd to exit, waits up to grace for done to close, then kills the whole group.
// done is closed by the caller's cmd.Wait goroutine.
func Stop(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	select {
	case <-done:
		killGroup(cmd) // leftovers of an exited leader
		return
	default:
	}
	terminateGroup(cmd)
	select {
	case <-done:
	case <-time.After(grace):
	}
	killGroup(cmd)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
