package procgroup

import (
	"context"
	"os/exec"
)

// Command is the only way the app builds a child process (exec.Command is banned elsewhere, see
// TestNoBareExecCommand): on Windows the child gets no console window (CREATE_NO_WINDOW + HideWindow), so git, the
// sandbox preflight and the tools do not flash a console from the windowsgui app. Prepare adds the process group on
// top and keeps these flags.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	hideConsole(cmd)
	return cmd
}

// CommandContext is Command with a context that kills the child when done.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	hideConsole(cmd)
	return cmd
}
