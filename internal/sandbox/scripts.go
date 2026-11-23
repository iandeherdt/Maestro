package sandbox

// The scripts a backend runs for the populate and export steps. Each takes
// its arguments as positional parameters, so no name is ever interpolated
// into shell text. They expect the workspace at /workspace, the
// repository in /workspace/repo, the mirror at /mirror and the hand-off
// folder at /handoff, wherever the backend keeps them on the host.

// BundleName is the file the export step writes in /handoff.
const BundleName = "out.bundle"

const CloneScript = `set -eu
git clone --quiet --no-hardlinks /mirror /workspace/repo
cd /workspace/repo
git checkout --quiet -b "$1" "origin/$2"
git config user.name "$3"
git config user.email "$4"
git rev-parse HEAD > .git/maestro-base
cat .git/maestro-base
`

const ProbeScript = `cat /workspace/repo/.git/maestro-base 2>/dev/null || true
`

const FastForwardScript = `set -eu
cd /workspace/repo
G="git -c core.hooksPath=/dev/null -c core.fsmonitor=false"
if ! $G ls-remote --exit-code --heads origin "refs/heads/$1" >/dev/null 2>&1; then
  # Nothing published. A workspace that holds no work yet starts again
  # from the newest base, so a fix to the base branch reaches it.
  base="$(cat .git/maestro-base)"
  if [ -n "${2:-}" ] && [ "$($G rev-parse HEAD)" = "$base" ] && [ -z "$($G status --porcelain)" ] &&
    $G fetch --quiet origin "refs/heads/$2" 2>/dev/null; then
    $G reset --quiet --hard FETCH_HEAD
    $G rev-parse HEAD > .git/maestro-base
  fi
  cat .git/maestro-base
  exit 0
fi
$G fetch --quiet origin "refs/heads/$1"
# Forward only. A branch that went another way is left as it is.
$G merge --quiet --ff-only FETCH_HEAD >/dev/null 2>&1 || true
cat .git/maestro-base
`

// ExportScript commits what is uncommitted, then looks at the working
// tree a second time with a git directory of its own. The workspace's
// .git belongs to the agent: its index can mark a changed file as
// unchanged (skip-worktree, assume-unchanged), its exclude file can hide
// new files, its settings can turn on filters or point git at another
// tree. The second git directory has none of that: an empty index, no
// settings and no exclude file, so every file is compared by content with
// the commit, and only a .gitignore that is itself in the commit hides a
// file. What it finds is committed, so the verify step runs on what is
// published and the diff policy sees it. $1 and $2 are the commit
// identity.
const ExportScript = `set -eu
cd /workspace/repo
G="git -c core.hooksPath=/dev/null -c core.fsmonitor=false"
if [ -n "$($G status --porcelain)" ]; then
  $G add -A
  $G commit --quiet --no-verify -m "maestro: uncommitted changes at end of turn"
fi
head="$($G rev-parse --verify HEAD^{commit})"
objects="$($G rev-parse --git-path objects)"
case "$objects" in /*) ;; *) objects="$PWD/$objects" ;; esac
check="$(mktemp -d)"
trap 'rm -rf "$check"' EXIT
git init --quiet "$check"
# Its objects are the workspace's, so what it commits is there at once.
c() {
  GIT_OBJECT_DIRECTORY="$objects" git --git-dir="$check/.git" --work-tree="$PWD" \
    -c core.hooksPath=/dev/null -c core.fsmonitor=false "$@"
}
c update-ref --no-deref HEAD "$head"
c read-tree "$head"
if [ -n "$(c status --porcelain)" ]; then
  c add -A
  tree="$(c write-tree)"
  new="$(c -c user.name="$1" -c user.email="$2" commit-tree "$tree" -p "$head" \
    -m "maestro: changes that git was told not to see")"
  $G update-ref HEAD "$new" "$head"
  $G reset --quiet
fi
base="$(cat .git/maestro-base)"
count="$($G rev-list --count "$base..HEAD")"
rm -f /handoff/out.bundle
if [ "$count" -gt 0 ]; then
  $G bundle create --quiet /handoff/out.bundle "$base..HEAD"
fi
echo "$($G rev-parse HEAD) $count"
`
