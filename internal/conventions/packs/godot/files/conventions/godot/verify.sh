#!/bin/sh
# Every check that decides whether a change is acceptable. Needs no network.
# The Maestro service runs this itself before it pushes anything, and CI
# runs the same script.
set -eu
cd "$(dirname "$0")/../.."
sh conventions/godot/check.sh
. conventions/godot/env.sh

echo "== every asset is imported, with its files committed =="
log=$(mktemp)
import_project "$log" || { tail -n 30 "$log" >&2; exit 1; }
grep -E 'SCRIPT ERROR|^ERROR: .*(res://|Failed|Cannot)' "$log" | grep -v fontconfig >&2 && { echo "conventions: the import reported the errors above." >&2; exit 1; }
if command -v git >/dev/null 2>&1 && [ -d .git ]; then
    made=$(git status --porcelain --untracked-files=all | sed -n 's/^?? //p' | grep -E '\.(import|uid)$' || true)
    if [ -n "$made" ]; then
        echo "conventions: Godot made these files on import. Commit them with the files they belong to, or references break between machines:" >&2
        echo "$made" | sed 's/^/  /' >&2
        exit 1
    fi
fi

echo "== tests (GUT) =="
if [ ! -f addons/gut/gut_cmdln.gd ]; then
    echo "conventions: the GUT test framework is missing (addons/gut). The owner adds it; see conventions/godot/RULES.md." >&2
    exit 1
fi
dir=test
xml=$(mktemp)
status=0
godot_run --script res://addons/gut/gut_cmdln.gd -gdir="res://$dir" -ginclude_subdirs -gexit -glog=1 \
    -gjunit_xml_file="$xml" > "$log" 2>&1 || status=$?
grep -E '^\[Failed\]|FAILED|Passing|Failing|Tests|Asserts|SCRIPT ERROR' "$log" | tail -n 30
count=$(sed -n 's/.*<testsuites[^>]* tests="\([0-9]*\)".*/\1/p' "$xml" | head -n 1)
fails=$(sed -n 's/.*<testsuites[^>]* failures="\([0-9]*\)".*/\1/p' "$xml" | head -n 1)
if [ "$status" -ne 0 ] || [ "${fails:-0}" -ne 0 ]; then
    echo "conventions: tests failed." >&2
    exit 1
fi
if [ "${count:-0}" -eq 0 ]; then
    echo "conventions: the project has no tests. Add one under $dir/, named test_*.gd." >&2
    exit 1
fi
echo "${count} tests passed."
rm -f "$log" "$xml"
