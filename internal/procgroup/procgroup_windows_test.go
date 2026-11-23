//go:build windows

package procgroup

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A PowerShell that starts a detached grandchild: the Job Object must take the grandchild down too (no orphans).
func TestStopKillsTheWholeJob(t *testing.T) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"$p = Start-Process ping -ArgumentList '-n','60','127.0.0.1' -PassThru -WindowStyle Hidden; $p.Id; Start-Sleep 60")
	Prepare(cmd)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Attach(cmd)
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("grandchild pid: %q", buf[:n])
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	Stop(cmd, done, 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child))
	if err != nil {
		return // gone
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code == 259 { // STILL_ACTIVE
		t.Fatalf("grandchild %d survived", child)
	}
}
