#!/bin/sh
# Builds traceshot, static, for linux/amd64, linux/arm64 and darwin/arm64,
# into OUTDIR (default ./bin). Run it from anywhere inside the Tracelight
# repository; it needs Go 1.27 or later.
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
out=${1:-bin}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cd "$root"
for target in linux/amd64 linux/arm64 darwin/arm64; do
	os=${target%/*} arch=${target#*/}
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w' -o "$out/traceshot-$os-$arch" ./cmd/traceshot
	echo "$out/traceshot-$os-$arch"
done
