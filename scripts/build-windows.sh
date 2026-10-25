#!/usr/bin/env bash
# Builds HomeboxDesktop.exe and its installer for 64-bit Windows.
# Works on Linux (cross-compiling) or in Git Bash / WSL on Windows.
# Needs: Go 1.24+, git, NSIS (makensis) and windres (binutils-mingw-w64).
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-1.4.1}"
# GitHub repository ("owner/name") the app checks for its own updates.
# Left empty, the app still updates Homebox itself but not Homebox Desktop.
DESKTOP_REPO="${DESKTOP_REPO:-}"
COMMAS="$(echo "$VERSION" | tr '.' ',')"
export GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod GOTOOLCHAIN=local

./scripts/fetch-deps.sh

echo "== tests"
go test -count=1 .

echo "== Windows resources (icon, manifest, version)"
WINDRES="${WINDRES:-x86_64-w64-mingw32-windres}"
sed "s/@VERSION_COMMAS@/$COMMAS/g; s/@VERSION@/$VERSION/g" build/windows/resources.rc > build/windows/resources.generated.rc
( cd build/windows && "$WINDRES" --preprocessor=cat -O coff -o ../../rsrc_windows_amd64.syso resources.generated.rc )
rm build/windows/resources.generated.rc

echo "== HomeboxDesktop.exe"
mkdir -p build/bin
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -tags desktop,production -trimpath \
  -ldflags "-s -w -H windowsgui -X main.appVersion=$VERSION -X main.desktopRepo=$DESKTOP_REPO" \
  -o build/bin/HomeboxDesktop.exe .

echo "== installer"
makensis -V2 -DVERSION="$VERSION" -DOUTFILE="../bin/HomeboxDesktop-Setup-$VERSION.exe" build/windows/installer.nsi
( cd build/bin && sha256sum "HomeboxDesktop-Setup-$VERSION.exe" > SHA256SUMS.txt )
ls -la build/bin
