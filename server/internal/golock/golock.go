// Package golock serializes Go builds on a VibeChat server: one `go build`/`templ generate` at a time (a 1 vCPU host
// cannot run two link steps of PocketBase in its memory limit). The lock is a file in the shared Go cache, so the
// host process and every `vibechat-server go-dev` in its jail take the same lock.
package golock

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// Lock blocks until it holds the build lock of cache (or ctx ends) and returns its release.
func Lock(ctx context.Context, cache string) (func(), error) {
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(cache, "build.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() { unlock(f); f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
