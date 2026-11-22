#!/bin/sh
# The quick checks: they need no licence and no network, so they also run
# in the agent's own container. Run them while working; verify.sh runs them
# too, and then lets Unity itself compile and test the project.
set -eu
cd "$(dirname "$0")/../.."
sh conventions/unity/meta.sh
sh conventions/unity/compile.sh
