#!/bin/sh
# Downloads the packages. This is the only script that needs the network.
# With --audit (used in CI) a package with a known vulnerability fails it.
set -eu
cd "$(dirname "$0")/../.."
. conventions/dotnet/env.sh
sln=$(solution)
if [ "${1:-}" = "--audit" ]; then
    dotnet restore "$sln"
else
    dotnet restore "$sln" -p:NuGetAudit=false
fi
