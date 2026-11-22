#!/bin/sh
# Every check that decides whether a change is acceptable. Needs no network.
# The Maestro service runs this itself before it pushes anything, and CI
# runs the same script. klad renders the same pixels on every machine, so
# the committed pictures must be exactly what their drawings export to.
set -eu
cd "$(dirname "$0")/../.."
. conventions/klad/env.sh
tmp=$(mktemp -d)
echo "== every drawing exports =="
KLAD_OUT="$tmp" sh conventions/klad/build.sh
echo "== the committed pictures are the drawings as they are now =="
stale=0
for f in $(cd "$tmp" && find . -type f | sed 's#^\./##' | sort); do
    if [ ! -f "$KLAD_OUTPUT/$f" ]; then
        echo "missing: $KLAD_OUTPUT/$f. Export it: sh conventions/klad/build.sh ${f%.png}" >&2
        stale=1
    elif ! cmp -s "$tmp/$f" "$KLAD_OUTPUT/$f"; then
        echo "out of date: $KLAD_OUTPUT/$f. Export it again: sh conventions/klad/build.sh ${f%.png}" >&2
        stale=1
    fi
done
rm -rf "$tmp"
exit "$stale"
