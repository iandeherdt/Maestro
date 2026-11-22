#!/bin/sh
# Exports the drawings to KLAD_OUTPUT (or to $KLAD_OUT when it is set).
#   sh conventions/klad/build.sh                 every drawing
#   sh conventions/klad/build.sh props/crate     only these
set -eu
cd "$(dirname "$0")/../.."
. conventions/klad/env.sh
out=${KLAD_OUT:-$KLAD_OUTPUT}
names=${*:-$(drawings)}
if [ -z "$names" ]; then
    echo "There are no drawings yet; nothing to build."
    exit 0
fi
failed=0
for n in $names; do
    n=${n%.klad}
    if [ ! -d "$KLAD_SOURCE/$n.klad" ]; then
        echo "conventions: there is no drawing $KLAD_SOURCE/$n.klad." >&2
        failed=1
        continue
    fi
    if export_one "$n" "$out/$n.png" > /dev/null; then
        echo "ok $n"
    else
        echo "FAILED $n" >&2
        failed=1
    fi
done
exit "$failed"
