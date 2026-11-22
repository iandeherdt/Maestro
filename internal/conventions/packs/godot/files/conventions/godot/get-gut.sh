#!/bin/sh
# Puts the GUT test framework into addons/gut, at the version the checks
# are made for, checked by its commit. Run it once in a new project, on
# your own machine, and commit addons/gut. The agent cannot change it.
set -eu
cd "$(dirname "$0")/../.."
version=v9.7.1
commit=aeb5d4f3f7f0a6c9b5e178876d6c99b791fda605

if [ -e addons/gut ]; then
    echo "addons/gut is already there; nothing to do. To change its version, remove it first."
    exit 0
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$version" https://github.com/bitwes/Gut.git "$tmp/gut"
got=$(git -C "$tmp/gut" rev-parse HEAD)
if [ "$got" != "$commit" ]; then
    echo "get-gut: the tag $version is at commit $got, not $commit. Not installed." >&2
    exit 1
fi
mkdir -p addons
cp -R "$tmp/gut/addons/gut" addons/gut
echo "GUT $version is in addons/gut. Commit it: git add addons/gut"
