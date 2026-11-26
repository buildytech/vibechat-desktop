//go:build server

package main

import "fmt"

func probeLocalhost() int {
	fmt.Println("probe-localhost: needs the desktop build (a webview)")
	return 2
}
