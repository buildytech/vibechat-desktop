// Package appid is the desktop app's fixed identity. The WebView keeps the Studio's local state (localStorage) per app
// identity: the macOS WKWebView data store follows the bundle id, the Windows WebView2 user-data folder is set from ID
// (cmd/vibechat), so these never change between releases or with a renamed binary. The packaging files under build/
// carry the same values (appid_test.go checks them).
package appid

const (
	// ID is the reverse-DNS application id (macOS CFBundleIdentifier, the Windows WebView2 data folder name, the
	// per-user data folder under os.UserConfigDir).
	ID = "tw.buildy.vibechat"
	// Name is the product name shown to people.
	Name = "BuildY VibeChat"
	// Binary is the executable name (Linux package and .desktop entry, the Windows .exe, the macOS CFBundleExecutable).
	Binary = "vibechat"
	// Company is the publisher shown by installers.
	Company = "BuildY"
	// KeychainService names the OS keychain items of the app (its SSH key and the host token).
	KeychainService = "vibechat-desktop"
)

// Version is the app version the build was stamped with: CI and scripts/package.sh set it with
// -ldflags "-X github.com/buildytech/vibechat-desktop/internal/appid.Version=<version>". Unstamped builds are "dev";
// GET /runtime/v1/host/capabilities reports it as version.
var Version = "dev"
