package main

import (
	"testing"
	"time"
)

func TestServerOptionsLoopback(t *testing.T) {
	t.Setenv("VIBECHAT_PORT", "")
	o := serverOptions()
	if o.Host != "127.0.0.1" || o.Port != defaultServerPort {
		t.Fatalf("loopback default changed: %s:%d", o.Host, o.Port)
	}
	if o.WriteTimeout < 30*time.Minute {
		t.Fatalf("WriteTimeout %v cuts long agent runs", o.WriteTimeout)
	}
	t.Setenv("VIBECHAT_PORT", "0.0.0.0")
	if serverOptions().Port != defaultServerPort {
		t.Fatal("bad VIBECHAT_PORT accepted")
	}
}
