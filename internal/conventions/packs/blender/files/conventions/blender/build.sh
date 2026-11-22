#!/bin/sh
# Runs the generators and puts what they make in ART_OUTPUT (or in $ART_OUT
# when it is set). With names, only those generators run:
#   sh conventions/blender/build.sh            every generator
#   sh conventions/blender/build.sh crate lamp only these
# Every generator runs into an empty folder of its own first, so one that
# writes nothing, or fails, is named.
set -eu
cd "$(dirname "$0")/../.."
. conventions/blender/env.sh
out=${ART_OUT:-$ART_OUTPUT}
mkdir -p "$out"
names=${*:-$(generators)}
if [ -z "$names" ]; then
    echo "There are no generators yet; nothing to build."
    exit 0
fi
work=$(mktemp -d)
failed=0
for n in $names; do
    if [ ! -f "$ART_SOURCE/$n.py" ]; then
        echo "conventions: there is no generator $ART_SOURCE/$n.py." >&2
        failed=1
        continue
    fi
    mkdir -p "$work/$n"
    if ! generate "$n" "$work/$n" "" "$work/$n.log"; then
        echo "FAILED $n:" >&2
        grep -E 'Error|Traceback|File "|^  ' "$work/$n.log" | tail -n 25 >&2 || tail -n 25 "$work/$n.log" >&2
        failed=1
        continue
    fi
    if [ -z "$(ls -A "$work/$n")" ]; then
        echo "FAILED $n: it wrote nothing into --out." >&2
        failed=1
        continue
    fi
    cp -R "$work/$n/." "$out/"
    # Which generator made which file, for the checks.
    if [ -n "${ART_MANIFEST:-}" ]; then
        (cd "$work/$n" && find . -type f | sed "s#^\./#$n #") >> "$ART_MANIFEST"
    fi
    echo "ok $n: $(ls "$work/$n" | tr '\n' ' ')"
done
rm -rf "$work"
exit "$failed"
