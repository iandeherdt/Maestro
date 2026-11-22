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
manifest=$(mktemp)
echo "== every generator runs and writes something =="
ART_OUT="$tmp" ART_MANIFEST="$manifest" sh conventions/blender/build.sh

echo "== no two generators write the same file =="
dups=$(awk '{ print $2 }' "$manifest" | sort | uniq -d)
if [ -n "$dups" ]; then
    for f in $dups; do
        echo "conventions: $f is written by more than one generator: $(awk -v f="$f" '$2 == f { print $1 }' "$manifest" | tr '\n' ' ')" >&2
    done
    exit 1
fi

# A generator that changed must come with what it makes, built again:
# otherwise the committed model is the old one. Only known inside the
# Maestro sandbox, where the base of the change is recorded.
if [ -f .git/maestro-base ] && command -v git >/dev/null 2>&1; then
    echo "== what a changed generator makes is built again and committed =="
    base=$(cat .git/maestro-base)
    stale=0
    for n in $(git diff --name-only "$base" -- "$ART_SOURCE" | sed -n "s#^$ART_SOURCE/\([^_/][^/]*\)\.py\$#\1#p"); do
        [ -f "$ART_SOURCE/$n.py" ] || continue
        for f in $(awk -v n="$n" '$1 == n { print $2 }' "$manifest"); do
            # Up to date when it is exactly what the generator makes now,
            # or when this change built it again (an export that carries
            # the time it was made is never the same twice).
            if ! cmp -s "$tmp/$f" "$ART_OUTPUT/$f" && [ -z "$(git diff --name-only "$base" -- "$ART_OUTPUT/$f")" ]; then
                echo "conventions: $ART_SOURCE/$n.py changed, but $ART_OUTPUT/$f was not built again. Run: sh conventions/blender/build.sh $n" >&2
                stale=1
            fi
        done
    done
    [ "$stale" -eq 0 ] || exit 1
fi
echo "== every model loads back into Blender, within the triangle limit =="
blender_run --python conventions/blender/check.py -- "$tmp" > "$tmp.log" 2>&1 || {
    grep -E '^FAILED|Error' "$tmp.log" >&2 || tail -n 25 "$tmp.log" >&2
    exit 1
}
grep '^CHECK' "$tmp.log" || true
rm -rf "$tmp" "$tmp.log" "$manifest"
