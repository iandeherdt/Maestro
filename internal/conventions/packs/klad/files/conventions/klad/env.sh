# Shared by the scripts next to it. Not a rule by itself.
. conventions/klad/settings.sh
KLAD=${KLAD:-klad}
if ! command -v "$KLAD" >/dev/null 2>&1; then
    echo "conventions: klad is missing (looked for $KLAD)." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it builds klad when this pack is switched on." >&2
    exit 1
fi
have=$("$KLAD" --version | sed -n 's/^klad \([0-9][0-9.]*\).*/\1/p')
if [ -n "${KLAD_VERSION:-}" ] && [ "$have" != "$KLAD_VERSION" ]; then
    echo "conventions: this is klad $have, the project exports with klad $KLAD_VERSION (KLAD_VERSION in conventions/klad/settings.sh)." >&2
    echo "Pictures exported with another version can differ. Use klad $KLAD_VERSION, or change the version by hand and export every drawing again." >&2
    exit 1
fi
# A missing drawing folder is a project without drawings yet: Git keeps
# no empty folders, so a new project never has one.
# The drawings, as paths relative to KLAD_SOURCE without .klad.
drawings() {
    [ -d "$KLAD_SOURCE" ] || return 0
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
