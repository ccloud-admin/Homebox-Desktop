#!/usr/bin/env bash
# Fetches every Go dependency of Homebox Desktop into third_party/ straight
# from GitHub, so the app can be built offline or behind a restrictive proxy
# (no Go module proxy needed). go.mod points at these folders with `replace`.
set -euo pipefail
cd "$(dirname "$0")/.."
TP=third_party
mkdir -p "$TP"

# module path | GitHub repo | tag or commit
DEPS=(
  "github.com/wailsapp/wails/v2|wailsapp/wails|v2.11.0|v2"
  "github.com/wailsapp/go-webview2|wailsapp/go-webview2|v1.0.22|"
  "github.com/jchv/go-winloader|jchv/go-winloader|715c2860da7e|"
  "golang.org/x/sys|golang/sys|v0.30.0|"
  "golang.org/x/net|golang/net|v0.35.0|"
  "golang.org/x/text|golang/text|v0.22.0|"
  "github.com/leaanthony/slicer|leaanthony/slicer|v1.6.0|"
  "github.com/leaanthony/go-ansi-parser|leaanthony/go-ansi-parser|v1.6.1|"
  "github.com/leaanthony/gosod|leaanthony/gosod|v1.0.4|"
  "github.com/leaanthony/u|leaanthony/u|v1.1.1|"
  "github.com/samber/lo|samber/lo|v1.49.1|"
  "github.com/bep/debounce|bep/debounce|v1.2.1|"
  "github.com/pkg/errors|pkg/errors|v0.9.1|"
  "github.com/pkg/browser|pkg/browser|5ac0b6a4141c|"
  "github.com/google/uuid|google/uuid|v1.6.0|"
  "github.com/wailsapp/mimetype|wailsapp/mimetype|v1.4.1|"
  "github.com/tkrajina/go-reflector|tkrajina/go-reflector|v0.5.8|"
  "github.com/go-ole/go-ole|go-ole/go-ole|v1.3.0|"
  "github.com/godbus/dbus/v5|godbus/dbus|v5.1.0|"
  "github.com/rivo/uniseg|rivo/uniseg|v0.4.7|"
)

for entry in "${DEPS[@]}"; do
  IFS='|' read -r mod repo ref sub <<<"$entry"
  dest="$TP/$mod"
  if [ -f "$dest/.fetched-$ref" ]; then continue; fi
  echo "fetching $mod@$ref"
  rm -rf "$dest" "$TP/.tmp"
  if [[ "$ref" == v* ]]; then
    git -c advice.detachedHead=false clone -q --depth 1 --branch "$ref" "https://github.com/$repo" "$TP/.tmp"
  else
    git clone -q "https://github.com/$repo" "$TP/.tmp"
    git -C "$TP/.tmp" -c advice.detachedHead=false checkout -q "$ref"
  fi
  src="$TP/.tmp${sub:+/$sub}"
  mkdir -p "$(dirname "$dest")"
  rm -rf "$src/.git"
  mv "$src" "$dest"
  rm -rf "$TP/.tmp"
  # Keep only the module line: every dependency is listed in our own go.mod.
  goline=$(grep -m1 '^go ' "$dest/go.mod" 2>/dev/null || echo "go 1.18")
  printf 'module %s\n\n%s\n' "$mod" "$goline" > "$dest/go.mod"
  rm -f "$dest/go.sum"
  touch "$dest/.fetched-$ref"
done
echo "dependencies ready in $TP/"
