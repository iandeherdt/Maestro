#!/bin/sh
# Nothing to download: Blender is in the image. This only says early, before
# the agent starts, when Blender or the generator folder is missing.
set -eu
cd "$(dirname "$0")/../.."
. conventions/blender/env.sh
"$BLENDER" --version | head -n 1
