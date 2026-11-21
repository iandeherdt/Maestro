package mirror

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/policy"
)

var ctx = context.Background()

// lab is a bare mirror plus a workspace clone, as the sandbox would have.
type lab struct {
	t      *testing.T
	root   string
	work   string
	base   string
	mirror *Mirror
}

func (l *lab) run(dir string, args ...string) string {
	l.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+l.root,
		"GIT_AUTHOR_NAME=agent", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=agent", "GIT_COMMITTER_EMAIL=agent@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		l.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (l *lab) write(rel, content string) {
	l.t.Helper()
	p := filepath.Join(l.work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func (l *lab) commit(msg string) string {
	l.t.Helper()
	l.run(l.work, "add", "-A")
	l.run(l.work, "commit", "-q", "--allow-empty", "-m", msg)
	return l.run(l.work, "rev-parse", "HEAD")
}

// bundle writes what the export step writes.
func (l *lab) bundle(spec ...string) string {
	l.t.Helper()
	path := filepath.Join(l.root, "handoff.bundle")
	os.Remove(path)
	if len(spec) == 0 {
		spec = []string{l.base + "..HEAD"}
	}
	l.run(l.work, append([]string{"bundle", "create", "-q", path}, spec...)...)
	return path
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	l := &lab{t: t, root: root, work: filepath.Join(root, "work")}
	seed := filepath.Join(root, "seed")
	os.MkdirAll(seed, 0o755)
	l.work = seed
	l.run(seed, "init", "-q", "-b", "main")
	l.write("README.md", "hello\n")
	l.write("src/app.go", "package app\n\nfunc A() int { return 1 }\n")
	l.write("old.txt", "remove me\n")
	l.write("notes.txt", "one\ntwo\n")
	l.write(".gitattributes", "*.txt -diff\n*.dat binary\n")
	l.base = l.commit("base")
	bare := filepath.Join(root, "mirror.git")
	l.run(root, "clone", "-q", "--bare", seed, bare)
	l.work = filepath.Join(root, "work")
	l.run(root, "clone", "-q", seed, l.work)
	l.mirror = &Mirror{Dir: bare, OwnerUID: -1, MaxBundleBytes: 1 << 20}
	return l
}

func wantRejected(t *testing.T, err error, part string) {
	t.Helper()
	var r *Rejected
	if !errors.As(err, &r) {
		t.Fatalf("got %v, want a rejection containing %q", err, part)
	}
	if !strings.Contains(r.Reason, part) {
		t.Fatalf("reason %q does not contain %q", r.Reason, part)
	}
}

var lim = Limits{MaxChangedFiles: 100, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20}

func byPath(d Diff) map[string]policy.Change {
	m := map[string]policy.Change{}
	for _, c := range d.Changes {
		m[c.Path] = c
	}
	return m
}

func TestImportAndDiff(t *testing.T) {
	l := newLab(t)
	l.write("src/app.go", "package app\n\nfunc A() int { return 2 }\n")
	l.write("src/new.go", "package app\n")
	l.write("notes.txt", "one\ntwo\nthree\n")
	os.Remove(filepath.Join(l.work, "old.txt"))
	first := l.commit("first change")
	l.write("run.sh", "#!/bin/sh\n")
	os.Chmod(filepath.Join(l.work, "run.sh"), 0o755)
	os.Symlink("/etc/passwd", filepath.Join(l.work, "link"))
	l.write("blob.bin", "\x00\x01\x02binary")
	l.write("odd\nname.txt", "x\n")
	second := l.commit("second change\n\nbody with \x1f a separator and a\ttab")

	imp, err := l.mirror.Import(ctx, l.bundle(), "GH-1-abc", l.base)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Head != second || len(imp.Commits) != 2 || imp.Commits[0].SHA != first || imp.Commits[1].SHA != second {
		t.Fatalf("imported %+v", imp)
	}
	c := imp.Commits[1]
	if c.AuthorName != "agent" || c.AuthorEmail != "agent@example.com" || c.CommitterEmail != "agent@example.com" ||
		!strings.Contains(c.Message, "\x1f a separator") || !strings.HasPrefix(c.Message, "second change\n") {
		t.Errorf("commit record %+v", c)
	}
	if got := l.run(l.mirror.Dir, "rev-parse", IncomingRef("GH-1-abc")); got != second {
		t.Errorf("incoming ref = %s", got)
	}

	d, err := l.mirror.Diff(ctx, l.base, imp.Head, lim)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(d)
	if len(got) != 8 {
		t.Fatalf("%d changes: %v", len(got), got)
	}
	app := got["src/app.go"]
	if app.Status != 'M' || len(app.Added) != 1 || app.Added[0] != "func A() int { return 2 }" ||
		len(app.Removed) != 1 || app.Removed[0] != "func A() int { return 1 }" || app.NewSize != 39 || app.DiffBytes == 0 {
		t.Errorf("modified file: %+v", app)
	}
	if n := got["src/new.go"]; n.Status != 'A' || n.OldMode != 0 || n.NewMode != policy.ModeFile || len(n.Added) != 1 {
		t.Errorf("added file: %+v", n)
	}
	if o := got["old.txt"]; o.Status != 'D' || o.NewMode != 0 || len(o.Removed) != 1 || len(o.Added) != 0 {
		t.Errorf("deleted file: %+v", o)
	}
	// The repository marks *.txt as -diff. The lines must be read anyway.
	if n := got["notes.txt"]; len(n.Added) != 1 || n.Added[0] != "three" || n.Binary {
		t.Errorf("file hidden by an attribute: %+v", n)
	}
	if s := got["run.sh"]; s.NewMode != policy.ModeExec {
		t.Errorf("executable: %+v", s)
	}
	if s := got["link"]; s.NewMode != policy.ModeSymlink {
		t.Errorf("symlink: %+v", s)
	}
	if b := got["blob.bin"]; !b.Binary || len(b.Added) != 0 {
		t.Errorf("binary: %+v", b)
	}
	if _, ok := got["odd\nname.txt"]; !ok {
		t.Errorf("a path with a newline was not reported verbatim")
	}
	if len(d.BasePaths) != 5 {
		t.Errorf("base paths: %q", d.BasePaths)
	}

	// The policy sees what it should.
	res := policy.Evaluate(config.Policy{MaxChangedFiles: 100, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20, SecretScan: true},
		policy.Input{Changes: d.Changes, Commits: imp.Commits, BasePaths: d.BasePaths})
	var rules []string
	for _, f := range res.Denied {
		rules = append(rules, f.Rule)
	}
	if strings.Join(rules, ",") != "D3,D6,D8" {
		t.Errorf("policy denied %v", res.Denied)
	}
}

func TestSecretBehindAnAttributeIsFound(t *testing.T) {
	l := newLab(t)
	l.write("notes.txt", "one\ntwo\ntoken: "+"ghp_"+strings.Repeat("a1B2", 9)+"\n")
	l.write("payload.dat", "key "+"ghp_"+strings.Repeat("c3D4", 9)+"\n")
	l.commit("leak")
	imp, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
	if err != nil {
		t.Fatal(err)
	}
	d, err := l.mirror.Diff(ctx, l.base, imp.Head, lim)
	if err != nil {
		t.Fatal(err)
	}
	res := policy.Evaluate(config.Policy{MaxChangedFiles: 100, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20, SecretScan: true},
		policy.Input{Changes: d.Changes})
	if len(res.Denied) != 2 || res.Denied[0].Rule != "D7" || res.Denied[1].Rule != "D7" {
		t.Fatalf("denied %v", res.Denied)
	}
}

func TestImportRejections(t *testing.T) {
	t.Run("no bundle", func(t *testing.T) {
		l := newLab(t)
		_, err := l.mirror.Import(ctx, filepath.Join(l.root, "missing"), "k", l.base)
		wantRejected(t, err, "no bundle")
	})
	t.Run("symlink", func(t *testing.T) {
		l := newLab(t)
		l.write("a", "1")
		l.commit("c")
		real := l.bundle()
		link := filepath.Join(l.root, "link.bundle")
		os.Symlink(real, link)
		_, err := l.mirror.Import(ctx, link, "k", l.base)
		wantRejected(t, err, "regular file")
	})
	t.Run("fifo", func(t *testing.T) {
		l := newLab(t)
		fifo := filepath.Join(l.root, "fifo")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skip(err)
		}
		_, err := l.mirror.Import(ctx, fifo, "k", l.base)
		wantRejected(t, err, "regular file")
	})
	t.Run("directory", func(t *testing.T) {
		l := newLab(t)
		_, err := l.mirror.Import(ctx, l.root, "k", l.base)
		wantRejected(t, err, "regular file")
	})
	t.Run("too large", func(t *testing.T) {
		l := newLab(t)
		l.write("a", strings.Repeat("x", 100))
		l.commit("c")
		l.mirror.MaxBundleBytes = 50
		_, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
		wantRejected(t, err, "larger than")
	})
	t.Run("wrong owner", func(t *testing.T) {
		l := newLab(t)
		l.write("a", "1")
		l.commit("c")
		l.mirror.OwnerUID = os.Getuid() + 1
		_, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
		wantRejected(t, err, "not owned")
	})
	t.Run("right owner", func(t *testing.T) {
		l := newLab(t)
		l.write("a", "1")
		l.commit("c")
		l.mirror.OwnerUID = os.Getuid()
		if _, err := l.mirror.Import(ctx, l.bundle(), "k", l.base); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		l := newLab(t)
		p := filepath.Join(l.root, "junk")
		os.WriteFile(p, []byte("# v2 git bundle\nnot really\n"), 0o644)
		_, err := l.mirror.Import(ctx, p, "k", l.base)
		wantRejected(t, err, "does not verify")
	})
	t.Run("no new commits", func(t *testing.T) {
		l := newLab(t)
		_, err := l.mirror.Import(ctx, l.bundle("HEAD"), "k", l.base)
		wantRejected(t, err, "no new commits")
	})
	t.Run("unrelated history", func(t *testing.T) {
		l := newLab(t)
		l.run(l.work, "checkout", "-q", "--orphan", "evil")
		l.write("a", "1")
		l.commit("root")
		_, err := l.mirror.Import(ctx, l.bundle("HEAD"), "k", l.base)
		wantRejected(t, err, "do not build on the base")
		// The rejected head is not left in the mirror's refs.
		if out, _ := exec.Command("git", "--git-dir="+l.mirror.Dir, "for-each-ref", "refs/symphony").Output(); len(out) != 0 {
			t.Errorf("refs left behind: %s", out)
		}
	})
	t.Run("merge of foreign history", func(t *testing.T) {
		l := newLab(t)
		l.run(l.work, "checkout", "-q", "--orphan", "evil")
		l.write("foreign", "1")
		l.commit("foreign root")
		l.run(l.work, "checkout", "-q", "main")
		l.run(l.work, "-c", "user.name=agent", "-c", "user.email=agent@example.com", "merge", "-q",
			"--allow-unrelated-histories", "-m", "merge", "evil")
		_, err := l.mirror.Import(ctx, l.bundle("HEAD"), "k", l.base)
		wantRejected(t, err, "single line of history")
	})
	t.Run("merge of two local branches", func(t *testing.T) {
		l := newLab(t)
		l.run(l.work, "checkout", "-q", "-b", "side")
		l.write("side", "1")
		l.commit("side")
		l.run(l.work, "checkout", "-q", "main")
		l.write("main", "1")
		l.commit("main")
		l.run(l.work, "-c", "user.name=agent", "-c", "user.email=agent@example.com", "merge", "-q", "-m", "merge", "side")
		_, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
		wantRejected(t, err, "single line of history")
	})
	t.Run("history rewritten below the base", func(t *testing.T) {
		l := newLab(t)
		l.run(l.work, "commit", "-q", "--amend", "-m", "rewritten base")
		l.write("a", "1")
		l.commit("on top")
		_, err := l.mirror.Import(ctx, l.bundle("HEAD"), "k", l.base)
		wantRejected(t, err, "do not build on the base")
	})
	t.Run("base missing from the mirror", func(t *testing.T) {
		l := newLab(t)
		l.write("a", "1")
		hidden := l.commit("not in the mirror")
		l.write("b", "1")
		l.commit("on top")
		_, err := l.mirror.Import(ctx, l.bundle(hidden+"..HEAD"), "k", l.base)
		wantRejected(t, err, "does not verify")
	})
	t.Run("too many commits", func(t *testing.T) {
		l := newLab(t)
		for _, m := range []string{"1", "2", "3"} {
			l.commit(m)
		}
		l.mirror.MaxCommits = 2
		_, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
		wantRejected(t, err, "more than 2")
		l.mirror.MaxCommits = 3
		if _, err := l.mirror.Import(ctx, l.bundle(), "k", l.base); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bad arguments", func(t *testing.T) {
		l := newLab(t)
		for _, key := range []string{"", "../x", "a/b", "-x", "a..b", "x.lock", "a b"} {
			if _, err := l.mirror.Import(ctx, "/dev/null", key, l.base); err == nil {
				t.Errorf("key %q accepted", key)
			}
		}
		if _, err := l.mirror.Import(ctx, "/dev/null", "k", "main"); err == nil {
			t.Error("a ref name was accepted as the base")
		}
		if _, err := l.mirror.Diff(ctx, "--output=/tmp/x", l.base, lim); err == nil {
			t.Error("an option was accepted as a commit")
		}
	})
}

func TestALaterAttemptReplacesTheHead(t *testing.T) {
	l := newLab(t)
	l.write("a", "1")
	l.commit("first try")
	if _, err := l.mirror.Import(ctx, l.bundle(), "k", l.base); err != nil {
		t.Fatal(err)
	}
	l.run(l.work, "reset", "-q", "--hard", l.base)
	l.write("b", "2")
	second := l.commit("second try")
	imp, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
	if err != nil || imp.Head != second {
		t.Fatalf("%+v %v", imp, err)
	}
}

func TestNothingInTheMirrorIsExecuted(t *testing.T) {
	l := newLab(t)
	marker := filepath.Join(l.root, "ran")
	hooks := filepath.Join(l.mirror.Dir, "hooks")
	os.MkdirAll(hooks, 0o755)
	script := "#!/bin/sh\ntouch " + marker + "\n"
	for _, name := range []string{"reference-transaction", "post-update", "pre-receive", "update"} {
		os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755)
	}
	// A diff driver and a text conversion configured in the mirror itself.
	l.run(l.mirror.Dir, "config", "diff.external", "touch "+marker)
	l.run(l.mirror.Dir, "config", "diff.x.textconv", "touch "+marker+";cat")
	l.write(".gitattributes", "*.go diff=x\n")
	l.write("src/app.go", "package app\n")
	l.commit("c")
	imp, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.mirror.Diff(ctx, l.base, imp.Head, lim); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook, diff driver or text conversion ran")
	}
}

func TestDiffStopsReadingAtTheLimits(t *testing.T) {
	l := newLab(t)
	l.write("big.txt", strings.Repeat("line\n", 400))
	l.write("small.txt", "s\n")
	l.write("zz.txt", "z\n")
	l.commit("c")
	imp, err := l.mirror.Import(ctx, l.bundle(), "k", l.base)
	if err != nil {
		t.Fatal(err)
	}

	d, _ := l.mirror.Diff(ctx, l.base, imp.Head, Limits{MaxChangedFiles: 2, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20})
	if len(d.Changes) != 3 || d.Changes[0].DiffBytes != 0 || len(d.Changes[1].Added) != 0 {
		t.Errorf("over the file count, content was still read: %+v", d.Changes)
	}

	d, _ = l.mirror.Diff(ctx, l.base, imp.Head, Limits{MaxChangedFiles: 10, MaxDiffBytes: 1 << 20, MaxFileBytes: 1000})
	got := byPath(d)
	if b := got["big.txt"]; b.NewSize != 2000 || len(b.Added) != 0 {
		t.Errorf("oversized file was loaded: size %d, %d lines", b.NewSize, len(b.Added))
	}
	if s := got["small.txt"]; len(s.Added) != 1 {
		t.Errorf("small file: %+v", s)
	}

	d, _ = l.mirror.Diff(ctx, l.base, imp.Head, Limits{MaxChangedFiles: 10, MaxDiffBytes: 500, MaxFileBytes: 1 << 20})
	var total int64
	for _, c := range d.Changes {
		total += c.DiffBytes
	}
	if total <= 500 || total > 600 {
		t.Errorf("total diff bytes = %d, want just over the limit", total)
	}
	if z := byPath(d)["zz.txt"]; len(z.Added) != 0 {
		t.Errorf("content was read after the budget was spent: %+v", z)
	}
}

func TestEmptyDiff(t *testing.T) {
	l := newLab(t)
	d, err := l.mirror.Diff(ctx, l.base, l.base, lim)
	if err != nil || len(d.Changes) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
}
