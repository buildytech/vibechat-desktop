# Desktop packaging assets

Generated with the pinned `wails3 generate build-assets` (v3.1.0), then trimmed to the three desktop systems. The
`.plist`, Windows resource/NSIS files and icons come from that generator; `scripts/package.sh` uses the same Wails
packaging commands (NSIS on Windows, `wails3 tool package` for DMG and DEB, `wails3 generate appimage` on Linux).

Fixed identity: `tw.buildy.vibechat`, product `BuildY VibeChat`, binary `vibechat`, company `BuildY` (`internal/appid`). The
bundle ID and executable name must not drift: the WebView keeps sessions under its app identity. Tests check the
packaging metadata; do not regenerate with default placeholder values. The icon is still Wails' generated icon (a
custom BuildY icon is a future polish step).

No signing identity/credential lives here. macOS gets an ad-hoc (`-`) signature only so Apple Silicon can execute it;
that is not a Developer ID signature and Gatekeeper will warn. See the root README for distribution/signing notes.
