#!/bin/sh
# Every check that decides whether a change is acceptable. Needs no network
# after prepare.sh. The Maestro service runs this itself before it pushes
# anything, and CI runs the same script.
set -eu

# The share of lines the tests have to cover, in percent, per test project.
COVERAGE_MIN=80

cd "$(dirname "$0")/../.."
. conventions/dotnet/env.sh
sln=$(solution)

echo "== format =="
dotnet format "$sln" --verify-no-changes --no-restore

echo "== build, analyzers =="
dotnet build "$sln" --no-restore --configuration Release

echo "== tests, coverage of at least ${COVERAGE_MIN}% in every test project =="
tested=0
for project in $(dotnet sln "$sln" list | grep 'proj$' | tr '\\' '/'); do
    if [ "$(dotnet msbuild "$project" -getProperty:IsTestProject)" != "true" ]; then
        continue
    fi
    tested=$((tested + 1))
    dotnet test "$project" --no-build --configuration Release \
        -p:CollectCoverage=true -p:Threshold="$COVERAGE_MIN" -p:ThresholdType=line -p:ThresholdStat=total \
        -p:CoverletOutput="$(mktemp -d)/"
done
if [ "$tested" -eq 0 ]; then
    echo "conventions: the solution has no test project, so nothing is covered. Add one." >&2
    exit 1
fi
