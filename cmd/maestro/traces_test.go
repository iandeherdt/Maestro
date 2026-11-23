package main

import (
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/trace"
	"github.com/iandeherdt/Maestro/internal/worker"
)

func TestTurnTracesFollowTheSettings(t *testing.T) {
	cfg := &config.Config{}
	f := turnTraces(func() *config.Config { return cfg }, trace.New("", nil))
	if tr := f(worker.Turn{WorkspaceKey: "GH-1"}); tr != nil {
		t.Fatalf("traces are sent while off: %#v", tr)
	}
	cfg = &config.Config{Traces: config.Traces{URL: "http://127.0.0.1:1/v1/traces", Project: "p"}}
	if f(worker.Turn{WorkspaceKey: "GH-1"}) == nil {
		t.Fatal("no trace with a url set")
	}
}
