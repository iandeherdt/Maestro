#!/bin/sh
# Every check that decides whether a change is acceptable. Needs no network.
# The Maestro service runs this itself before it pushes anything, and CI
# runs the same script. Everything is built into a throwaway folder: an
# export carries the time it was made, so building into the repository
# would change committed files on every run.
set -eu
cd "$(dirname "$0")/../.."
. conventions/blender/env.sh
tmp=$(mktemp -d)
echo "== every generator runs and writes something =="
ART_OUT="$tmp" sh conventions/blender/build.sh
echo "== every model loads back into Blender, within the triangle limit =="
blender_run --python conventions/blender/check.py -- "$tmp" > "$tmp.log" 2>&1 || {
    grep -E '^FAILED|Error' "$tmp.log" >&2 || tail -n 25 "$tmp.log" >&2
    exit 1
}
grep '^CHECK' "$tmp.log" || true
rm -rf "$tmp" "$tmp.log"
