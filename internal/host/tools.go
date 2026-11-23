package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/buildytech/vibechat-desktop/internal/procgroup"
)

// runToolOutput runs argv in dir in its own process group (killed with its children on ctx end) and keeps the last
// 4 KB of output for the error message.
func runToolOutput(ctx context.Context, dir string, argv []string, env []string, log func(string), out *bytes.Buffer) error {
	if len(argv) == 0 {
		return nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	procgroup.Prepare(cmd)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var werr error
	go func() { werr = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		procgroup.Stop(cmd, done, 2e9)
		<-done
		return ctx.Err()
	}
	if out != nil {
		out.Write(buf.Bytes())
	}
	if log != nil {
		log(fmt.Sprintf("%s (in %s): %v", strings.Join(argv, " "), dir, werr))
	}
	if werr != nil {
		tail := buf.String()
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return fmt.Errorf("%s: %v\n%s", strings.Join(argv, " "), werr, strings.TrimSpace(tail))
	}
	return nil
}
