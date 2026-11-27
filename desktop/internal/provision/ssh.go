package provision

import (
	"bytes"
	"context"
	"errors"
	"io"

	"golang.org/x/crypto/ssh"
)

// SSHRunner runs the scripts over an SSH connection as root: one session per script (`bash -c <script>`). Scripts
// carry no secrets (they show in the host's process list); a secret goes through stdin.
type SSHRunner struct{ Client *ssh.Client }

const maxOutput = 4 << 20

type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}

func (r SSHRunner) Run(ctx context.Context, script string, stdin []byte) (Result, error) {
	return r.run(ctx, script, stdin, nil)
}

// RunStream is Run that also hands every stderr line to line as it arrives.
func (r SSHRunner) RunStream(ctx context.Context, script string, stdin []byte, line func(string)) (Result, error) {
	lw := &lineWriter{fn: line}
	res, err := r.run(ctx, script, stdin, lw)
	lw.flush()
	return res, err
}

// lineWriter calls fn for each complete line written to it (a partial last line on flush).
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.fn(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 64<<10 {
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

func (l *lineWriter) flush() {
	if len(l.buf) > 0 {
		l.fn(string(l.buf))
		l.buf = nil
	}
}

func (r SSHRunner) run(ctx context.Context, script string, stdin []byte, errTee io.Writer) (Result, error) {
	s, err := r.Client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	s.Stdin = bytes.NewReader(stdin)
	var out, errb capped
	s.Stdout, s.Stderr = &out, &errb
	if errTee != nil {
		s.Stderr = io.MultiWriter(&errb, errTee)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run("/bin/bash -c " + quote(script)) }()
	select {
	case <-ctx.Done():
		_ = s.Signal(ssh.SIGKILL)
		_ = s.Close()
		return Result{}, ctx.Err()
	case err = <-done:
	}
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitStatus()
	default:
		return res, err
	}
	return res, nil
}
