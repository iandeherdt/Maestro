#!/bin/sh
# Takes pictures of the game for the pull request: into $SCREENSHOT_DIR
# (Maestro's screenshots command) or the folder given. Each scene of the
# build (or the scenes given in SHOT_SCENES, separated by ':') is played for
# two seconds and then drawn, at most 6. SHOT_WIDTH and SHOT_HEIGHT set the
# size, 1280 by 720 unless set.
#
# Unity draws with software OpenGL here, so a picture shows the layout and
# the colours of the game, but effects that need a real graphics card can
# look different or be missing.
set -eu
cd "$(dirname "$0")/../.."
. conventions/unity/env.sh
dir=${1:-${SCREENSHOT_DIR:-}}
if [ -z "$dir" ]; then
    echo "usage: sh conventions/unity/previews.sh DIR (or set SCREENSHOT_DIR)" >&2
    exit 2
fi
mkdir -p "$dir"
SCREENSHOT_DIR=$(cd "$dir" && pwd)
export SCREENSHOT_DIR
licence

# The picture taker is part of the project for this one run only. Unity
# compiles it with the other editor scripts.
helper=Assets/MaestroShots
if [ -e "$helper" ]; then
    echo "conventions: $helper already exists; it is reserved for the pictures." >&2
    exit 1
fi
mkdir -p "$helper/Editor"
cp conventions/unity/MaestroShots.cs "$helper/Editor/MaestroShots.cs"
log=$(mktemp)
tidy() { rm -rf "$helper" "$helper.meta" "$log"; }
trap tidy EXIT
status=0
unity_screen "${SHOT_WIDTH:-1280}x${SHOT_HEIGHT:-720}" -executeMethod MaestroShots.Run -logFile "$log" \
    >/dev/null 2>&1 || status=$?
grep -E 'MaestroShots:' "$log" | sed 's/^/unity: /' >&2 || true
if [ "$status" -ne 0 ]; then
    errors=$(grep -E ': error [A-Z]+[0-9]+' "$log" | sort -u | head -n 20 || true)
    [ -z "$errors" ] || printf '%s\n' "$errors" >&2
    [ -n "$errors" ] || tail -n 30 "$log" >&2
    echo "conventions: Unity could not take the pictures (exit $status)." >&2
    exit 1
fi
