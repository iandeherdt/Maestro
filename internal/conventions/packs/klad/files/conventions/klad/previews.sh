#!/bin/sh
# Renders the drawings this change touched, for the pull request: into
# $SCREENSHOT_DIR (Maestro's screenshots command) or the folder given.
# Without a known base it renders every drawing.
set -eu
cd "$(dirname "$0")/../.."
. conventions/klad/env.sh
dir=${1:-${SCREENSHOT_DIR:-}}
if [ -z "$dir" ]; then
    echo "usage: sh conventions/klad/previews.sh DIR (or set SCREENSHOT_DIR)" >&2
    exit 2
fi
mkdir -p "$dir"
names=""
if [ -f .git/maestro-base ]; then
    names=$(git diff --name-only "$(cat .git/maestro-base)" -- "$KLAD_SOURCE" 2>/dev/null |
        sed -n "s#^$KLAD_SOURCE/\(.*\)\.klad/.*#\1#p" | sort -u)
fi
names=${names:-$(drawings)}
for n in $names; do
    [ -d "$KLAD_SOURCE/$n.klad" ] || continue
    "$KLAD" render "$KLAD_SOURCE/$n.klad" -o "$dir/$(echo "$n" | tr '/' '-').png" > /dev/null ||
        echo "no preview of $n: it does not render" >&2
done
