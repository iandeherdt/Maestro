#!/bin/sh
# Writes the .meta file, with a new GUID, for every file and folder under
# Assets/ that has none, the way the Unity Editor would on import. Give
# paths to limit it to those; with none it fills in the whole of Assets/.
# Existing .meta files are never touched: their GUIDs are what other
# assets refer to.
#
#   sh conventions/unity/new-meta.sh                 # everything missing
#   sh conventions/unity/new-meta.sh Assets/Scripts/Player.cs
set -eu
cd "$(dirname "$0")/../.."

guid() { od -An -tx1 -N16 /dev/urandom | tr -d ' \n'; }

common='  externalObjects: {}
  userData: 
  assetBundleName: 
  assetBundleVariant: '

body() {
    case "$1" in
    *.cs)
        printf 'MonoImporter:\n  externalObjects: {}\n  serializedVersion: 2\n  defaultReferences: []\n  executionOrder: 0\n  icon: {instanceID: 0}\n  userData: \n  assetBundleName: \n  assetBundleVariant: \n' ;;
    *.asmdef|*.asmref)
        printf 'AssemblyDefinitionImporter:\n%s\n' "$common" ;;
    *.prefab)
        printf 'PrefabImporter:\n%s\n' "$common" ;;
    *.mat)
        printf 'NativeFormatImporter:\n  externalObjects: {}\n  mainObjectFileID: 2100000\n  userData: \n  assetBundleName: \n  assetBundleVariant: \n' ;;
    *.asset)
        printf 'NativeFormatImporter:\n  externalObjects: {}\n  mainObjectFileID: 11400000\n  userData: \n  assetBundleName: \n  assetBundleVariant: \n' ;;
    *.txt|*.json|*.xml|*.csv|*.yaml|*.bytes|*.md)
        printf 'TextScriptImporter:\n%s\n' "$common" ;;
    *.unity|*.controller|*.anim|*.overrideController|*.mask|*.mixer|*.physicMaterial|*.renderTexture|*.shader|*.compute|*.hlsl|*.cginc|*.inputactions|*.uss|*.uxml|*.tss)
        printf 'DefaultImporter:\n%s\n' "$common" ;;
    *)
        return 1 ;;
    esac
}

write() {
    p=$1
    [ -e "$p.meta" ] && return 0
    case "$p" in
    Assets/*) ;;
    *) echo "new-meta: $p is not under Assets/" >&2; return 1 ;;
    esac
    if [ -d "$p" ]; then
        printf 'fileFormatVersion: 2\nguid: %s\nfolderAsset: yes\nDefaultImporter:\n%s\n' "$(guid)" "$common" > "$p.meta"
    elif text=$(body "$p"); then
        printf 'fileFormatVersion: 2\nguid: %s\n%s\n' "$(guid)" "$text" > "$p.meta"
    else
        echo "new-meta: no .meta written for $p: Unity imports this kind of file with settings of its own. Textures, models and audio are added by the owner in the Editor." >&2
        return 1
    fi
    echo "wrote $p.meta"
}

failed=0
if [ "$#" -eq 0 ]; then
    set -- Assets
fi
for start in "$@"; do
    start=${start%/}
    list=$(find "$start" \( -name '.*' -o -name '*~' -o -name 'cvs' -o -name '*.tmp' \) -prune -o -print | grep -v '\.meta$' | grep -vx 'Assets' || true)
    # Parent folders first, so every folder on the way has its .meta too.
    dir=$(dirname "$start")
    while [ "$dir" != "Assets" ] && [ "$dir" != "." ] && [ "$dir" != "/" ]; do
        list="$dir
$list"
        dir=$(dirname "$dir")
    done
    todo=$(mktemp)
    printf '%s\n' "$list" | sed '/^$/d' | sort -u > "$todo"
    while IFS= read -r p; do
        write "$p" || failed=1
    done < "$todo"
    rm -f "$todo"
done
exit "$failed"
