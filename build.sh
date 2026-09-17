#!/usr/bin/env bash
# Build release binaries for all common platforms into ./dist with checksums.
# Usage:  ./build.sh [version]     e.g.  ./build.sh v1.0.0
set -euo pipefail

APP=votereminder-smtp
VERSION="${1:-dev}"
OUT=dist

rm -rf "$OUT"
mkdir -p "$OUT"

# GOOS GOARCH pairs to build.
platforms=(
  "linux   amd64"
  "linux   arm64"
  "darwin  amd64"
  "darwin  arm64"
  "windows amd64"
)

for p in "${platforms[@]}"; do
  read -r goos goarch <<< "$p"
  ext=""
  [ "$goos" = "windows" ] && ext=".exe"
  bin="${APP}-${VERSION}-${goos}-${goarch}${ext}"
  echo "building ${bin}"
  # CGO off => fully static, no libc coupling; trimpath + -s -w => smaller,
  # path-independent binaries.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w" -o "${OUT}/${bin}" .
done

# Checksums so downloaders can verify integrity (sha256sum on Linux, shasum on macOS).
if command -v sha256sum >/dev/null 2>&1; then
  SHA="sha256sum"
else
  SHA="shasum -a 256"
fi
( cd "$OUT" && $SHA * > SHA256SUMS )

echo
echo "built into ${OUT}/:"
ls -1 "$OUT"
