#!/bin/bash
#
# build.sh — cross-compiles cfr-trigger for this router's CPU (IPQ5018,
# ARMv7 hard-float), no cross-compiler toolchain needed since
# CGO_ENABLED=0 makes this a static, syscall-only binary (same approach as
# ../sms-reader/build.sh).
#
set -e
cd "$(dirname "$0")"

OUT_DIR="${1:-./dist}"
mkdir -p "$OUT_DIR"

echo "==> building cfr-trigger-arm (linux/arm, GOARM=7)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o "$OUT_DIR/cfr-trigger-arm" .

echo
echo "Done. Binary in $OUT_DIR/"
