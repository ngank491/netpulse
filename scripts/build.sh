#!/bin/sh
# Cross-compile release archives into dist/. Usage: VERSION=0.1.0 ./scripts/build.sh
set -eu
cd "$(dirname "$0")/.."
VERSION=${VERSION:-dev}
rm -rf dist && mkdir dist
for target in linux/amd64 linux/arm64 linux/arm/7 windows/amd64 darwin/arm64 darwin/amd64; do
  os=${target%%/*}; rest=${target#*/}; arch=${rest%%/*}; arm=""
  [ "$rest" != "$arch" ] && arm=${rest#*/}
  name="netpulse-$VERSION-$os-$arch${arm:+v$arm}"
  bin=netpulse; [ "$os" = windows ] && bin=netpulse.exe
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=$arm \
    go build -trimpath -ldflags="-s -w -X netpulse/internal/app.Version=$VERSION" -o "dist/$name/$bin" .
  cp README.md LICENSE "dist/$name/"
  if [ "$os" = windows ]; then
    (cd dist && zip -qr "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "dist/$name"
  echo "built $name"
done
(cd dist && sha256sum ./*.zip ./*.tar.gz > SHA256SUMS)
