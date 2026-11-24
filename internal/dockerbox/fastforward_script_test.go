package dockerbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// The fast-forward script, run on this machine against a stand-in for the
// mirror, so that it is tested where there is no container engine too.
func runFastForward(t *testing.T, repo, branch, base string) string {
	t.Helper()
	script := strings.ReplaceAll(sandbox.FastForwardScript, "/workspace/repo", repo)
	cmd := exec.Command("sh", "-c", script, "fast-forward", branch, base)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fast-forward: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// fastForwardRepos makes a mirror with main and a published work branch,
// and a workspace cloned from it on that branch.
func fastForwardRepos(t *testing.T) (mirror, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	mirror, repo = filepath.Join(root, "mirror"), filepath.Join(root, "repo")
	repoSh(t, root, `git init -q -b main mirror && cd mirror &&
		printf 'one\ntwo\nthree\n' > a.txt && git add -A && git commit -qm base &&
		git checkout -q -b maestro/GH-1 && printf 'one\nTWO by the agent\nthree\n' > a.txt && git commit -qam work &&
		git checkout -q main`)
	repoSh(t, root, `git clone -q mirror repo && cd repo && git checkout -q maestro/GH-1 &&
		git config user.name agent && git config user.email agent@example.com &&
		git rev-parse main > .git/maestro-base`)
	return mirror, repo
}

func TestFastForwardMergesTheNewestBase(t *testing.T) {
	mirror, repo := fastForwardRepos(t)
	repoSh(t, mirror, `echo other > b.txt && git add b.txt && git commit -qm "other change"`)
	newMain := repoSh(t, mirror, `git rev-parse main`)
	if got := runFastForward(t, repo, "maestro/GH-1", "main"); got != newMain {
		t.Fatalf("base %q, want the newest main %s", got, newMain)
	}
	if got := repoSh(t, repo, `cat b.txt; git log -1 --format=%s; git status --porcelain; git diff --stat main..HEAD`); !strings.HasPrefix(got,
		"other\nMerge main into maestro/GH-1\n a.txt | 2 +-") {
		t.Fatalf("after the merge:\n%s", got)
	}
	// Up to date: nothing changes.
	head := repoSh(t, repo, `git rev-parse HEAD`)
	if got := runFastForward(t, repo, "maestro/GH-1", "main"); got != newMain || repoSh(t, repo, `git rev-parse HEAD`) != head {
		t.Fatalf("a second run changed something: %q", got)
	}
}

func TestFastForwardLeavesAConflictForTheAgent(t *testing.T) {
	mirror, repo := fastForwardRepos(t)
	repoSh(t, mirror, `printf 'one\nTWO by someone else\nthree\n' > a.txt && git commit -qam "same line"`)
	newMain := repoSh(t, mirror, `git rev-parse main`)
	if got := runFastForward(t, repo, "maestro/GH-1", "main"); got != newMain {
		t.Fatalf("base %q, want %s", got, newMain)
	}
	if got := repoSh(t, repo, `test -f .git/MERGE_HEAD && git status --porcelain`); got != "UU a.txt" {
		t.Fatalf("status %q", got)
	}
	// A run while the conflict is still there leaves it alone.
	if got := runFastForward(t, repo, "maestro/GH-1", "main"); got != newMain {
		t.Fatalf("second run: %q", got)
	}
	// The agent resolves and commits: the merge has both parents.
	repoSh(t, repo, `printf 'one\nTWO by both\nthree\n' > a.txt && git add a.txt && git commit -q --no-edit`)
	if got := repoSh(t, repo, `git merge-base --is-ancestor main HEAD && echo yes`); got != "yes" {
		t.Fatal("the resolved merge does not contain main")
	}
}
