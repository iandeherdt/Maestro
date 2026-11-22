package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsAddListRemove(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	skill := filepath.Join(src, "skills", "house-style")
	os.MkdirAll(filepath.Join(skill, "bin"), 0o755)
	os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: house-style\ndescription: \"How we write \\e[31mcode here.\"\n---\nUse tabs.\n"), 0o644)
	os.WriteFile(filepath.Join(skill, "bin", "check.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.MkdirAll(filepath.Join(src, "agents"), 0o755)
	os.WriteFile(filepath.Join(src, "agents", "reviewer.md"), []byte("---\nname: reviewer\ndescription: Reviews a diff.\n---\nReview.\n"), 0o644)

	do := func(stdin string, args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		code := runSkills(append([]string{"--dir", dir}, args...), strings.NewReader(stdin), &out, &errs)
		return code, out.String(), errs.String()
	}
	if code, out, _ := do("", "list"); code != 0 || !strings.Contains(out, "no extra skills") || !strings.Contains(out, "maestro skills add") {
		t.Fatalf("empty list: %d %s", code, out)
	}
	// Saying no adds nothing.
	code, out, _ := do("n\n", "add", src)
	if code != 3 || !strings.Contains(out, "Nothing was added") {
		t.Fatalf("declined: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-files", "skills", "house-style")); err == nil {
		t.Fatal("a declined skill was installed")
	}
	// The flags may stand after the source.
	code, out, errs := do("y\n", "add", src, "--path", "skills")
	if code != 0 {
		t.Fatalf("add: %d %s %s", code, out, errs)
	}
	for _, want := range []string{"skill  house-style", "How we write", "2 file(s)", "programs the agent may run: bin/check.sh",
		"like it follows your own instructions", "inside the agent's container", "Added this one"} {
		if !strings.Contains(out, want) {
			t.Errorf("add lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Error("a control character from a description reached the terminal")
	}
	if strings.Contains(out, "reviewer") {
		t.Error("--path did not narrow the source")
	}
	code, out, _ = do("", "add", "--yes", src)
	if code != 0 || !strings.Contains(out, "replaces the one from") || !strings.Contains(out, "agent  reviewer") || !strings.Contains(out, "Added these 2") {
		t.Fatalf("add all: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-files", "agents", "reviewer.md")); err != nil {
		t.Error(err)
	}
	code, out, _ = do("")
	if code != 0 || !strings.Contains(out, "skill  house-style") || !strings.Contains(out, "agent  reviewer") || !strings.Contains(out, "from "+src) {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, _, errs := do("", "remove", "nope"); code != 1 || !strings.Contains(errs, "Nothing called") {
		t.Errorf("removing what is not there: %d %s", code, errs)
	}
	if code, out, _ := do("", "remove", "house-style"); code != 0 || !strings.Contains(out, "Removed house-style") {
		t.Errorf("remove: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-files", "skills", "house-style")); err == nil {
		t.Error("remove left the skill")
	}
	for _, bad := range [][]string{{"add"}, {"add", "a", "b"}, {"frobnicate"}, {"add", "not a source"}, {"add", src, "--nope"}} {
		if code, _, _ := do("", bad...); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
	if code, _, errs := do("", "add", "--yes", t.TempDir()); code != 1 || !strings.Contains(errs, "No skill") {
		t.Errorf("an empty source: %d %s", code, errs)
	}
}
