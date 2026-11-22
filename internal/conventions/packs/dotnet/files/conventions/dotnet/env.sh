# Shared by the scripts next to it. Not a rule by itself.
# Inside the Maestro sandbox the home directory does not survive a step,
# so packages and the SDK's own files are kept in the workspace.
if [ -d /workspace ] && [ -w /workspace ]; then
    export NUGET_PACKAGES=/workspace/.cache/nuget
    export DOTNET_CLI_HOME=/workspace/.cache/dotnet
fi
export DOTNET_NOLOGO=1
export DOTNET_CLI_TELEMETRY_OPTOUT=1
export DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1
export MSBUILDDISABLENODEREUSE=1

if ! command -v dotnet >/dev/null 2>&1; then
    echo "conventions: the dotnet command is missing." >&2
    echo "In the Maestro sandbox: run \`maestro image\` again, it adds the .NET SDK when this pack is switched on." >&2
    exit 1
fi

# The one solution the checks run on.
solution() {
    found=$(ls ./*.slnx ./*.sln 2>/dev/null | head -n 1)
    if [ -z "$found" ]; then
        echo "conventions: no solution file (.sln or .slnx) in the root of the repository." >&2
        echo "Create one that contains every project: dotnet new sln, then dotnet sln add <project>." >&2
        exit 1
    fi
    echo "$found"
}
