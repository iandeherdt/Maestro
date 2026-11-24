package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService stands in for a running service's local API.
type fakeService struct {
	mu      sync.Mutex
	running int // polls left with an agent at work
	paused  bool
	reason  string
	calls   []string
	dir     string
	srv     *httptest.Server
}

func newFakeService(t *testing.T, running int) *fakeService {
	t.Helper()
	f := &fakeService{running: running, dir: t.TempDir()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/state":
			s := map[string]any{"worker": "mac", "paused": f.paused, "pause_reason": f.reason}
			if f.running > 0 {
				f.running--
				s["running"] = []map[string]any{{"issue_identifier": "GH-44", "phase": "turn", "turn_count": 3}}
			}
			json.NewEncoder(w).Encode(s)
		case "/api/v1/pause":
			var in struct{ Reason string }
			json.NewDecoder(r.Body).Decode(&in)
			f.paused, f.reason = true, in.Reason
			w.WriteHeader(http.StatusAccepted)
		case "/api/v1/resume":
			f.paused, f.reason = false, ""
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.setPID(100)
	return f
}

func (f *fakeService) setPID(pid int) {
	raw, _ := json.Marshal(serviceInfo{Addr: strings.TrimPrefix(f.srv.URL, "http://"), PID: pid})
	os.WriteFile(filepath.Join(f.dir, serviceFile), raw, 0o600)
}

func (f *fakeService) state() (paused bool, reason string, calls string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused, f.reason, strings.Join(f.calls, " ")
}

func fastPoll(t *testing.T) {
	old := drainPoll
	drainPoll = 5 * time.Millisecond
	t.Cleanup(func() { drainPoll = old })
}

func TestDrainWaitsForTheRunningAgents(t *testing.T) {
	fastPoll(t)
	f := newFakeService(t, 3)
	var out bytes.Buffer
	paused, err := drain(bg, []string{f.dir}, time.Minute, &out)
	if err != nil || len(paused) != 1 {
		t.Fatalf("drain: %v %v", paused, err)
	}
	if p, reason, _ := f.state(); !p || reason != pauseForRestart {
		t.Fatalf("not paused for the restart: %v %q", p, reason)
	}
	if f.running != 0 || !strings.Contains(out.String(), "GH-44 (turn, turn 3)") {
		t.Fatalf("returned before the agent finished, or did not say what it waits for:\n%s", out.String())
	}
	// The restarted service comes back paused; the pause is lifted there,
	// not in the old process.
	before := servicePIDs(paused)
	f.setPID(200)
	out.Reset()
	resumeAfterRestart(bg, paused, before, &out)
	if p, _, _ := f.state(); p || !strings.Contains(out.String(), "back") {
		t.Fatalf("still paused after the restart: %s", out.String())
	}
}

func TestDrainGivesUpAndResumes(t *testing.T) {
	fastPoll(t)
	f := newFakeService(t, 1000)
	var out bytes.Buffer
	if _, err := drain(bg, []string{f.dir}, 30*time.Millisecond, &out); err == nil {
		t.Fatal("no error after the wait")
	}
	if p, _, _ := f.state(); p {
		t.Fatal("left paused after giving up")
	}
}

func TestDrainKeepsTheOwnersPause(t *testing.T) {
	fastPoll(t)
	f := newFakeService(t, 0)
	f.paused, f.reason = true, "holiday"
	paused, err := drain(bg, []string{f.dir}, time.Minute, &bytes.Buffer{})
	if err != nil || len(paused) != 0 {
		t.Fatalf("drain: %v %v", paused, err)
	}
	if p, reason, _ := f.state(); !p || reason != "holiday" {
		t.Fatalf("the owner's pause was changed: %v %q", p, reason)
	}
	// A service that does not run is not waited for.
	if paused, err := drain(bg, []string{t.TempDir()}, time.Minute, &bytes.Buffer{}); err != nil || len(paused) != 0 {
		t.Fatalf("no service: %v %v", paused, err)
	}
}

var bg = context.Background()
