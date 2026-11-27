// Package gopin is the Go toolchain VibeChat runs Go projects with (the templ runner): one pinned release for the
// setup wizard, `vibechat-server ensure-go` and the import checks. Bump Version, Archive, SHA256 and Size together
// (from https://go.dev/dl/?mode=json) and the templates' templ layer `go` field.
package gopin

const (
	// Version is the toolchain (go.mod `go`/`toolchain` lines above it are refused: GOTOOLCHAIN=local).
	Version = "1.27.4"
	// Archive is the linux-amd64 release archive on dl.google.com.
	Archive = "go1.27.4.linux-amd64.tar.gz"
	URL     = "https://dl.google.com/go/" + Archive
	SHA256  = "7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840"
	Size    = 70680823
	// Root is GOROOT on a VibeChat server: root-owned and outside the data folder (the tools jail hides that), so
	// Go's whole tree is visible read-only in the jail. Bin goes on the unit PATH.
	Dir  = "/opt/vibechat"
	Root = Dir + "/go"
	Bin  = Root + "/bin"
)

// Domains are the hosts a Go project needs through the server egress allowlist: the module proxy, the checksum
// database and the toolchain download.
var Domains = []string{"proxy.golang.org", "sum.golang.org", "dl.google.com"}
