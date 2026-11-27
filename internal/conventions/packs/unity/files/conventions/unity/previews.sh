#!/bin/sh
# Takes pictures of the game for the pull request: into $SCREENSHOT_DIR
# (Maestro's screenshots command) or the folder given. Each scene of the
# build (or the scenes given in SHOT_SCENES, separated by ':') is played for
# two seconds and then drawn, at most 6. SHOT_WIDTH and SHOT_HEIGHT set the
# size, 1280 by 720 unless set. The project's picture set-ups (a class named
# MaestroShotSetups, see MaestroShots.cs) are drawn first; SHOT_SHOW (names
# separated by '|', from the agent's MAESTRO_SHOW lines) picks set-ups and
# scenes.
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
tidy() { rm -rf "$helper" "$helper.meta" "$log" "$log.first"; }
trap tidy EXIT

# One run of the Editor, its log in $log and its exit in $status.
shoot() {
    status=0
    unity_screen "${SHOT_WIDTH:-1280}x${SHOT_HEIGHT:-720}" -executeMethod MaestroShots.Run -logFile "$log" \
        >/dev/null 2>&1 || status=$?
    grep -E 'MaestroShots:' "$log" | sed 's/^/unity: /' >&2 || true
}

shoot
if [ "$status" -ge 128 ]; then
    # The Editor crashed. Software OpenGL compiles its drawing code for the
    # processor it finds, which goes wrong where the processor is emulated
    # (an Intel image on an Apple Mac). Mesa's simpler renderer compiles
    # nothing, so it is tried once more with that.
    echo "conventions: Unity crashed (exit $status); trying once more with the simpler software renderer." >&2
    cp "$log" "$log.first" 2>/dev/null || true
    GALLIUM_DRIVER=softpipe LP_NATIVE_VECTOR_WIDTH=128
    export GALLIUM_DRIVER LP_NATIVE_VECTOR_WIDTH
    shoot
fi
if [ "$status" -ne 0 ]; then
    # The whole log stays in the workspace for `maestro shell`.
    mkdir -p Logs && cp "$log" Logs/maestro-shots.log 2>/dev/null || true
    [ ! -f "$log.first" ] || cp "$log.first" Logs/maestro-shots-first.log 2>/dev/null || true
    errors=$(grep -E ': error [A-Z]+[0-9]+' "$log" | sort -u | head -n 20 || true)
    # A crash: what Unity printed just before its stack, which names the
    # signal and where it came from. The stack itself says little.
    at=$(grep -n -m 1 -E 'Obtained [0-9]+ stack frames|Native Crash Reporting|Received signal|Caught fatal signal' "$log" | cut -d: -f1 || true)
    if [ -n "$errors" ]; then
        printf '%s\n' "$errors" >&2
    elif [ -n "$at" ]; then
        from=$((at > 12 ? at - 12 : 1))
        sed -n "${from},$((at + 2))p" "$log" >&2
    else
        tail -n 15 "$log" >&2
    fi
    echo "conventions: Unity could not take the pictures (exit $status). The whole log is Logs/maestro-shots.log in the workspace." >&2
    exit 1
fi
