#!/bin/bash
#
# build.sh — cross-compiles the Go GUI for macOS (Apple Silicon + Intel)
# and Windows (amd64), no Go toolchain needed on the machine that runs it.
#
set -e
cd "$(dirname "$0")"

OUT_DIR="${1:-./dist}"
mkdir -p "$OUT_DIR"

build() {
  local goos="$1" goarch="$2" out="$3"
  echo "==> building $out ($goos/$goarch)"
  GOOS="$goos" GOARCH="$goarch" go build -o "$OUT_DIR/$out" .
}

build darwin arm64 cb0401-tune-control-macos-arm64
build darwin amd64 cb0401-tune-control-macos-intel
build windows amd64 cb0401-tune-control-windows.exe

echo
echo "Done. Binaries in $OUT_DIR/"
