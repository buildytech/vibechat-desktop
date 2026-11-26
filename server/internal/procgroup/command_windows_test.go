//go:build windows

package procgroup

import (
	"context"
	"testing"
)

// No console window for any child, and Prepare keeps it while adding the process group.
func TestCommandHidesTheConsole(t *testing.T) {
	c := Command("git", "--version")
	if c.SysProcAttr == nil || !c.SysProcAttr.HideWindow || c.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("Command: %+v", c.SysProcAttr)
	}
	cc := CommandContext(context.Background(), "git")
	if cc.SysProcAttr == nil || cc.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CommandContext: %+v", cc.SysProcAttr)
	}
	Prepare(c)
	if c.SysProcAttr.CreationFlags&(createNoWindow|createNewProcessGroup) != createNoWindow|createNewProcessGroup || !c.SysProcAttr.HideWindow {
		t.Fatalf("Prepare lost a flag: %+v", c.SysProcAttr)
	}
}
