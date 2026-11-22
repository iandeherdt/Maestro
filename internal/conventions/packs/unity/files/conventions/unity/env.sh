# Shared by the scripts next to it. Not a rule by itself.
UNITY_ROOT=${UNITY_PATH:-/opt/unity}
UNITY_EDITOR=$UNITY_ROOT/Editor
UNITY_DATA=$UNITY_EDITOR/Data

if [ ! -x "$UNITY_EDITOR/Unity" ]; then
    echo "conventions: the Unity Editor is missing (looked in $UNITY_EDITOR)." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it adds the Unity Editor when this pack is switched on." >&2
    exit 1
fi

if [ ! -f ProjectSettings/ProjectVersion.txt ]; then
    echo "conventions: no Unity project in the root of the repository (ProjectSettings/ProjectVersion.txt is missing)." >&2
    exit 1
fi

# The Editor version in the image, and the one the project was saved with.
installed_version() { cat "$UNITY_ROOT/version" 2>/dev/null || echo unknown; }
project_version() { sed -n 's/^m_EditorVersion: *//p' ProjectSettings/ProjectVersion.txt | tr -d '\r'; }

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
    if [ -z "$(find "$HOME/.local/share/unity3d/Unity" "$HOME/.config/unity3d/Unity/licenses" -type f 2>/dev/null | head -n 1)" ]; then
        echo "conventions: there is no Unity licence, so Unity cannot open the project or run its tests." >&2
        echo "On the machine that runs Maestro: maestro unity activate. In CI: the UNITY_LICENSE secret." >&2
        echo "The quick check that needs no licence is: sh conventions/unity/check.sh" >&2
        exit 1
    fi
}

# Runs the Editor without a screen. Logs go to the file named by the caller.
unity() {
    LD_PRELOAD=$([ -f /usr/lib/game-ci/libsem_eintr_retry.so ] && echo /usr/lib/game-ci/libsem_eintr_retry.so) \
        xvfb-run -a -s "-screen 0 1024x768x24" "$UNITY_EDITOR/Unity" -batchmode -nographics -projectPath "$PWD" "$@"
}
