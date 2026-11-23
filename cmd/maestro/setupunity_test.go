package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/setup"
)

func unitySettings(dir string) string {
	return "---\ntracker:\n  kind: github\n  allowed_authors: [ian]\nworkspace:\n  root: " + dir + "/ws\n  repo: o/game\n" +
		"sandbox:\n  backend: docker\n  network:\n    proxy: 127.0.0.1:3128\nverify:\n  command: 'true'\nbudget:\n  state_dir: " + dir + "/state\nworker:\n  name: mac\n---\nWork on the ticket.\n"
}

// A repository with a Unity project on a Mac with Apple silicon: the step
// switches the pack on, finds the Editor Hub installed in the home folder,
// writes it into the settings and waits for Hub's licence.
func TestSetupUnityOnMac(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	file := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(file, []byte(unitySettings(dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	want := unityVersion()
	st := &setup.State{Repo: "o/game", WorkflowFile: file}
	s := &setupUnity{home: home, goos: "darwin", arch: "arm64",
		version: func(context.Context, *setup.State) (string, error) { return want, nil }}

	status, err := s.Check(t.Context(), st)
	if err != nil || status.Done || !strings.Contains(status.Detail, "pack is off") {
		t.Fatalf("before: %+v %v", status, err)
	}

	editor := filepath.Join(home, "Applications", "Unity", "Hub", "Editor", want, "Unity.app", "Contents", "MacOS", "Unity")
	os.MkdirAll(filepath.Dir(editor), 0o755)
	os.WriteFile(editor, []byte("#!/bin/sh\n"), 0o755)
	lic := filepath.Join(home, "Library", "Unity", "licenses")
	os.MkdirAll(lic, 0o755)

	// Yes to the pack, yes to the Editor; then no licence: stop.
	var out strings.Builder
	err = s.Run(t.Context(), setup.NewUI(strings.NewReader("y\ny\nn\n"), &out), st)
	if err != setup.ErrAborted {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "unity") || !strings.Contains(string(raw), editor) {
		t.Fatalf("settings:\n%s\n%s", raw, out.String())
	}
	if status, _ := s.Check(t.Context(), st); status.Done || !strings.Contains(status.Detail, "licence") {
		t.Fatalf("without licence: %+v", status)
	}
	os.WriteFile(filepath.Join(lic, "UnityEntitlementLicense.xml"), []byte("<x/>"), 0o600)
	if status, _ := s.Check(t.Context(), st); !status.Done {
		t.Fatalf("after: %+v", status)
	}
}

// A repository without a Unity project needs nothing.
func TestSetupUnityNotUnity(t *testing.T) {
	st := &setup.State{Repo: "o/web", WorkflowFile: "/nowhere"}
	s := &setupUnity{goos: "darwin", arch: "arm64",
		version: func(context.Context, *setup.State) (string, error) { return "", nil }}
	if status, err := s.Check(t.Context(), st); err != nil || !status.Done {
		t.Fatalf("%+v %v", status, err)
	}
}

func TestFetchUnityVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/repos/o/game/contents/ProjectSettings/ProjectVersion.txt":
			w.Write([]byte("m_EditorVersion: 6000.5.8f1\nm_EditorVersionWithRevision: 6000.5.8f1 (abc)\n"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	if v, err := fetchUnityVersion(t.Context(), srv.URL, "o/game", "tok"); err != nil || v != "6000.5.8f1" {
		t.Errorf("game: %q %v", v, err)
	}
	if v, err := fetchUnityVersion(t.Context(), srv.URL, "o/web", "tok"); err != nil || v != "" {
		t.Errorf("web: %q %v", v, err)
	}
	if _, err := fetchUnityVersion(t.Context(), srv.URL, "o/game", "bad"); err == nil {
		t.Error("401 accepted")
	}
}
