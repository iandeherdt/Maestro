package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

const review = "---\nname: code-review\ndescription: |\n  How this team\n  reviews code.\n---\n\n# Review\n"

// source is a directory like a repository of skills.
func source(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "README.md"), "# skills\n", 0o644)
	write(t, filepath.Join(root, "skills", "code-review", "SKILL.md"), review, 0o644)
	write(t, filepath.Join(root, "skills", "code-review", "scripts", "lint.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(root, "skills", "code-review", "notes", "style.md"), "be kind\n", 0o644)
	write(t, filepath.Join(root, "skills", "Release", "SKILL.md"), "---\ndescription: How to cut a release.\n---\n", 0o644)
	write(t, filepath.Join(root, "skills", "broken", "SKILL.md"), "no front matter\n", 0o644)
	write(t, filepath.Join(root, "skills", "silent", "SKILL.md"), "---\nname: silent\n---\n", 0o644)
	write(t, filepath.Join(root, "agents", "tester.md"), "---\nname: tester\ndescription: Writes tests.\ntools: Read, Bash\n---\nYou write tests.\n", 0o644)
	write(t, filepath.Join(root, "agents", "README.md"), "# agents\n", 0o644)
	write(t, filepath.Join(root, "agents", "Bad Name.md"), "---\nname: Bad Name\ndescription: x\n---\n", 0o644)
	write(t, filepath.Join(root, "docs", "tester.md"), "---\nname: not-an-agent\ndescription: x\n---\n", 0o644)
	return root
}

func TestDiscover(t *testing.T) {
	root := source(t)
	found, problems := Discover(root)
	var got []string
	for _, f := range found {
		got = append(got, string(f.Kind)+":"+f.Name)
	}
	if strings.Join(got, " ") != "skill:code-review skill:release agent:tester" {
		t.Fatalf("found %v", got)
	}
	cr := found[0]
	if cr.Description != "How this team reviews code." || strings.Join(cr.Files, ",") != "SKILL.md,notes/style.md,scripts/lint.sh" ||
		strings.Join(cr.Scripts, ",") != "scripts/lint.sh" {
		t.Errorf("code-review: %+v", cr)
	}
	all := strings.Join(problems, "\n")
	for _, want := range []string{"broken", "no front matter", "silent", "no description", "Bad Name"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q:\n%s", want, all)
		}
	}
	// A source that is one skill.
	one, problems := Discover(filepath.Join(root, "skills", "code-review"))
	if len(one) != 1 || one[0].Name != "code-review" || len(problems) != 0 {
		t.Errorf("a single skill: %v %v", one, problems)
	}
}

func TestASkillHoldsOnlyOrdinaryFiles(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "s", "SKILL.md"), review, 0o644)
	secret := filepath.Join(t.TempDir(), "key.pem")
	write(t, secret, "secret", 0o600)
	if err := os.Symlink(secret, filepath.Join(root, "s", "key.pem")); err != nil {
		t.Skip(err)
	}
	found, problems := Discover(root)
	if len(found) != 0 || len(problems) != 1 || !strings.Contains(problems[0], "link") {
		t.Fatalf("a link to a file of the machine was accepted: %v %v", found, problems)
	}
	// SKILL.md itself as a link.
	root = t.TempDir()
	os.MkdirAll(filepath.Join(root, "s"), 0o755)
	os.Symlink(secret, filepath.Join(root, "s", "SKILL.md"))
	if found, _ := Discover(root); len(found) != 0 {
		t.Fatalf("a linked SKILL.md was accepted: %v", found)
	}
	// Too large.
	root = t.TempDir()
	write(t, filepath.Join(root, "s", "SKILL.md"), review, 0o644)
	write(t, filepath.Join(root, "s", "big.bin"), strings.Repeat("x", MaxBytes+1), 0o644)
	if found, problems := Discover(root); len(found) != 0 || !strings.Contains(strings.Join(problems, ""), "larger") {
		t.Fatalf("an oversized skill: %v %v", found, problems)
	}
}

func TestStore(t *testing.T) {
	found, _ := Discover(source(t))
	s := Store{Dir: filepath.Join(t.TempDir(), "agent-files")}
	if es, err := s.List(); err != nil || len(es) != 0 {
		t.Fatalf("an empty store: %v %v", es, err)
	}
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for _, f := range found {
		f.Source, f.Commit = "owner/repo", strings.Repeat("a", 40)
		if err := s.Install(f, now); err != nil {
			t.Fatal(err)
		}
	}
	es, err := s.List()
	if err != nil || len(es) != 3 || es[0].Name != "code-review" || es[2].Kind != Agent || es[0].Added != "2026-11-22T09:00:00Z" {
		t.Fatalf("%v %v", es, err)
	}
	script := filepath.Join(s.SkillsDir(), "code-review", "scripts", "lint.sh")
	if st, err := os.Stat(script); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("the script: %v", err)
	}
	if st, err := os.Stat(filepath.Join(s.AgentsDir(), "tester.md")); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("the agent file: %v", err)
	}
	// Adding again replaces: a file that is gone from the source is gone.
	os.Remove(filepath.Join(found[0].Path, "scripts", "lint.sh"))
	again, _ := Discover(filepath.Dir(filepath.Dir(found[0].Path)))
	if err := s.Install(again[0], now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err == nil {
		t.Error("a replaced skill kept a file of the old version")
	}
	if es, _ := s.List(); len(es) != 3 {
		t.Errorf("replacing made a second entry: %v", es)
	}
	if n, err := s.Remove("code-review"); n != 1 || err != nil {
		t.Fatalf("remove: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(s.SkillsDir(), "code-review")); err == nil {
		t.Error("remove left the files")
	}
	if n, _ := s.Remove("code-review"); n != 0 {
		t.Error("removing twice")
	}
	bad := again[0]
	bad.Name = "../escape"
	if err := s.Install(bad, now); err == nil {
		t.Error("a name with a path was installed")
	}
}

func TestParse(t *testing.T) {
	for arg, want := range map[string]Source{
		"anthropics/skills":               {URL: "https://github.com/anthropics/skills.git"},
		"owner/repo.git/skills/pdf":       {URL: "https://github.com/owner/repo.git", Sub: "skills/pdf"},
		"https://git.example.com/a/b.git": {URL: "https://git.example.com/a/b.git"},
		"~/my skills":                     {Local: "/home/ian/my skills"},
		"/srv/skills":                     {Local: "/srv/skills"},
	} {
		got, err := Parse(arg, "", "", "/home/ian")
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", arg, got, err)
		}
	}
	if got, err := Parse("owner/repo", "v1.2", "skills/x/", "/h"); err != nil || got.Ref != "v1.2" || got.Sub != "skills/x" {
		t.Errorf("ref and path: %+v %v", got, err)
	}
	for _, bad := range [][3]string{{"", "", ""}, {"skills", "", ""}, {"git@github.com:a/b.git", "", ""}, {"http://example.com/a.git", "", ""},
		{"--upload-pack=x/y", "", ""}, {"owner/repo", "--upload-pack=x", ""}, {"owner/repo", "a..b", ""}, {"owner/repo", "", "../.."},
		{"owner/repo/x", "", "y"}, {"./dir", "main", ""}, {"https://example.com/a b", "", ""}, {"file:///etc", "", ""}} {
		if got, err := Parse(bad[0], bad[1], bad[2], "/h"); err == nil {
			t.Errorf("%v was accepted: %+v", bad, got)
		}
	}
}

func TestFetch(t *testing.T) {
	ctx := context.Background()
	root := source(t)
	got, commit, cleanup, err := Fetch(ctx, Source{Local: root, Sub: "skills"})
	cleanup()
	if err != nil || commit != "" || got != filepath.Join(root, "skills") {
		t.Fatalf("local: %q %q %v", got, commit, err)
	}
	if _, _, _, err := Fetch(ctx, Source{Local: root, Sub: "nope"}); err == nil {
		t.Error("a missing directory")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.Symlink("/etc", filepath.Join(root, "outside"))
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "skills")
	head := git("rev-parse", "HEAD")
	fileProtocol = "always"
	defer func() { fileProtocol = "never" }()
	url := "file://" + root
	dir, commit, cleanup, err := Fetch(ctx, Source{URL: url, Sub: "skills"})
	if err != nil || commit != head {
		t.Fatalf("fetch: %q %v", commit, err)
	}
	if found, _ := Discover(dir); len(found) != 2 {
		t.Errorf("found %v", found)
	}
	cleanup()
	if _, err := os.Stat(dir); err == nil {
		t.Error("cleanup left the download")
	}
	// A commit as the ref.
	_, commit, cleanup, err = Fetch(ctx, Source{URL: url, Ref: head})
	cleanup()
	if err != nil || commit != head {
		t.Errorf("by commit: %q %v", commit, err)
	}
	if _, _, cleanup, err := Fetch(ctx, Source{URL: url, Ref: "no-such-branch"}); err == nil {
		t.Error("an unknown ref")
	} else {
		cleanup()
	}
	// A directory in the repository that is a link out of it.
	if _, _, cleanup, err := Fetch(ctx, Source{URL: url, Sub: "outside"}); err == nil {
		t.Error("a link out of the repository was followed")
	} else {
		cleanup()
	}
	// The default refuses the file protocol.
	fileProtocol = "never"
	if _, _, cleanup, err := Fetch(ctx, Source{URL: url}); err == nil {
		t.Error("a file address was fetched")
	} else {
		cleanup()
	}
}

func TestSkillFrontMatterLimits(t *testing.T) {
	root := t.TempDir()
	write := func(rel, text string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/long/SKILL.md", "---\nname: long\ndescription: "+strings.Repeat("word ", 300)+"\n---\nbody\n")
	write("skills/tag/SKILL.md", "---\nname: tag\ndescription: Uses <b>tags</b>.\n---\nbody\n")
	write("skills/hooked/SKILL.md", "---\nname: hooked\ndescription: Formats code after each edit.\nallowed-tools: Bash\nhooks:\n  PostToolUse: []\n---\nbody\n")
	write("agents/runner.md", "---\nname: runner\ndescription: Runs things.\nmodel: opus\nmcpServers:\n  x: {command: x}\n---\nbody\n")
	found, problems := Discover(root)
	all := strings.Join(problems, "\n")
	for _, want := range []string{"long: the description is", "tag: the description holds"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q:\n%s", want, all)
		}
	}
	got := map[string]string{}
	for _, f := range found {
		got[f.Name] = strings.Join(f.Settings, ",")
	}
	if got["hooked"] != "allowed-tools,hooks" || got["runner"] != "mcpServers,model" || len(found) != 2 {
		t.Errorf("settings: %v", got)
	}
}
