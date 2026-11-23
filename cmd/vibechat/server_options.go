package main

import (
	"os"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// defaultServerPort is the headless (`server` build) port; VIBECHAT_PORT overrides it.
const defaultServerPort = 18120

// serverOptions bind the `server` build (headless HTTP, for tests and the exam) to loopback only: that mode has no
// window and no auth, so it must never listen on a shared interface. WriteTimeout outlasts a long agent run on the
// synchronous prompt route (the Wails default is 30 s).
func serverOptions() application.ServerOptions {
	port := defaultServerPort
	if v, err := strconv.Atoi(os.Getenv("VIBECHAT_PORT")); err == nil && v > 0 && v < 65536 {
		port = v
	}
	return application.ServerOptions{Host: "127.0.0.1", Port: port, WriteTimeout: serverWriteTimeout}
}

// serverWriteTimeout covers the longest agent run the host allows (agentRunTimeout) with margin.
const serverWriteTimeout = 35 * time.Minute
