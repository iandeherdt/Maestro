#!/bin/sh
# The quick checks, while working: the scripts parse, are formatted and pass
# the linter. verify.sh runs them too.
set -eu
cd "$(dirname "$0")/../.."
. conventions/godot/env.sh
failed=0

echo "== Godot version =="
want=$(project_version)
have=$(installed_version)
if [ -n "$want" ] && [ "$want" != "$have" ]; then
    echo "conventions: the project is made for Godot $want, the engine here is $have. The owner changes the version, by hand." >&2
    failed=1
fi

echo "== import =="
ilog=$(mktemp)
if ! import_project "$ilog"; then
    tail -n 30 "$ilog" >&2
    exit 1
fi
rm -f "$ilog"

echo "== every script parses =="
for s in $(scripts); do
    if ! out=$(godot_run --check-only --script "res://$s" 2>&1); then
        echo "$out" | grep -E 'SCRIPT ERROR|at: ' >&2 || echo "$out" | tail -n 5 >&2
        failed=1
    fi
done

if command -v gdformat >/dev/null 2>&1; then
    echo "== formatted (gdformat) =="
    list=$(scripts)
    if [ -n "$list" ]; then
        # shellcheck disable=SC2086
        gdformat --check $list || { echo "conventions: run gdformat on the files above." >&2; failed=1; }
        echo "== lint (gdlint) =="
        # shellcheck disable=SC2086
        gdlint $list || failed=1
    fi
else
    echo "conventions: gdformat and gdlint are missing; run \`maestro image\` again." >&2
    failed=1
fi
exit "$failed"
