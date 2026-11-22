#!/bin/sh
# Nothing to download: klad is in the image. This only says early, before
# the agent starts, when klad or the drawing folder is missing.
set -eu
cd "$(dirname "$0")/../.."
. conventions/klad/env.sh
"$KLAD" --version
