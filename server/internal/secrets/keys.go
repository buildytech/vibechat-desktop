// Package secrets keeps the owner's Cursor API key on the server: a 0600 file under the data folder (file.go,
// secrets.File). The key is never logged, never
// put in argv or URLs and never handed to the webview: the host passes it to the cursor-sdk-bridge process through its
// environment only. (The desktop app has no Cursor key: the agent runs on the server.)
package secrets

import "strings"

// Mask is the hint the UI may show: the last four characters only.
func Mask(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return "••••"
	}
	return "••••" + key[len(key)-4:]
}
