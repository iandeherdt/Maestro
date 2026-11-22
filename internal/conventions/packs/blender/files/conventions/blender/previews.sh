#!/bin/sh
# Renders the pictures of the generators this change touched, for the pull
# request: into $SCREENSHOT_DIR (Maestro's screenshots command) or the
# folder given. Without a known base it renders every generator.
set -eu
cd "$(dirname "$0")/../.."
. conventions/blender/env.sh
dir=${1:-${SCREENSHOT_DIR:-}}
if [ -z "$dir" ]; then
    echo "usage: sh conventions/blender/previews.sh DIR (or set SCREENSHOT_DIR)" >&2
    exit 2
fi
mkdir -p "$dir"
names=""
if [ -f .git/maestro-base ]; then
    names=$(git diff --name-only "$(cat .git/maestro-base)" -- "$ART_SOURCE" 2>/dev/null |
        sed -n "s#^$ART_SOURCE/\([^_/][^/]*\)\.py\$#\1#p")
fi
names=${names:-$(generators)}
work=$(mktemp -d)
for n in $names; do
    [ -f "$ART_SOURCE/$n.py" ] || continue
    mkdir -p "$work/out" "$work/pic"
    if generate "$n" "$work/out" "$work/pic" "$work/log"; then
        for p in "$work/pic"/*.png; do
            [ -f "$p" ] && cp "$p" "$dir/$n-$(basename "$p")"
        done
    else
        echo "no preview of $n: the generator failed" >&2
    fi
    rm -rf "$work/out" "$work/pic"
done
rm -rf "$work"
