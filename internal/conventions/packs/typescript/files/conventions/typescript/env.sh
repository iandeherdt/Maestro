# Shared by the scripts next to it. Not a rule by itself.
# Inside the Maestro sandbox the home directory does not survive a step,
# so the package cache is kept in the workspace.
if [ -d /workspace ] && [ -w /workspace ]; then
    export npm_config_cache=/workspace/.cache/npm
    export npm_config_store_dir=/workspace/.cache/pnpm
fi
export npm_config_update_notifier=false
export npm_config_fund=false
export NEXT_TELEMETRY_DISABLED=1

# The package manager is the one whose lockfile is committed.
if [ -f pnpm-lock.yaml ]; then
    PM=pnpm
elif [ -f package-lock.json ]; then
    PM=npm
else
    echo "conventions: no package-lock.json or pnpm-lock.yaml in the root of the repository." >&2
    echo "Run npm install (or pnpm install) once and commit the lockfile." >&2
    exit 1
fi

if ! command -v "$PM" >/dev/null 2>&1; then
    echo "conventions: the $PM command is missing." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it adds pnpm when this pack is switched on." >&2
    exit 1
fi

# run <tool> [arguments]: runs a tool that the project has installed, never one from the network.
run() {
    if [ "$PM" = pnpm ]; then
        pnpm exec "$@"
    else
        npx --no-install "$@"
    fi
}

# uses <package>: whether package.json lists it.
uses() {
    node -e '
        const p = require("./package.json");
        process.exit(process.argv[1] in { ...p.dependencies, ...p.devDependencies } ? 0 : 1);
    ' "$1"
}
