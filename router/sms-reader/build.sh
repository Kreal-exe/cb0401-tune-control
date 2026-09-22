#!/bin/bash
#
# build.sh — cross-compiles sms-reader for this router's CPU (IPQ5018,
# ARMv7 hard-float), no cross-compiler toolchain needed since
# CGO_ENABLED=0 makes this a static, syscall-only binary.
#
# setup.sh/.ps1 normally do this on the fly whenever Go is available; this
# script exists for the same reason gui/build.sh does - so someone without
# Go installed can build it on another machine and drop the result in
# (setup.sh/.ps1 fall back to dist/sms-reader-arm here if they can't build
# it themselves).
#
set -e
cd "$(dirname "$0")"

OUT_DIR="${1:-./dist}"
mkdir -p "$OUT_DIR"

echo "==> building sms-reader-arm (linux/arm, GOARM=7)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o "$OUT_DIR/sms-reader-arm" .

echo
echo "Done. Binary in $OUT_DIR/"
