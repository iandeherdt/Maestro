package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWithSetting(t *testing.T) {
	cases := []struct {
		name, in, section, key, literal, want string
	}{
		{"replaces the line and keeps its comment",
			"---\nbudget:\n  state_dir: /s\n  max_runs_per_day: 20 # was low\nworker:\n  name: mac\n---\nprompt\n",
			"budget", "max_runs_per_day", "40",
			"---\nbudget:\n  state_dir: /s\n  max_runs_per_day: 40 # was low\nworker:\n  name: mac\n---\nprompt\n"},
		{"adds the key under its section",
			"---\nbudget:\n    state_dir: /s\nworker:\n  name: mac\n---\nprompt\n",
			"budget", "max_runs_per_day", "40",
			"---\nbudget:\n    max_runs_per_day: 40\n    state_dir: /s\nworker:\n  name: mac\n---\nprompt\n"},
		{"adds the section",
			"---\nworker:\n  name: mac\n---\nprompt: budget:\n",
			"claude", "model", `"sonnet"`,
			"---\nworker:\n  name: mac\nclaude:\n  model: \"sonnet\"\n---\nprompt: budget:\n"},
		{"leaves a deeper key of the same name alone",
			"---\nagent:\n  nested:\n    max_turns: 3\n  max_turns: 8\n---\n",
			"agent", "max_turns", "12",
			"---\nagent:\n  nested:\n    max_turns: 3\n  max_turns: 12\n---\n"},
	}
	for _, c := range cases {
		got, err := withSetting(c.in, c.section, c.key, c.literal)
		if err != nil || got != c.want {
			t.Errorf("%s: err %v\n%s", c.name, err, got)
		}
	}
	if _, err := withSetting("---\nbudget: {state_dir: /s}\n---\n", "budget", "max_runs_per_day", "40"); err == nil {
		t.Error("a section on one line was edited")
	}
}

func TestSettingsShowAndSet(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "WORKFLOW.example.md"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, example, 0o600)
	var out, errOut bytes.Buffer
	if code := run([]string{"settings", "--file", path}, &out, &errOut); code != 0 {
		t.Fatalf("show: %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), path) || !strings.Contains(out.String(), "budget.max_runs_per_day") {
		t.Errorf("show:\n%s", out.String())
	}

	out.Reset()
	if code := run([]string{"settings", "set", "budget.max_runs_per_day", "40", "--file", path}, &out, &errOut); code != 0 {
		t.Fatalf("set: %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "now 40 (it was 20)") {
		t.Errorf("set output:\n%s", out.String())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	for _, args := range [][]string{
		{"budget.max_tokens_per_day", "5000000"},
		{"budget.max_tokens_per_day", "none"},
		{"claude.model", "sonnet"},
	} {
		if code := run(append([]string{"settings", "set"}, append(args, "--file", path)...), &out, &errOut); code != 0 {
			t.Fatalf("set %v: %d %s", args, code, errOut.String())
		}
	}
	if code := run([]string{"check", path}, &out, &errOut); code != 0 {
		t.Fatalf("the changed file is not valid: %s", errOut.String())
	}
	out.Reset()
	run([]string{"settings", "--file", path}, &out, &errOut)
	for _, want := range []string{"budget.max_runs_per_day            40 ", "budget.max_tokens_per_day          none ", "claude.model                       sonnet "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, out.String())
		}
	}

	// Refusals leave the file as it was.
	before, _ := os.ReadFile(path)
	for _, args := range [][]string{
		{"budget.max_runs_per_day", "0"},
		{"budget.max_runs_per_day", "none"},
		{"budget.state_dir", "/tmp"},
	} {
		errOut.Reset()
		if code := run(append([]string{"settings", "set"}, append(args, "--file", path)...), &out, &errOut); code != 1 {
			t.Errorf("set %v: %d %s", args, code, errOut.String())
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("a refused change altered the file")
	}
}

func TestStatusNamesTheCommandThatRaisesALimit(t *testing.T) {
	var out bytes.Buffer
	s := statusJSON{Worker: "mac"}
	s.Budget.Limit = "daily run limit reached (20 of 20 runs); raise budget.max_runs_per_day"
	printStatus(&out, s, time.Now())
	if !strings.Contains(out.String(), "maestro settings set budget.max_runs_per_day NUMBER") {
		t.Errorf("status:\n%s", out.String())
	}
}
