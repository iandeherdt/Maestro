package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/budget"
	"github.com/iandeherdt/Maestro/internal/httpapi"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

type fakeOrch struct {
	snap    orchestrator.Snapshot
	calls   []string
	unblock map[string]orchestrator.UnblockResult
}

func (f *fakeOrch) Latest() orchestrator.Snapshot { return f.snap }
func (f *fakeOrch) RequestTick()                  {}
func (f *fakeOrch) Pause(reason string)           { f.calls = append(f.calls, "pause:"+reason) }
func (f *fakeOrch) Resume()                       { f.calls = append(f.calls, "resume") }
func (f *fakeOrch) Unblock(id string) <-chan orchestrator.UnblockResult {
	f.calls = append(f.calls, "unblock:"+id)
	ch := make(chan orchestrator.UnblockResult, 1)
	ch <- f.unblock[id]
	return ch
}

func TestStatusPauseResumeReachTheRunningService(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var s statusJSON
	err := reach(ctx, dir, http.MethodGet, "/api/v1/state", nil, &s)
	if err == nil || !strings.Contains(err.Error(), "maestro run") {
		t.Fatalf("nothing running: %v", err)
	}

	now := time.Date(2026, 11, 21, 19, 0, 0, 0, time.UTC)
	orch := &fakeOrch{snap: orchestrator.Snapshot{Worker: "mac", Today: budget.Day{Runs: 2, TotalTokens: 1500, AgentSeconds: 95},
		Running: []orchestrator.RunningView{{Identifier: "GH-11", Phase: "agent", TurnCount: 2, StartedAt: now.Add(-90 * time.Second),
			LastMessage: "editing \x1b[31mhello.txt\nnext"}},
		Retrying:       []orchestrator.RetryEntry{{Identifier: "GH-12", Kind: "failure", DueAt: now.Add(20 * time.Second), Error: "verify_failed"}},
		AwaitingChecks: []orchestrator.PublishRecord{{Identifier: "GH-13", PRNumber: 14}},
		Paused:         true, PauseReason: "on holiday",
		DailyLimit: "daily run limit reached (20 of 20 runs); raise budget.max_runs_per_day"}}
	srv, err := httpapi.Listen(0, orch)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Shutdown(ctx)
	if err := writeServiceInfo(dir, srv.Addr()); err != nil {
		t.Fatal(err)
	}
	if err := reach(ctx, dir, http.MethodGet, "/api/v1/state", nil, &s); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printStatus(&out, s, now)
	for _, want := range []string{`worker "mac"`, "PAUSED: on holiday", "maestro resume",
		"until tomorrow: daily run limit reached (20 of 20 runs)", "GH-11    working (agent, turn 2, started 1m30s ago)", "last: editing  [31mhello.txt next",
		"GH-13    pull request #14 is open", "GH-12    will be tried again in 20s (verify_failed)",
		"Today: 2 agent run(s), 1500 tokens, 1m35s of agent time."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Error("a control character reached the terminal")
	}
	if err := reach(ctx, dir, http.MethodPost, "/api/v1/pause", strings.NewReader(`{"reason":"lunch"}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := reach(ctx, dir, http.MethodPost, "/api/v1/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(orch.calls, ",") != "pause:lunch,resume" {
		t.Errorf("calls = %v", orch.calls)
	}

	// The file is never a way to make this command call another machine.
	os.WriteFile(filepath.Join(dir, serviceFile), []byte(`{"addr":"93.184.216.34:80","pid":1}`), 0o600)
	if err := reach(ctx, dir, http.MethodGet, "/api/v1/state", nil, &s); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("a non-loopback address: %v", err)
	}
	// A file left by a service that was killed.
	os.WriteFile(filepath.Join(dir, serviceFile), []byte(`{"addr":"127.0.0.1:1","pid":1}`), 0o600)
	if err := reach(ctx, dir, http.MethodGet, "/api/v1/state", nil, &s); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("a dead service: %v", err)
	}

	out.Reset()
	printStatus(&out, statusJSON{Worker: "mac"}, now)
	if !strings.Contains(out.String(), "Nothing is being worked on.") || strings.Contains(out.String(), "PAUSED") || strings.Contains(out.String(), "tomorrow") {
		t.Errorf("idle status:\n%s", out.String())
	}
}

func TestUnblockAsksTheRunningService(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	orch := &fakeOrch{unblock: map[string]orchestrator.UnblockResult{
		"12":    {Identifier: "GH-12", Found: true, Failures: 3, MovedTo: "Todo"},
		"GH-13": {Identifier: "GH-13", Found: true, Failures: 1},
		"14":    {Identifier: "GH-14", Found: true, OtherWorker: true},
		"15":    {Identifier: "GH-15", Found: true, Failures: 2, Err: errors.New("GitHub answered 502")},
	}}
	srv, err := httpapi.Listen(0, orch)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Shutdown(ctx)
	if err := writeServiceInfo(dir, srv.Addr()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   string
		code int
		want string
	}{
		{"12", 0, "GH-12 is unblocked: 3 failed attempt(s) are forgotten and it is back in Todo."},
		{"GH-13", 0, "GH-13 starts again with a fresh budget: 1 failed attempt(s)"},
		{"14", 1, "blocked, but for another worker"},
		{"15", 1, "could not be moved: GitHub answered 502"},
		{"99", 1, "No blocked ticket 99 was found"},
	} {
		var out, errb bytes.Buffer
		if code := unblock(ctx, dir, c.id, &out, &errb); code != c.code {
			t.Errorf("%s: exit %d, want %d (%s%s)", c.id, code, c.code, out.String(), errb.String())
		}
		if !strings.Contains(out.String()+errb.String(), c.want) {
			t.Errorf("%s: %q lacks %q", c.id, out.String()+errb.String(), c.want)
		}
	}
}
