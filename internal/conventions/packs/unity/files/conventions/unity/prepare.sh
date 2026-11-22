#!/bin/sh
# Opens the project once in Unity: resolves the packages (the only step
# that needs the network) and imports the assets, so the checks after it
# are fast. Compile errors are not this script's business; verify.sh
# reports them.
set -eu
cd "$(dirname "$0")/../.."
. conventions/unity/env.sh
licence
log=$(mktemp)
status=0
unity -quit -logFile "$log" >/dev/null 2>&1 || status=$?
if grep -qE 'No valid Unity Editor license|License is not active|Licensing::.*Error: .*(expired|revoked)' "$log"; then
    grep -E 'licen[cs]e' "$log" | tail -n 5 >&2
    echo "conventions: Unity did not accept the licence. Run \`maestro unity activate\` again." >&2
    exit 1
fi
if grep -qE 'An error occurred while resolving packages|Cannot resolve package' "$log"; then
    grep -A 5 -E 'An error occurred while resolving packages|Cannot resolve package' "$log" >&2
    exit 1
fi
if [ "$status" -ne 0 ] && ! grep -q 'error CS' "$log"; then
    tail -n 40 "$log" >&2
    exit "$status"
fi
rm -f "$log"
