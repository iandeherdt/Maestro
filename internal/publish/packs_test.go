package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/mirror"
)

// packLab is a lab whose repository had the unity pack applied by an
// older version of Maestro.
func packLab(t *testing.T) (*lab, map[string][]byte) {
	t.Helper()
	l := newLab(t)
	l.cfg.Conventions = []string{"unity"}
	pk, err := conventions.Get("unity")
	if err != nil {
		t.Fatal(err)
	}
	own, err := pk.Files()
	if err != nil {
		t.Fatal(err)
	}
	for n, raw := range own {
		switch {
		case n == "conventions/unity/previews.sh" || n == "conventions/unity/MaestroShots.cs":
			continue // new in this version
		case n == "conventions/unity/env.sh":
			l.write(n, "# an older env.sh\n")
		case strings.HasPrefix(n, ".github/workflows/"):
			l.write(n, "name: an older workflow\n")
		default:
			l.write(n, string(raw))
		}
	}
	l.write(".gitignore", "Library/\n")
	l.git(l.work, "add", "-A")
	l.git(l.work, "commit", "-q", "-m", "unity pack, older")
	l.git(l.work, "push", "-q", l.remote, "HEAD:main")
	mir := l.pub.Mirror.(*mirror.Mirror)
	l.pub.Fetch = func(ctx context.Context) error { return mir.Sync(ctx, l.remote, token) }
	l.hub.head = "maestro/packs"
	return l, own
}

func (l *lab) write(path, content string) {
	l.t.Helper()
	full := filepath.Join(l.work, filepath.FromSlash(path))
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func TestUpdatePacksOpensAPullRequestWithTheNewerFiles(t *testing.T) {
	l, own := packLab(t)
	u, err := l.pub.UpdatePacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.PRNumber != 31 || strings.Join(u.Packs, ",") != "unity" {
		t.Fatalf("%+v", u)
	}
	want := map[string]bool{"conventions/unity/env.sh": true, "conventions/unity/previews.sh": true,
		"conventions/unity/MaestroShots.cs": true, ".gitignore": true}
	if len(u.Files) != len(want) {
		t.Fatalf("files %q", u.Files)
	}
	for _, f := range u.Files {
		if !want[f] {
			t.Fatalf("files %q", u.Files)
		}
	}
	if len(u.ByHand) != 1 || !strings.HasPrefix(u.ByHand[0], ".github/workflows/") {
		t.Fatalf("by hand %q", u.ByHand)
	}
	// The branch on the remote has the new files, with the scripts executable.
	show := func(spec string) string {
		out, err := exec.Command("git", "--git-dir="+l.remote, "show", spec).Output()
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		return string(out)
	}
	if show("maestro/packs:conventions/unity/env.sh") != string(own["conventions/unity/env.sh"]) {
		t.Fatal("env.sh was not updated")
	}
	if !strings.Contains(show("maestro/packs:.gitignore"), "Temp/") || !strings.HasPrefix(show("maestro/packs:.gitignore"), "Library/\n") {
		t.Fatalf(".gitignore: %q", show("maestro/packs:.gitignore"))
	}
	if out, _ := exec.Command("git", "--git-dir="+l.remote, "ls-tree", "maestro/packs", "conventions/unity/previews.sh").Output(); !strings.HasPrefix(string(out), "100755") {
		t.Fatalf("previews.sh mode: %s", out)
	}
	if strings.Contains(show("maestro/packs:"+u.ByHand[0]), "conventions") {
		t.Fatal("the workflow was changed")
	}
	created := l.hub.sent("POST /repos/ian/app/pulls")
	if len(created) != 1 || created[0]["head"] != "maestro/packs" || created[0]["base"] != "main" {
		t.Fatalf("created %v", created)
	}
	body := created[0]["body"].(string)
	for _, s := range []string{"`conventions/unity/previews.sh`", "## To copy by hand", "maestro conventions apply unity --replace", footer} {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q:\n%s", s, body)
		}
	}

	// A second run with nothing new pushes nothing new.
	before, _ := exec.Command("git", "--git-dir="+l.remote, "rev-parse", "maestro/packs").Output()
	if _, err := l.pub.UpdatePacks(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := exec.Command("git", "--git-dir="+l.remote, "rev-parse", "maestro/packs").Output()
	if string(before) != string(after) {
		t.Fatal("the branch moved without a change")
	}
}

func TestUpdatePacksLeavesARepositoryThatIsCurrent(t *testing.T) {
	l, own := packLab(t)
	for n, raw := range own {
		if !strings.HasPrefix(n, ".github/workflows/") {
			l.write(n, string(raw))
		}
	}
	l.write(".gitignore", strings.Join(mustPack(t).Gitignore, "\n")+"\n")
	l.git(l.work, "add", "-A")
	l.git(l.work, "commit", "-q", "-m", "current")
	l.git(l.work, "push", "-q", l.remote, "HEAD:main")
	u, err := l.pub.UpdatePacks(ctx)
	if err != nil || u.PRNumber != 0 || len(u.Files) != 0 {
		t.Fatalf("%+v %v", u, err)
	}
	if len(l.hub.sent("POST /repos/ian/app/pulls")) != 0 {
		t.Fatal("a pull request was opened")
	}
}

func TestUpdatePacksSkipsAPackThatWasNeverApplied(t *testing.T) {
	l := newLab(t)
	l.cfg.Conventions = []string{"unity"}
	mir := l.pub.Mirror.(*mirror.Mirror)
	l.pub.Fetch = func(ctx context.Context) error { return mir.Sync(ctx, l.remote, token) }
	u, err := l.pub.UpdatePacks(ctx)
	if err != nil || u.PRNumber != 0 || len(u.Files) != 0 {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestUpdatePacksIsNotReopenedAfterTheOwnerClosedIt(t *testing.T) {
	l, _ := packLab(t)
	if _, err := l.pub.UpdatePacks(ctx); err != nil {
		t.Fatal(err)
	}
	head, _ := exec.Command("git", "--git-dir="+l.remote, "rev-parse", "maestro/packs").Output()
	l.hub.pulls = []map[string]any{{"number": 31, "state": "closed",
		"head": map[string]any{"ref": "maestro/packs", "sha": strings.TrimSpace(string(head)), "repo": map[string]any{"full_name": "ian/app"}},
		"base": map[string]any{"ref": "main"}, "user": map[string]any{"login": "bot"}}}
	u, err := l.pub.UpdatePacks(ctx)
	if err != nil || !u.Declined || u.PRNumber != 0 {
		t.Fatalf("%+v %v", u, err)
	}
	if len(l.hub.sent("POST /repos/ian/app/pulls")) != 1 {
		t.Fatal("the pull request was opened again")
	}
}

func mustPack(t *testing.T) *conventions.Pack {
	p, err := conventions.Get("unity")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
