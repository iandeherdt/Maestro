#!/bin/sh
# Installs the symphony binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/iandeherdt/Maestro/main/install.sh | sh
#
# It downloads the archive for this machine, checks it against the
# release's SHA256SUMS file, and puts the binary in ~/.local/bin. It does
# not use sudo and changes nothing else.
#
# Settings (environment variables):
#   SYMPHONY_VERSION      a tag such as v0.1.0 (default: the latest release)
#   SYMPHONY_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
set -eu

main() {
  repo="iandeherdt/Maestro"
  base="${SYMPHONY_DOWNLOAD_BASE:-https://github.com/$repo/releases}"
  version="${SYMPHONY_VERSION:-}"
  dir="${SYMPHONY_INSTALL_DIR:-$HOME/.local/bin}"

  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) fail "this installer supports macOS and Linux only" ;;
  esac
  case "$(uname -m)" in
    arm64 | aarch64) arch=arm64 ;;
    x86_64 | amd64) arch=amd64 ;;
    *) fail "unsupported processor: $(uname -m)" ;;
  esac
  for tool in curl tar; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is needed and was not found"
  done

  if [ -n "$version" ]; then
    case "$version" in
      *[!A-Za-z0-9.-]*) fail "SYMPHONY_VERSION must look like v1.2.3" ;;
      v[0-9]*) ;;
      *) fail "SYMPHONY_VERSION must look like v1.2.3" ;;
    esac
    url="$base/download/$version"
  else
    url="$base/latest/download"
  fi

  archive="symphony_${os}_${arch}.tar.gz"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  say "Downloading $archive"
  fetch "$url/$archive" "$tmp/$archive" || fail "the download failed. Is there a release yet? See $base"
  fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS" || fail "the checksum file could not be downloaded"

  want="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
  [ -n "$want" ] || fail "the checksum file does not list $archive"
  if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
  elif command -v shasum >/dev/null 2>&1; then
    got="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
  else
    fail "sha256sum or shasum is needed to check the download"
  fi
  [ "$got" = "$want" ] || fail "the checksum does not match. Nothing was installed."
  say "Checksum ok"

  tar -xzf "$tmp/$archive" -C "$tmp" symphony
  mkdir -p "$dir"
  # Written next to the target and renamed, so a running copy is never
  # half replaced.
  cp "$tmp/symphony" "$dir/.symphony.new"
  chmod 755 "$dir/.symphony.new"
  mv -f "$dir/.symphony.new" "$dir/symphony"

  say "Installed $("$dir/symphony" version) in $dir"
  case ":$PATH:" in
    *":$dir:"*) say "Next: symphony setup" ;;
    *)
      say "$dir is not on your PATH. Run it as:"
      say "  $dir/symphony setup"
      say "or add this line to your shell profile:"
      say "  export PATH=\"$dir:\$PATH\""
      ;;
  esac
}

fetch() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "$1" --output "$2"
}

say() { printf '%s\n' "$*"; }

fail() {
  printf 'install failed: %s\n' "$*" >&2
  exit 1
}

# Everything is inside main, so a download cut off halfway runs nothing.
main "$@"
