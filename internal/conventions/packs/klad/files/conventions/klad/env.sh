# Shared by the scripts next to it. Not a rule by itself.
. conventions/klad/settings.sh
KLAD=${KLAD:-klad}
if ! command -v "$KLAD" >/dev/null 2>&1; then
    echo "conventions: klad is missing (looked for $KLAD)." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it builds klad when this pack is switched on." >&2
    exit 1
fi
if [ ! -d "$KLAD_SOURCE" ]; then
    echo "conventions: there is no drawing folder $KLAD_SOURCE (KLAD_SOURCE in conventions/klad/settings.sh)." >&2
    exit 1
fi
# The drawings, as paths relative to KLAD_SOURCE without .klad.
drawings() {
    (cd "$KLAD_SOURCE" && find . -type d -name '*.klad' -prune -print | sed 's#^\./##; s#\.klad$##' | sort)
}

# Exports one drawing to a PNG file. A drawing may have extra export
# options in a file next to it, NAME.export, one line (for example
# --sheet all --scale 0.5 for an animation).
export_one() { # name out.png
    extra=""
    if [ -f "$KLAD_SOURCE/$1.export" ]; then
        extra=$(head -n 1 "$KLAD_SOURCE/$1.export")
    fi
    mkdir -p "$(dirname "$2")"
    # shellcheck disable=SC2086
    "$KLAD" export "$KLAD_SOURCE/$1.klad" --scale "$KLAD_SCALE" $extra -o "$2"
}
