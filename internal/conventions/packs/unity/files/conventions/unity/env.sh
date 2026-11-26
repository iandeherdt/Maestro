# Shared by the scripts next to it. Not a rule by itself.
if [ ! -f ProjectSettings/ProjectVersion.txt ]; then
    echo "conventions: no Unity project in the root of the repository (ProjectSettings/ProjectVersion.txt is missing)." >&2
    exit 1
fi
project_version() { sed -n 's/^m_EditorVersion: *//p' ProjectSettings/ProjectVersion.txt | tr -d '\r'; }

UNITY_ROOT=${UNITY_PATH:-/opt/unity}
UNITY_EDITOR=$UNITY_ROOT/Editor
UNITY_DATA=$UNITY_EDITOR/Data
UNITY_VERSION=$(cat "$UNITY_ROOT/version" 2>/dev/null || echo unknown)

# On a Mac, outside the sandbox: the Editor that Unity Hub installed for the
# project's version, in Hub's own folder or in the one set in Hub's settings.
if [ -z "${UNITY_PATH:-}" ] && [ ! -x "$UNITY_EDITOR/Unity" ] && [ "$(uname)" = Darwin ]; then
    v=$(project_version)
    hub=$(sed -n 's/^ *"\(.*\)" *$/\1/p' "$HOME/Library/Application Support/UnityHub/secondaryInstallPath.json" 2>/dev/null || true)
    for d in ${hub:+"$hub"} /Applications/Unity/Hub/Editor; do
        if [ -n "$v" ] && [ -x "$d/$v/Unity.app/Contents/MacOS/Unity" ]; then
            UNITY_EDITOR=$d/$v/Unity.app/Contents/MacOS
            UNITY_DATA=$d/$v/Unity.app/Contents
            UNITY_VERSION=$v
            break
        fi
    done
    if [ ! -x "$UNITY_EDITOR/Unity" ]; then
        echo "conventions: Unity $v, the version of this project, is not installed with Unity Hub on this Mac." >&2
        echo "Install it with Unity Hub (Installs, Install Editor), then run this again." >&2
        exit 1
    fi
fi

if [ ! -x "$UNITY_EDITOR/Unity" ]; then
    echo "conventions: the Unity Editor is missing (looked in $UNITY_EDITOR)." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it adds the Unity Editor when this pack is switched on." >&2
    exit 1
fi

# The version of the Editor found above; project_version is the one the
# project was saved with.
installed_version() { echo "$UNITY_VERSION"; }

# Puts the licence where the Editor looks for it. In the Maestro sandbox the
# service mounts the activated licence read-only at /unity-licence, in the
# repository checks and the setup steps only, never in the agent's own
# container. In CI the licence file comes from the UNITY_LICENSE secret.
# The home directory does not survive a step, so this runs every time.
licence() {
    if [ -d /unity-licence ]; then
        cp -R /unity-licence/. "$HOME/"
    fi
    if [ -n "${UNITY_LICENSE:-}" ]; then
        mkdir -p "$HOME/.local/share/unity3d/Unity"
        printf '%s' "$UNITY_LICENSE" > "$HOME/.local/share/unity3d/Unity/Unity_lic.ulf"
    fi
    if [ -z "$(find "$HOME/.local/share/unity3d/Unity" "$HOME/.config/unity3d/Unity/licenses" "$HOME/Library/Unity/licenses" -type f 2>/dev/null | head -n 1)" ]; then
        echo "conventions: there is no Unity licence, so Unity cannot open the project or run its tests." >&2
        echo "On the machine that runs Maestro: maestro unity activate. In CI: the UNITY_LICENSE secret." >&2
        echo "The quick check that needs no licence is: sh conventions/unity/check.sh" >&2
        exit 1
    fi
}

# Runs the Editor without a screen. Logs go to the file named by the caller.
unity() {
    if [ "$(uname)" = Darwin ] && ! command -v xvfb-run >/dev/null 2>&1; then
        # A Mac needs no X server: the Editor runs in batch mode as it is.
        "$UNITY_EDITOR/Unity" -batchmode -nographics -projectPath "$PWD" "$@"
        return
    fi
    LD_PRELOAD=$([ -f /usr/lib/game-ci/libsem_eintr_retry.so ] && echo /usr/lib/game-ci/libsem_eintr_retry.so) \
        xvfb-run -a -s "-screen 0 1024x768x24" "$UNITY_EDITOR/Unity" -batchmode -nographics -projectPath "$PWD" "$@"
}

# Runs the Editor with graphics, for pictures: batch mode, but it draws. On
# Linux on a virtual screen of the size given first (WIDTHxHEIGHT), with
# software OpenGL.
unity_screen() {
    size=$1
    shift
    if [ "$(uname)" = Darwin ]; then
        # A Mac draws with its own graphics (Metal) in batch mode, without
        # an X server, even where Maestro put an xvfb-run in the PATH.
        "$UNITY_EDITOR/Unity" -batchmode -projectPath "$PWD" "$@"
        return
    fi
    LIBGL_ALWAYS_SOFTWARE=1 LD_PRELOAD=$([ -f /usr/lib/game-ci/libsem_eintr_retry.so ] && echo /usr/lib/game-ci/libsem_eintr_retry.so) \
        xvfb-run -a -s "-screen 0 ${size}x24" "$UNITY_EDITOR/Unity" -batchmode -force-glcore -projectPath "$PWD" "$@"
}
