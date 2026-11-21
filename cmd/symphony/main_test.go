package main

import (
	"bytes"
	"os"
	"path/filepath"
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
