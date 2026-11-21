package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckExample(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"check", filepath.Join("..", "..", "examples", "WORKFLOW.example.md")}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "required_labels=[run:linux]") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestCheckFailures(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(bad, []byte("---\ntracker:\n  kind: jira\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"missing file":   {"check", filepath.Join(dir, "nope.md")},
		"invalid config": {"check", bad},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 1 {
			t.Errorf("%s: exit %d, want 1", name, code)
		}
		if errOut.Len() == 0 {
			t.Errorf("%s: nothing on stderr", name)
		}
	}
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("no arguments: exit %d, want 2", code)
	}
}

func TestCheckUsesDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	var out, errOut bytes.Buffer
	if code := run([]string{"check"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "missing_workflow_file") {
		t.Errorf("exit %d, stderr %q; want missing_workflow_file for ./WORKFLOW.md", code, errOut.String())
	}
}

func TestCheckRejectsBadTemplate(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "WORKFLOW.example.md"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"unknown variable": "Fix {{ issue.titel }}",
		"unknown filter":   "Fix {{ issue.title | shell }}",
		"include tag":      "{% include 'other' %}",
	} {
		path := filepath.Join(t.TempDir(), "WORKFLOW.md")
		if err := os.WriteFile(path, append(example, []byte("\n"+body+"\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := run([]string{"check", path}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "template_render_error") {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut.String())
		}
	}
}

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 || out.String() != "symphony dev\n" {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

func TestCheckFindsTheFileSetupWrote(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the configuration directory is redirected through XDG_CONFIG_HOME")
	}
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "WORKFLOW.example.md"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	fromSetup := filepath.Join(dir, "config", "symphony", "WORKFLOW.md")
	if err := os.MkdirAll(filepath.Dir(fromSetup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fromSetup, example, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"check"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "checked "+fromSetup) {
		t.Fatalf("exit %d, output %q, stderr %q", code, out.String(), errOut.String())
	}
	// A file in the current directory comes first.
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte("---\ntracker:\n  kind: jira\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"check"}, &out, &errOut); code != 1 {
		t.Fatalf("the local file was not used: exit %d, output %q", code, out.String())
	}
}
