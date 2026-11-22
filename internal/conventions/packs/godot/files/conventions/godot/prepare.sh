#!/bin/sh
# Nothing to download: Godot is in the image. This imports the assets once,
# so the agent starts with Godot's cache built.
set -eu
cd "$(dirname "$0")/../.."
. conventions/godot/env.sh
godot_run --import > /dev/null 2>&1 || true
