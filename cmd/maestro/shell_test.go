package main

import (
	"io"
	"strings"
	"testing"
)

func TestFindWorkspace(t *testing.T) {
	keys := []string{"ENG-7", "GH-12", "GH-12-design", "GH-120"}
	for typed, want := range map[string]string{"12": "GH-12", "gh-12": "GH-12", "#12": "GH-12", "GH-12-design": "GH-12-design", "eng-7": "ENG-7", "7": "ENG-7"} {
		if got, err := findWorkspace(keys, typed); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", typed, got, err, want)
		}
	}
	if _, err := findWorkspace(keys, "13"); err == nil || !strings.Contains(err.Error(), "GH-12") {
		t.Errorf("a missing ticket: %v", err)
	}
	if _, err := findWorkspace([]string{"ENG-7", "GH-7"}, "7"); err == nil || !strings.Contains(err.Error(), "ENG-7 or GH-7") {
		t.Errorf("an ambiguous number: %v", err)
	}
}

func TestParseSession(t *testing.T) {
	f, ok := parseSession("shell", []string{"12", "--agent"}, io.Discard)
	if !ok || f.ticket != "12" || !f.agent {
		t.Errorf("%+v %v", f, ok)
	}
	f, ok = parseSession("shell", []string{"--dir", "/x"}, io.Discard)
	if !ok || f.ticket != "" || f.dir != "/x" {
		t.Errorf("%+v %v", f, ok)
	}
	f, ok = parseSession("talk", []string{"--new", "ENG-7"}, io.Discard)
	if !ok || f.ticket != "ENG-7" || !f.new {
		t.Errorf("%+v %v", f, ok)
	}
	for _, bad := range [][]string{{}, {"1", "2"}, {"--agent", "1"}} {
		if _, ok := parseSession("talk", bad, io.Discard); ok {
			t.Errorf("talk %q was accepted", bad)
		}
	}
}
