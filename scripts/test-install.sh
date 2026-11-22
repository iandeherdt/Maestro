#!/bin/sh
# Tests install.sh against locally built archives. A stand-in for curl
# serves files from a directory, so no network is used.
# Usage: scripts/test-install.sh
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

"$root/scripts/build-release.sh" v9.8.7 "$work/served/download/v9.8.7" >/dev/null
mkdir -p "$work/served/latest" "$work/bin"
cp -R "$work/served/download/v9.8.7" "$work/served/latest/download"

# The stand-in: the URL is the argument that starts with https://test/,
# the target follows --output.
cat > "$work/bin/curl" <<'STUB'
#!/bin/sh
url="" out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift ;;
    https://test/*) url="$1" ;;
  esac
  shift
done
src="$SERVED/${url#https://test/}"
[ -f "$src" ] || exit 22
cp "$src" "$out"
STUB
chmod +x "$work/bin/curl"

run() {
  env PATH="$work/bin:$PATH" SERVED="$work/served" HOME="$work/home" \
    MAESTRO_DOWNLOAD_BASE=https://test "$@" sh "$root/install.sh"
}

fails=0
check() {
  if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fails=$((fails + 1)); fi
}

# 1. Latest release into the default directory.
out="$(run 2>&1)"
check "latest installs" "$("$work/home/.local/bin/maestro" version)" "maestro v9.8.7"
case "$out" in *"Checksum ok"*"is not on your PATH"*) r=yes ;; *) r=no ;; esac
check "says what to do when the directory is not on PATH" "$r" yes

# 2. A named version into a chosen directory that is on PATH.
out="$(PATH="$work/custom:$PATH" run MAESTRO_VERSION=v9.8.7 MAESTRO_INSTALL_DIR="$work/custom" 2>&1)"
check "named version installs" "$("$work/custom/maestro" version)" "maestro v9.8.7"
case "$out" in *"Next: maestro setup"*) r=yes ;; *) r=no ;; esac
check "next step when on PATH" "$r" yes

# 3. A tampered archive is refused and nothing is installed.
for f in "$work"/served/latest/download/maestro_*.tar.gz; do printf 'x' >> "$f"; done
if out="$(run MAESTRO_INSTALL_DIR="$work/tampered" 2>&1)"; then r=installed; else r=refused; fi
check "tampered archive is refused" "$r" refused
check "nothing installed after a bad checksum" "$(ls "$work/tampered" 2>/dev/null | wc -l | tr -d ' ')" 0
case "$out" in *"checksum does not match"*) r=yes ;; *) r=no ;; esac
check "says why" "$r" yes

# 4. An archive missing from the checksum file is refused.
: > "$work/served/latest/download/SHA256SUMS"
if run MAESTRO_INSTALL_DIR="$work/unlisted" >/dev/null 2>&1; then r=installed; else r=refused; fi
check "unlisted archive is refused" "$r" refused

# 5. No release, and a malformed version.
if run MAESTRO_VERSION=v0.0.0 MAESTRO_INSTALL_DIR="$work/none" >/dev/null 2>&1; then r=installed; else r=refused; fi
check "missing release fails" "$r" refused
if run MAESTRO_VERSION='v9.8.7/../../latest/download' MAESTRO_INSTALL_DIR="$work/none" >/dev/null 2>&1; then r=installed; else r=refused; fi
check "version with a path in it fails" "$r" refused
if run MAESTRO_VERSION='../../x' MAESTRO_INSTALL_DIR="$work/none" >/dev/null 2>&1; then r=installed; else r=refused; fi
check "malformed version fails" "$r" refused

[ "$fails" -eq 0 ] || exit 1
