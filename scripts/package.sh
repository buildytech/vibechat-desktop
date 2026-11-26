#!/usr/bin/env bash
# Unsigned installers of BuildY VibeChat for the system this runs on (CI: one job per OS), into dist/:
#   linux    vibechat_<version>_amd64.deb (wails3 tool package, nfpm): /usr/bin/vibechat
#   windows  vibechat-amd64-installer.exe (NSIS, per-machine): vibechat.exe
#   darwin   BuildY VibeChat.app (ad-hoc signed only) inside BuildY-VibeChat.dmg
#            (no space in the file name: GitHub turns a space in an asset name into a dot, and SHA256SUMS must match)
# and bin/: the bare executable (CI uploads bin/ as is). The app is a window to the owner's vibechat-server: no agent
# runtime, no templates, no local projects (scripts/check-desktop.sh checks the binary).
# Portable zips with stable names (GitHub Release assets, links .../releases/latest/download/<name>):
#   vibechat-desktop-linux-x64.zip    vibechat-desktop-linux-x64/vibechat
#   vibechat-desktop-windows-x64.zip  vibechat-desktop-windows-x64/vibechat.exe
#   vibechat-desktop-macos-arm64.zip  BuildY VibeChat.app (ditto: keeps the ad-hoc signature and symlinks)
# Needs: Go, the wails3 CLI (WAILS3, default wails3), desktop/shell/dist built (scripts/build-shell.sh), and per OS:
# GTK 4 + WebKitGTK 6 dev files (Linux), makensis (Windows), Xcode command line tools (macOS).
# Identity: internal/appid (fixed: tw.buildy.vibechat). Nothing here signs or notarizes; see README "Signing".
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
WAILS3="${WAILS3:-wails3}"
VERSION="${VIBECHAT_VERSION:-0.2.3}"
export VIBECHAT_VERSION="$VERSION"
TAGS="production"
# The status host.version of the build (internal/appid.Version; unstamped builds report "dev").
LDFLAGS="-s -w -X github.com/buildytech/vibechat-desktop/internal/appid.Version=$VERSION"
[ -f desktop/shell/dist/index.html ] || { echo "package: desktop/shell/dist missing (scripts/build-shell.sh)" >&2; exit 1; }
rm -rf bin dist && mkdir -p bin dist
# portable <dir> <executable>: bin/<dir>/ with the executable.
portable() {
	mkdir -p "bin/$1"
	cp "$2" "bin/$1/"
}
case "$(uname -s)" in
Linux)
	export GOARCH=amd64
	go build -tags "$TAGS" -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o bin/vibechat ./desktop/cmd/vibechat
	"$WAILS3" tool package -name vibechat -format deb -config build/linux/nfpm/nfpm.yaml -out dist
	mv dist/vibechat.deb "dist/vibechat_${VERSION}_amd64.deb"
	portable vibechat-desktop-linux-x64 bin/vibechat
	(cd bin && zip -qry ../dist/vibechat-desktop-linux-x64.zip vibechat-desktop-linux-x64)
	;;
MINGW* | MSYS* | CYGWIN*)
	export GOOS=windows GOARCH=amd64 CGO_ENABLED=0
	"$WAILS3" generate syso -arch amd64 -icon build/windows/icon.ico -manifest build/windows/wails.exe.manifest -info build/windows/info.json -out desktop/cmd/vibechat/wails_windows_amd64.syso
	go build -tags "$TAGS" -trimpath -buildvcs=false -ldflags "$LDFLAGS -H windowsgui" -o bin/vibechat.exe ./desktop/cmd/vibechat
	rm -f desktop/cmd/vibechat/wails_windows_amd64.syso
	"$WAILS3" generate webview2bootstrapper -dir build/windows/nsis
	(cd build/windows/nsis && makensis -V2 -DINFO_PRODUCTVERSION="$VERSION" -DARG_WAILS_AMD64_BINARY="$(cygpath -w "$PWD/../../../bin/vibechat.exe")" project.nsi)
	mv bin/vibechat-amd64-installer.exe dist/
	portable vibechat-desktop-windows-x64 bin/vibechat.exe
	if command -v 7z >/dev/null; then
		(cd bin && 7z a -tzip -bso0 -bsp0 ../dist/vibechat-desktop-windows-x64.zip vibechat-desktop-windows-x64)
	else
		powershell -NoProfile -Command "Compress-Archive -Path bin/vibechat-desktop-windows-x64 -DestinationPath dist/vibechat-desktop-windows-x64.zip"
	fi
	rm -f build/windows/nsis/MicrosoftEdgeWebview2Setup.exe
	;;
Darwin)
	export CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=12.0
	go build -tags "$TAGS" -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o bin/vibechat ./desktop/cmd/vibechat
	app="bin/BuildY VibeChat.app"
	mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
	cp bin/vibechat "$app/Contents/MacOS/vibechat"
	cp build/darwin/Info.plist "$app/Contents/Info.plist"
	/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $VERSION" -c "Set :CFBundleShortVersionString $VERSION" "$app/Contents/Info.plist"
	cp build/darwin/icons.icns build/darwin/Assets.car "$app/Contents/Resources/"
	xattr -cr "$app"
	codesign --force --deep --sign - "$app"
	codesign --verify --deep --strict "$app"
	"$WAILS3" tool package -format dmg -name "BuildY VibeChat" -out bin -background build/darwin/dmg-background.png -volume-icon build/darwin/icons.icns -file-icon build/darwin/dmg-file-icon.icns
	mv "bin/BuildY VibeChat.dmg" dist/BuildY-VibeChat.dmg
	[ "$(uname -m)" = "arm64" ] || { echo "package: the macOS zip is the arm64 build; run on Apple Silicon" >&2; exit 1; }
	ditto -c -k --sequesterRsrc --keepParent "$app" dist/vibechat-desktop-macos-arm64.zip
	;;
*)
	echo "package: unsupported system $(uname -s)" >&2
	exit 1
	;;
esac
ls -l dist
