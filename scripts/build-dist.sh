#!/bin/sh
# Builds release archives for every supported platform into dist/.
#   VERSION=1.0.0 scripts/build-dist.sh
set -eu
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
rm -rf dist && mkdir -p dist
for target in linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}
  name="upax-relay_${os}_${arch}"
  out="dist/$name"; mkdir -p "$out"
  ext=""; [ "$os" = windows ] && ext=".exe"
  # GOARM=6 also runs on ARMv7 (Raspberry Pi Zero/1 through 4 in 32-bit mode)
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=6 \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/upax-relay$ext" ./cmd/upax-relay
  cp README.md "$out/"
  if [ "$os" = windows ]; then
    (cd dist && zip -qr "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "$out"
done
cp scripts/install.sh scripts/install.ps1 dist/
(cd dist && shasum -a 256 *.tar.gz *.zip > checksums.txt)
ls -lh dist
