#!/bin/bash
# Builds Omni for macOS (Apple Silicon + Intel), Windows, and Linux into dist/.
set -euo pipefail
cd "$(dirname "$0")"

mkdir -p dist
LDFLAGS="-s -w"

echo "building omni…"
GOOS=darwin  GOARCH=arm64 go build -ldflags="$LDFLAGS" -o dist/omni-macos-arm64 ./...
GOOS=darwin  GOARCH=amd64 go build -ldflags="$LDFLAGS" -o dist/omni-macos-intel ./...
GOOS=windows GOARCH=amd64 go build -ldflags="$LDFLAGS" -o dist/omni-windows.exe ./...
GOOS=linux   GOARCH=amd64 go build -ldflags="$LDFLAGS" -o dist/omni-linux       ./...

# A ready-to-run copy for this machine.
cp "dist/omni-macos-$(uname -m | sed 's/x86_64/intel/;s/arm64/arm64/')" omni 2>/dev/null || \
  go build -ldflags="$LDFLAGS" -o omni ./...

echo "done → dist/"
ls -lh dist/
