package dockerbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// The export script, run on this machine with its two fixed paths replaced,
// so that it is tested where there is no container engine too.
func runExport(t *testing.T, repo, handoff string) string {
	t.Helper()
	script := strings.ReplaceAll(strings.ReplaceAll(sandbox.ExportScript, "/workspace/repo", repo), "/handoff", handoff)
	cmd := exec.Command("sh", "-c", script, "export", "sym[bot]", "sym@example.com")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func repoSh(t *testing.T, dir, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

func exportRepo(t *testing.T) (repo, handoff, base string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repo, handoff = filepath.Join(root, "repo"), filepath.Join(root, "handoff")
	if err := os.MkdirAll(handoff, 0o755); err != nil {
		t.Fatal(err)
	}
	repoSh(t, root, `git init -q -b main repo && cd repo && mkdir conventions &&
		echo 'run the tests' > conventions/verify.sh && echo 'node_modules/' > .gitignore && echo hi > README &&
		git add -A && git commit -qm base && git rev-parse HEAD > .git/maestro-base &&
		git config user.name agent && git config user.email agent@example.com`)
	return repo, handoff, repoSh(t, repo, `git rev-parse HEAD`)
}

func TestExportLeavesAnHonestWorkspaceAlone(t *testing.T) {
	repo, handoff, base := exportRepo(t)
	if got := runExport(t, repo, handoff); got != base+" 0" {
		t.Fatalf("clean export: %q", got)
	}
	// Ignored by a .gitignore that is in the commit: not part of the work.
	repoSh(t, repo, `mkdir node_modules && echo x > node_modules/x && echo a > a.txt && git add a.txt && git commit -qm a && echo b > b.txt`)
	fields := strings.Fields(runExport(t, repo, handoff))
	if len(fields) != 2 || fields[1] != "2" {
		t.Fatalf("export: %v", fields)
	}
	if got := repoSh(t, repo, `git log -1 --format=%s; git status --porcelain`); got != "maestro: uncommitted changes at end of turn" {
		t.Fatalf("after export: %q", got)
	}
	if got := runExport(t, repo, handoff); got != fields[0]+" 2" {
		t.Fatalf("second export: %q", got)
	}
}

func TestExportCommitsWhatGitWasToldNotToSee(t *testing.T) {
	for name, hide := range map[string]string{
		"skip-worktree":    `echo 'exit 0' > conventions/verify.sh && git update-index --skip-worktree conventions/verify.sh`,
		"assume-unchanged": `echo 'exit 0' > conventions/verify.sh && git update-index --assume-unchanged conventions/verify.sh`,
		"exclude file":     `echo helper > cheat.txt && echo cheat.txt >> .git/info/exclude`,
		"clean filter": `echo 'exit 0' > conventions/verify.sh && echo 'conventions/verify.sh filter=hide' > .git/info/attributes &&
			git config filter.hide.clean "sh -c 'cat >/dev/null; echo run the tests'" && git add conventions/verify.sh`,
		"another work tree": `echo 'exit 0' > conventions/verify.sh && mkdir ../elsewhere && git --work-tree=../elsewhere checkout -q -f HEAD &&
			git config core.worktree "$PWD/../elsewhere"`,
	} {
		t.Run(name, func(t *testing.T) {
			repo, handoff, base := exportRepo(t)
			repoSh(t, repo, hide)
			if got := repoSh(t, repo, `git status --porcelain`); got != "" {
				t.Fatalf("the change is not hidden from the workspace's git: %q", got)
			}
			fields := strings.Fields(runExport(t, repo, handoff))
			if len(fields) != 2 || fields[1] != "1" || fields[0] == base {
				t.Fatalf("export: %v", fields)
			}
			// The bundle holds the hidden change, under the service's identity.
			inspect := filepath.Join(t.TempDir(), "inspect")
			repoSh(t, repo, `git clone -q --bare . "`+inspect+`" 2>/dev/null; cd "`+inspect+`" && git bundle verify -q "`+handoff+`/out.bundle"`)
			show := repoSh(t, repo, `git show --stat --format='%an <%ae> %s' `+fields[0])
			if !strings.Contains(show, "sym[bot] <sym@example.com> maestro: changes that git was told not to see") ||
				!(strings.Contains(show, "conventions/verify.sh") || strings.Contains(show, "cheat.txt")) {
				t.Fatalf("the commit: %s", show)
			}
		})
	}
}
