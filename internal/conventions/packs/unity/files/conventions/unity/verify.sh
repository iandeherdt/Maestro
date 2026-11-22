#!/bin/sh
# Every check that decides whether a change is acceptable. Needs no network
# after prepare.sh. Unity itself compiles the scripts here; compile.sh is only
# the quick check for while working. The Maestro service runs this itself before it pushes
# anything, and CI runs the same script.
set -eu
cd "$(dirname "$0")/../.."
sh conventions/unity/meta.sh
. conventions/unity/env.sh
licence

work=$(mktemp -d)
total=0
for platform in EditMode PlayMode; do
    echo "== $platform tests, in Unity =="
    status=0
    unity -runTests -testPlatform "$platform" -testResults "$work/$platform.xml" -logFile "$work/$platform.log" \
        >/dev/null 2>&1 || status=$?
    if [ ! -f "$work/$platform.xml" ]; then
        grep -E 'error CS|Scripts have compiler errors' "$work/$platform.log" >&2 || tail -n 40 "$work/$platform.log" >&2
        echo "conventions: Unity could not run the $platform tests (exit $status)." >&2
        exit 1
    fi
    count=$(sed -n 's/.*<test-run [^>]*total="\([0-9]*\)".*/\1/p' "$work/$platform.xml" | head -n 1)
    total=$((total + ${count:-0}))
    if [ "$status" -ne 0 ]; then
        # The failed tests, with their messages.
        awk '/<test-case /{ name=$0; sub(/.*fullname="/, "", name); sub(/".*/, "", name) }
             /<test-case .*result="Failed"/{ print "FAILED " name }
             /<message>/{ if (name != "") { m=$0; gsub(/.*<message><!\[CDATA\[|\]\]><\/message>.*/, "", m); print "  " m } }' \
            "$work/$platform.xml" >&2
        echo "conventions: $platform tests failed (exit $status)." >&2
        exit 1
    fi
done
if [ "$total" -eq 0 ]; then
    echo "conventions: the project has no tests. Add an EditMode test assembly under Assets/Tests." >&2
    exit 1
fi
echo "$total tests passed."
rm -rf "$work"
