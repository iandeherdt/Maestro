#!/bin/sh
# Needs no licence and no network. Every asset under Assets/ has its .meta
# file, no .meta file is left without its asset, no two assets share a GUID,
# assets are saved as text, and the project is on the Editor in the image.
set -eu
cd "$(dirname "$0")/../.."
. conventions/unity/env.sh

failed=0
fail() { echo "conventions: $*" >&2; failed=1; }

echo "== Editor version =="
want=$(project_version)
have=$(installed_version)
if [ "$want" != "$have" ]; then
    fail "the project is saved with Unity $want, the Editor here is $have. Opening it would upgrade the project. The owner changes the version, by hand."
fi

echo "== assets as text, meta files visible =="
settings=ProjectSettings/EditorSettings.asset
if ! grep -q '^ *m_SerializationMode: 2' "$settings" 2>/dev/null; then
    fail "$settings must set Asset Serialization to Force Text (m_SerializationMode: 2), so scenes and prefabs can be read and reviewed."
fi
if grep -q '^ *m_ExternalVersionControlSupport: Hidden' "$settings" 2>/dev/null; then
    fail "$settings hides meta files. Set Version Control mode to Visible Meta Files."
fi

echo "== every asset has its .meta file =="
# Unity skips hidden files and folders, and folders ending in ~.
list=$(mktemp)
find Assets \( -name '.*' -o -name '*~' -o -name 'cvs' -o -name '*.tmp' \) -prune -o -print | sed '1d' | sort > "$list"
missing=$(grep -v '\.meta$' "$list" | while IFS= read -r p; do [ -f "$p.meta" ] || echo "$p"; done)
if [ -n "$missing" ]; then
    fail "these assets have no .meta file. Unity would create one with a new GUID, and references to the asset would break between machines:"
    echo "$missing" | sed 's/^/  /' >&2
fi
orphans=$(grep '\.meta$' "$list" | while IFS= read -r m; do [ -e "${m%.meta}" ] || echo "$m"; done)
if [ -n "$orphans" ]; then
    fail "these .meta files have no asset. Delete them with the asset they belonged to:"
    echo "$orphans" | sed 's/^/  /' >&2
fi

echo "== no GUID is used twice =="
dups=$(grep '\.meta$' "$list" | while IFS= read -r m; do
    g=$(sed -n 's/^guid: *//p' "$m" | tr -d '\r')
    if [ -n "$g" ]; then printf '%s %s\n' "$g" "$m"; fi
done | sort | awk '{ if ($1 == prev) { print prevline; print $0 } prev = $1; prevline = $0 }' | sort -u)
if [ -n "$dups" ]; then
    fail "these .meta files share a GUID. Give the copy a new one (a copied .meta file keeps the old GUID):"
    echo "$dups" | sed 's/^/  /' >&2
fi
rm -f "$list"
exit "$failed"
