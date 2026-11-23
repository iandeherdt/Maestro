#!/bin/sh
# Builds the release archives for one version into a directory, with a
# SHA256SUMS file. Usage: scripts/build-release.sh v0.1.0 dist
set -eu

version="${1:?usage: build-release.sh VERSION OUTDIR}"
out="${2:?usage: build-release.sh VERSION OUTDIR}"
case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version must look like v1.2.3" >&2; exit 2 ;;
esac

mkdir -p "$out"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  mkdir -p "$work/$os-$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "$work/$os-$arch/maestro" ./cmd/maestro
  files=maestro
  # The root launcher of the systemd sandbox, Linux only (SPEC 11.2).
  if [ "$os" = linux ]; then
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
      -ldflags "-s -w" -o "$work/$os-$arch/maestro-launch" ./cmd/maestro-launch
    files="maestro maestro-launch"
  fi
  # shellcheck disable=SC2086
  tar -C "$work/$os-$arch" -czf "$out/maestro_${os}_${arch}.tar.gz" $files
done

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum maestro_*.tar.gz > SHA256SUMS
else
  shasum -a 256 maestro_*.tar.gz > SHA256SUMS
fi
