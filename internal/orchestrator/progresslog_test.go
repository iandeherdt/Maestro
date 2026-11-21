package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker/trackertest"
)

func TestStatusCommentsAreAlsoLogged(t *testing.T) {
	var buf bytes.Buffer
	o := &Orchestrator{d: Deps{Tracker: trackertest.New(), Log: slog.New(slog.NewTextHandler(&buf, nil))}}
	o.comment(context.Background(), issue.Issue{ID: "1", Identifier: "GH-1"}, "Pull request #3 opened.", "agent text")
	if !strings.Contains(buf.String(), `issue=GH-1 text="Pull request #3 opened."`) || strings.Contains(buf.String(), "agent text") {
		t.Errorf("log:\n%s", buf.String())
	}
}

func TestPauseAndResumeFromAnotherGoroutine(t *testing.T) {
	e := newEnv(t, todo(1))
	e.o.Pause("")
	e.o.Drain(e.ctx)
	if s := e.o.Latest(); !s.Paused || s.PauseReason != "paused by the operator" {
		t.Fatalf("%+v", s)
	}
	e.o.Pause("on holiday")
	e.tick()
	if s := e.o.Latest(); s.PauseReason != "on holiday" || len(s.Running) != 0 {
		t.Fatalf("paused, yet: %+v", s)
	}
	// The pause survives a restart.
	e.open()
	e.tick()
	if s := e.o.Latest(); !s.Paused || len(s.Running) != 0 {
		t.Fatalf("after a restart: %+v", s)
	}
	e.o.Resume()
	e.o.Drain(e.ctx)
	if s := e.o.Latest(); s.Paused || !e.o.forceTick {
		t.Fatalf("%+v", s)
	}
	e.tick()
	if s := e.o.Latest(); len(s.Running) != 1 {
		t.Fatalf("resumed, nothing dispatched: %+v", s)
	}
}
