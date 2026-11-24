package main

import (
	"bytes"
	"strings"
	"testing"
)

// Every command a person types has a help page.
var typedCommands = []string{"setup", "new", "check", "image", "login", "run", "status", "dashboard",
	"pause", "resume", "unblock", "doctor", "service", "conventions", "settings", "restart", "skills", "unity", "linear",
	"assistant", "projects", "gitlab", "gitea", "update", "version", "help"}

func TestEveryCommandHasAPage(t *testing.T) {
	for _, name := range typedCommands {
		if findHelp(name) == nil {
			t.Errorf("no help page for %s", name)
		}
	}
	for _, p := range helpPages {
		found := false
		for _, g := range helpGroups {
			found = found || p.group == g
		}
		if !found {
			t.Errorf("%s is in group %q, which the overview does not show", p.name, p.group)
		}
		if p.usage == "" || p.summary == "" || p.body == "" {
			t.Errorf("%s has an empty part", p.name)
		}
		for _, s := range p.see {
			if findHelp(s) == nil {
				t.Errorf("%s refers to %s, which has no page", p.name, s)
			}
		}
	}
}

func TestHelpOverview(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("%v: status %d", args, code)
		}
		for _, name := range typedCommands {
			if !strings.Contains(out.String(), "\n  "+name+" ") {
				t.Errorf("%v: the overview does not list %s", args, name)
			}
		}
		if errOut.Len() != 0 {
			t.Errorf("%v: wrote to stderr: %s", args, errOut.String())
		}
	}
}

func TestHelpPage(t *testing.T) {
	for _, args := range [][]string{{"help", "setup"}, {"setup", "--help"}, {"setup", "-h"}, {"setup", "--board", "linear", "-help"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("%v: status %d: %s", args, code, errOut.String())
		}
		for _, want := range []string{"maestro setup:", "--host", "--board", "--sandbox", "--port", "--project NAME", "See also:"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: the page lacks %q", args, want)
			}
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"skills", "--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "SKILL.md") {
		t.Errorf("skills --help: status %d\n%s", code, out.String())
	}
}

func TestHelpUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{"help", "nope"}, {"nope"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%v: status %d", args, code)
		}
		if !strings.Contains(errOut.String(), `no command "nope"`) || !strings.Contains(errOut.String(), "maestro help COMMAND") {
			t.Errorf("%v: %s", args, errOut.String())
		}
	}
}

// A help flag after "--" is an argument, not a request for help.
func TestHelpFlagAfterDashes(t *testing.T) {
	if _, done := runHelp([]string{"pause", "--", "-h"}, &bytes.Buffer{}, &bytes.Buffer{}); done {
		t.Error("-h after -- was taken as help")
	}
}
