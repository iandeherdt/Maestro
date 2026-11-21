#!/bin/sh
# Installs exactly what the lockfile says. This is the only script that
# needs the network. With --audit (used in CI) a package with a known
# vulnerability of high or critical severity fails it.
set -eu
cd "$(dirname "$0")/../.."
. conventions/typescript/env.sh
if [ "$PM" = pnpm ]; then
    pnpm install --frozen-lockfile
else
    npm ci
fi
if [ "${1:-}" = "--audit" ]; then
    if [ "$PM" = pnpm ]; then
        pnpm audit --audit-level high
    else
        npm audit --audit-level=high
    fi
fi
