package golock

import (
	"context"
	"testing"
	"time"
)

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, dir); err == nil {
		t.Fatal("a second lock while the first is held")
	}
	release()
	again, err := Lock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
