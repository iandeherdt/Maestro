package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithPackTouchesOneLine(t *testing.T) {
	const file = "---\ntracker:\n  kind: github\n---\nprompt\n"
	on, err := withPack(file, "dotnet", true)
	if err != nil || on != "---\nconventions: [dotnet]\ntracker:\n  kind: github\n---\nprompt\n" {
		t.Fatalf("%q %v", on, err)
	}
	both, err := withPack(on, "typescript", true)
	if err != nil || !strings.Contains(both, "conventions: [dotnet, typescript]\n") {
		t.Fatalf("%q %v", both, err)
	}
	off, err := withPack(both, "dotnet", false)
	if err != nil || !strings.Contains(off, "conventions: [typescript]\n") || strings.Count(off, "\n") != strings.Count(both, "\n") {
		t.Fatalf("%q %v", off, err)
	}
	if same, err := withPack(file, "dotnet", false); err != nil || same != file {
		t.Errorf("switching off what is not on changed the file: %q %v", same, err)
	}
	// A comment after the list survives, and quotes are understood.
	got, err := withPack("---\nconventions: [\"dotnet\"]  # ours\n---\n", "typescript", true)
	if err != nil || got != "---\nconventions: [dotnet, typescript]  # ours\n---\n" {
		t.Errorf("%q %v", got, err)
	}
	if _, err := withPack("---\nconventions:\n  - dotnet\n---\n", "typescript", true); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Errorf("a list in another form: %v", err)
	}
	if _, err := withPack("no settings block", "dotnet", true); err == nil {
		t.Error("a file without a settings block was edited")
	}
}

func TestConventionsCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"conventions"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "dotnet") || !strings.Contains(out.String(), "typescript") {
		t.Fatalf("list: %d %s %s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"conventions", "show", "typescript"}, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "enforced by: prettier --check") || !strings.Contains(out.String(), "The agent may not change") {
		t.Fatalf("show: %d %s", code, out.String())
	}
	if code := run([]string{"conventions", "show", "cobol"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "The packs are") {
		t.Fatalf("an unknown pack: %d %s", code, errOut.String())
	}

	// apply wants a checkout, writes the files, and says what is left.
	dir := t.TempDir()
	errOut.Reset()
	if code := run([]string{"conventions", "apply", "dotnet", "--to", dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "not a checkout") {
		t.Fatalf("outside a checkout: %d %s", code, errOut.String())
	}
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	out.Reset()
	if code := run([]string{"conventions", "apply", "dotnet", "--to", dir}, &out, &errOut); code != 0 {
		t.Fatalf("apply: %d %s", code, errOut.String())
	}
	for _, want := range []string{"wrote  Directory.Build.props", "git push", "conventions-dotnet", "maestro conventions enable dotnet", "1. Check that the project", "4. Switch the pack on"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("apply output lacks %q:\n%s", want, out.String())
		}
	}
	os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte("mine"), 0o644)
	errOut.Reset()
	if code := run([]string{"conventions", "apply", "dotnet", "--to", dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "--replace") {
		t.Fatalf("a conflict: %d %s", code, errOut.String())
	}
}

func TestConventionsEnableChangesTheSettingsFileSafely(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "WORKFLOW.example.md"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, example, 0o600)
	var out, errOut bytes.Buffer
	if code := run([]string{"conventions", "enable", "dotnet", "--file", path}, &out, &errOut); code != 0 {
		t.Fatalf("enable: %d %s", code, errOut.String())
	}
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), "---\nconventions: [dotnet]\n") || !strings.HasSuffix(string(got), strings.TrimPrefix(string(example), "---\n")) {
		t.Errorf("the file:\n%s", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	if !strings.Contains(out.String(), "maestro image") || !strings.Contains(out.String(), ".NET SDK") {
		t.Errorf("output:\n%s", out.String())
	}
	out.Reset()
	if code := run([]string{"conventions", "enable", "dotnet", "--file", path}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "already on") {
		t.Fatalf("a second time: %d %s", code, out.String())
	}
	if code := run([]string{"check", path}, &out, &errOut); code != 0 {
		t.Fatalf("the changed file is not valid: %s", errOut.String())
	}
	if code := run([]string{"conventions", "disable", "dotnet", "--file", path}, &out, &errOut); code != 0 {
		t.Fatalf("disable: %d %s", code, errOut.String())
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "conventions: []\n") {
		t.Errorf("after disable:\n%s", got)
	}

	// A file that is not valid is not touched.
	bad := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(bad, []byte("---\ntracker:\n  kind: jira\n---\n"), 0o600)
	errOut.Reset()
	if code := run([]string{"conventions", "enable", "dotnet", "--file", bad}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "Fix that first") {
		t.Fatalf("an invalid file: %d %s", code, errOut.String())
	}
	if raw, _ := os.ReadFile(bad); strings.Contains(string(raw), "conventions") {
		t.Error("an invalid file was changed")
	}
}
