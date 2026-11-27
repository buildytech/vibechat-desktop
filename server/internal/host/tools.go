package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/isolate"
	"github.com/buildytech/vibechat-desktop/server/internal/procgroup"
)

// runToolOutput runs a host tool (git) argv in dir in its own process group (killed with its children on ctx end)
// and keeps the last 4 KB of output for the error message. Project tools (Bun, Vite) go through runProjectTool.
func runToolOutput(ctx context.Context, dir string, argv []string, env []string, log func(string), out *bytes.Buffer) error {
	if len(argv) == 0 {
		return nil
	}
	return runCmd(ctx, dir, argv, argv, append(os.Environ(), env...), "", log, out)
}

// runProjectTool runs a project tool (bun install, bun --version) isolated from the server (package isolate): a
// clean environment and, on Linux, a bubblewrap jail where the server's data folder (secrets, auth, other
// projects) does not exist and only dir and spec.Writable are writable.
func runProjectTool(ctx context.Context, dir string, argv []string, spec isolate.Spec, log func(string), out *bytes.Buffer) error {
	if len(argv) == 0 {
		return nil
	}
	spec.Dir = dir
	full, env, mode, err := isolate.Command(argv, spec)
	if err != nil {
		return fmt.Errorf("%s: %v", strings.Join(argv, " "), err)
	}
	return runCmd(ctx, dir, argv, full, env, mode, log, out)
}

func runCmd(ctx context.Context, dir string, shown, argv, env []string, mode string, log func(string), out *bytes.Buffer) error {
	cmd := procgroup.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	procgroup.Prepare(cmd)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return err
	}
	procgroup.Attach(cmd)
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
		how := ""
		if mode != "" {
			how = ", " + mode
		}
		log(fmt.Sprintf("%s (in %s%s): %v", strings.Join(shown, " "), dir, how, werr))
	}
	if werr != nil {
		tail := buf.String()
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		return fmt.Errorf("%s: %v\n%s", strings.Join(shown, " "), werr, strings.TrimSpace(tail))
	}
	return nil
}
