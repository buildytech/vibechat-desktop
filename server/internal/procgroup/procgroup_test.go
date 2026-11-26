//go:build !windows

package procgroup

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A shell that spawns a grandchild: Stop must take the grandchild down too (no orphans).
func TestStopKillsTheWholeGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $!; wait")
	Prepare(cmd)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	var child int
	for _, c := range strings.TrimSpace(string(buf[:n])) {
		child = child*10 + int(c-'0')
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	Stop(cmd, done, 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(child, 0); err == nil {
		t.Fatalf("grandchild %d survived", child)
	}
}
