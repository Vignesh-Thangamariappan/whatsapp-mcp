#!/usr/bin/env bash
# Cross-compiles the bridge for Windows x64 from any machine with Go.
# Pure Go (no CGO), so no C compiler is needed here or on the Windows machine.
# Output: dist/whatsapp-bridge.exe, then copy the repo (plus this exe into
# whatsapp-bridge/) to the Windows machine and run windows/install.ps1 there.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/whatsapp-bridge"

mkdir -p "$ROOT/dist"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o "$ROOT/dist/whatsapp-bridge.exe" .
echo "Built $ROOT/dist/whatsapp-bridge.exe"
