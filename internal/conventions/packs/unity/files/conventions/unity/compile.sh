#!/bin/sh
# Needs no licence and no network: compiles every script under Assets/ with
# the C# compiler that comes with the Editor, against Unity's own assemblies.
# Fast, and enough to catch most mistakes while working. It is not the last
# word: verify.sh compiles with Unity itself. The assemblies of the packages
# are found once Unity has opened the project (prepare.sh does that);
# before, a script that uses a package fails to compile here.
set -eu
cd "$(dirname "$0")/../.."
. conventions/unity/env.sh

out=$(mktemp -d)
rsp=$out/compile.rsp
# One assembly per assembly definition under Assets/ is built by Unity.
# Here they are compiled together, so they are left out of the references.
own=$(find Assets -name '*.asmdef' -exec sed -n 's/^[[:space:]]*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' {} \;)
{
    echo "-nologo -target:library -nostdlib -langversion:9 -nowarn:0169,0414,0649 -out:$out/check.dll"
    printf -- '-define:%s\n' UNITY_EDITOR UNITY_EDITOR_64 UNITY_EDITOR_LINUX UNITY_STANDALONE UNITY_STANDALONE_LINUX \
        ENABLE_MONO NET_STANDARD NET_STANDARD_2_0 NET_STANDARD_2_1 CSHARP_7_3_OR_NEWER UNITY_5_3_OR_NEWER \
        UNITY_2017_1_OR_NEWER UNITY_2018_1_OR_NEWER UNITY_2019_1_OR_NEWER UNITY_2020_1_OR_NEWER \
        UNITY_2021_1_OR_NEWER UNITY_2022_1_OR_NEWER UNITY_2023_1_OR_NEWER UNITY_6000_0_OR_NEWER
    for f in "$UNITY_DATA"/Managed/UnityEngine/*.dll "$UNITY_DATA"/NetStandard/ref/2.1.0/netstandard.dll "$UNITY_DATA"/NetStandard/compat/2.1.0/shims/netfx/*.dll \
        "$UNITY_DATA"/Resources/PackageManager/BuiltInPackages/com.unity.ext.nunit/net40/unity-custom/nunit.framework.dll; do
        if [ -f "$f" ]; then echo "-r:$f"; fi
    done
    if [ -d Library/ScriptAssemblies ]; then
        for f in Library/ScriptAssemblies/*.dll; do
            [ -f "$f" ] || continue
            name=$(basename "$f" .dll)
            case "$name" in Assembly-CSharp*) continue ;; esac
            if echo "$own" | grep -qx "$name"; then continue; fi
            echo "-r:$f"
        done
    fi
    # Only .NET assemblies: a plugin folder can also hold native libraries
    # for other platforms, which end in .dll too. A .NET assembly carries
    # the metadata signature BSJB.
    find Assets Library/PackageCache \( -name '.*' -o -name '*~' \) -prune -o -name '*.dll' -print 2>/dev/null |
        grep -v '^Library/PackageCache/.*/Tests/' | while IFS= read -r f; do
            if LC_ALL=C grep -q -a BSJB "$f"; then echo "-r:$f"; fi
        done
    find Assets \( -name '.*' -o -name '*~' \) -prune -o -name '*.cs' -print
} > "$rsp"

if ! grep -q '\.cs$' "$rsp"; then
    echo "== compile: no scripts under Assets/ =="
    rm -rf "$out"
    exit 0
fi
[ -d Library/ScriptAssemblies ] || echo "compile: the packages are not resolved yet, so only Unity's own assemblies are referenced." >&2
echo "== compile, without Unity =="
if [ ! -x "$UNITY_DATA/NetCoreRuntime/dotnet" ] || [ ! -f "$UNITY_DATA/DotNetSdkRoslyn/csc.dll" ]; then
    echo "conventions: the Editor in $UNITY_EDITOR has no C# compiler where it is expected ($UNITY_DATA/DotNetSdkRoslyn)." >&2
    rm -rf "$out"
    exit 1
fi
status=0
"$UNITY_DATA/NetCoreRuntime/dotnet" "$UNITY_DATA/DotNetSdkRoslyn/csc.dll" "@$rsp" > "$out/log" 2>&1 || status=$?
grep -E ': error ' "$out/log" >&2 || true
[ "$status" -eq 0 ] || grep -qE ': error ' "$out/log" || cat "$out/log" >&2
rm -rf "$out"
exit "$status"
