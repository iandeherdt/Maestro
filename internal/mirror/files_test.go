package mirror

import (
	"strings"
	"testing"
)

func TestCommitFilesSetsOnlyTheGivenFiles(t *testing.T) {
	l := newLab(t)
	files := map[string]File{
		"README.md":                {Content: []byte("hello\n")}, // unchanged
		"conventions/unity/new.sh": {Content: []byte("#!/bin/sh\n"), Executable: true},
		"src/app.go":               {Content: []byte("package app\n")},
	}
	head, changed, err := l.mirror.CommitFiles(ctx, "maestro-packs", "main", files, "Update the packs", "Maestro", "bot@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, ",") != "conventions/unity/new.sh,src/app.go" {
		t.Fatalf("changed %q", changed)
	}
	git := func(args ...string) string {
		return l.run(l.root, append([]string{"--git-dir=" + l.mirror.Dir}, args...)...)
	}
	if got := git("rev-parse", IncomingRef("maestro-packs")); got != head {
		t.Fatalf("incoming ref %s, head %s", got, head)
	}
	if got := git("rev-parse", head+"^"); got != l.base {
		t.Fatalf("parent %s", got)
	}
	if got := git("ls-tree", head, "conventions/unity/new.sh"); !strings.HasPrefix(got, "100755 blob") {
		t.Fatalf("mode: %s", got)
	}
	// Everything else is as it was.
	if got := git("show", head+":old.txt"); got != "remove me" {
		t.Fatalf("old.txt: %q", got)
	}
	if got := git("log", "-1", "--format=%an <%ae>|%s", head); got != "Maestro <bot@example.com>|Update the packs" {
		t.Fatalf("commit %q", got)
	}

	// The same files again give the same commit.
	again, _, err := l.mirror.CommitFiles(ctx, "maestro-packs", "main", files, "Update the packs", "Maestro", "bot@example.com")
	if err != nil || again != head {
		t.Fatalf("again %s (%v), first %s", again, err, head)
	}

	raw, ok, err := l.mirror.FileAt(ctx, "main", "notes.txt")
	if err != nil || !ok || string(raw) != "one\ntwo\n" {
		t.Fatalf("FileAt %q %v %v", raw, ok, err)
	}
	if _, ok, err := l.mirror.FileAt(ctx, "main", "missing.txt"); ok || err != nil {
		t.Fatalf("a missing file: %v %v", ok, err)
	}
	if _, ok, err := l.mirror.FileAt(ctx, "main", "src"); ok || err != nil {
		t.Fatalf("a folder: %v %v", ok, err)
	}
}

func TestCommitFilesWithoutAChange(t *testing.T) {
	l := newLab(t)
	head, changed, err := l.mirror.CommitFiles(ctx, "maestro-packs", "main",
		map[string]File{"README.md": {Content: []byte("hello\n")}}, "x", "Maestro", "bot@example.com")
	if err != nil || head != "" || len(changed) != 0 {
		t.Fatalf("%q %q %v", head, changed, err)
	}
}

func TestCommitFilesRefusesOddPaths(t *testing.T) {
	l := newLab(t)
	for _, p := range []string{"../x", "/etc/x", ".git/config", "a//b", "a/./b", "dir/", "a\nb"} {
		if _, _, err := l.mirror.CommitFiles(ctx, "maestro-packs", "main", map[string]File{p: {Content: []byte("x")}}, "x", "M", "m@example.com"); err == nil {
			t.Errorf("%q was accepted", p)
		}
	}
	if _, _, err := l.mirror.CommitFiles(ctx, "maestro-packs", "nope", map[string]File{"a": {}}, "x", "M", "m@example.com"); err == nil {
		t.Error("a missing branch was accepted")
	}
}
