# Shared by the scripts next to it. Not a rule by itself.
. conventions/blender/settings.sh
BLENDER=${BLENDER:-blender}
if ! command -v "$BLENDER" >/dev/null 2>&1; then
    echo "conventions: Blender is missing (looked for $BLENDER)." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it adds Blender when this pack is switched on." >&2
    echo "Elsewhere: set BLENDER to the Blender program, for example /Applications/Blender.app/Contents/MacOS/Blender." >&2
    exit 1
fi
if [ ! -d "$ART_SOURCE" ]; then
    echo "conventions: there is no generator folder $ART_SOURCE (ART_SOURCE in conventions/blender/settings.sh)." >&2
    exit 1
fi

# The generators, by name: every .py file in ART_SOURCE whose name does
# not start with an underscore.
generators() {
    for f in "$ART_SOURCE"/*.py; do
        [ -f "$f" ] || continue
        n=$(basename "$f" .py)
        case "$n" in _*) continue ;; esac
        echo "$n"
    done
}

# Runs Blender without a screen, with the project's settings and nothing of
# the user's. A Python error ends it with a failure. The generators find
# the helper module maestro_art and their shared code on the path.
blender_run() {
    PYTHONPATH="$PWD/conventions/blender:$PWD/$ART_SOURCE" ART_MAX_TRIANGLES="$ART_MAX_TRIANGLES" \
        "$BLENDER" --background --factory-startup --python-use-system-env --python-exit-code 1 "$@"
}

# Runs one generator into an empty folder. Its output is in $log.
generate() { # name out preview log
    if [ -n "$3" ]; then
        blender_run --python "$ART_SOURCE/$1.py" -- --out "$2" --preview "$3" > "$4" 2>&1
    else
        blender_run --python "$ART_SOURCE/$1.py" -- --out "$2" > "$4" 2>&1
    fi
}
