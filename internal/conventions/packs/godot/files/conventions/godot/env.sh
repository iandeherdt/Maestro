# Shared by the scripts next to it. Not a rule by itself.
GODOT=${GODOT:-godot}
if ! command -v "$GODOT" >/dev/null 2>&1; then
    echo "conventions: Godot is missing (looked for $GODOT)." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it adds Godot when this pack is switched on." >&2
    echo "Elsewhere: set GODOT to the Godot program." >&2
    exit 1
fi
if [ ! -f project.godot ]; then
    echo "conventions: no Godot project in the root of the repository (project.godot is missing)." >&2
    exit 1
fi
# Godot keeps its settings in the home directory, which does not survive a
# step in the Maestro sandbox. Nothing of it matters to the checks.
export HOME=${HOME:-/tmp}

# Runs Godot without a screen or sound, quietly.
godot_run() { "$GODOT" --headless --audio-driver Dummy "$@"; }

# Every GDScript file of the project, without addons and Godot's own folder.
scripts() {
    find . \( -path ./.godot -o -path ./addons -o -name '.*' ! -name . \) -prune -o -name '*.gd' -print | sed 's#^\./##' | sort
}

# The engine version in the image and the one the project is made for.
installed_version() { "$GODOT" --version 2>/dev/null | grep -E '^[0-9]' | sed -n 's/^\([0-9]*\.[0-9]*\).*/\1/p'; }
project_version() { sed -n 's/^config\/features=PackedStringArray("\([0-9]*\.[0-9]*\)".*/\1/p' project.godot; }

# Imports the assets and builds Godot's cache of named classes, which the
# script check needs. The output goes to the file named.
import_project() { godot_run --import > "$1" 2>&1; }
