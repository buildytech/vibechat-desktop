package remote

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The lockdown confirmation rides out sshd's re-exec after a reload (connection refused), but nothing else.
func TestWhileRefused(t *testing.T) {
	n := 0
	err := whileRefused(context.Background(), 5*time.Second, func() error {
		if n++; n < 3 {
			if n == 1 {
				return errors.New("dial tcp 192.0.2.1:22: connect: connection refused")
			}
			return errors.New("dial tcp 192.0.2.1:22: connectex: No connection could be made because the target machine actively refused it.")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatal(err, n)
	}
	n = 0
	err = whileRefused(context.Background(), 5*time.Second, func() error { n++; return errors.New("ssh: unable to authenticate") })
	if err == nil || n != 1 {
		t.Fatal(err, n)
	}
	n = 0
	err = whileRefused(context.Background(), 0, func() error { n++; return errors.New("connect: connection refused") })
	if err == nil || n != 1 {
		t.Fatal(err, n)
	}
}
