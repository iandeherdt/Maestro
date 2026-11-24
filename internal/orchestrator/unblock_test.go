package orchestrator

import (
	"strings"
	"testing"
	"time"
)

func unblockNow(e *env, id string) UnblockResult {
	ch := e.o.Unblock(id)
	e.o.Drain(e.ctx)
	return <-ch
}

func TestUnblockMovesABlockedIssueBackWithAFreshBudget(t *testing.T) {
	e := newEnv(t, todo(1))
	e.tick()
	for i := 0; i < 3; i++ {
		e.exit(Outcome{Kind: Failed, Category: "turn_failed"})
		if i < 2 {
			e.after(time.Hour)
		}
	}
	if got := e.tr.Get("id-1").State; got != "Blocked" {
		t.Fatalf("state = %q, want Blocked", got)
	}
	e.wantStarts(3)

	res := unblockNow(e, "#1")
	if !res.Found || res.Identifier != "GH-1" || res.Failures != 3 || res.MovedTo != "Rework" || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	if e.b.Failures("id-1") != 0 || e.b.BlockMarked("id-1") {
		t.Fatal("budget not cleared")
	}
	if got := e.tr.Get("id-1").State; got != "Rework" {
		t.Fatalf("state = %q, want Rework", got)
	}
	ws := e.tr.Writes()
	if last := ws[len(ws)-1]; last.Op != "comment" || !strings.Contains(last.Value, "maestro unblock") {
		t.Errorf("last write = %+v", last)
	}
	// It is picked up again, and the cleared count survives a restart.
	e.open()
	e.tick()
	e.wantStarts(4)
}

func TestUnblockClearsTheCountOfAnIssueItHolds(t *testing.T) {
	e := newEnv(t, todo(1))
	e.tick()
	e.exit(Outcome{Kind: Failed, Category: "turn_failed"})
	if e.b.Failures("id-1") != 1 {
		t.Fatalf("failures = %d", e.b.Failures("id-1"))
	}
	res := unblockNow(e, "gh-1")
	if !res.Found || res.Failures != 1 || res.MovedTo != "" {
		t.Fatalf("result = %+v", res)
	}
	if e.b.Failures("id-1") != 0 {
		t.Fatal("count not cleared")
	}
	if e.retry("id-1") == nil {
		t.Fatal("the retry is gone")
	}
}

func TestUnblockLeavesOtherWorkersAndUnknownIssues(t *testing.T) {
	other := todo(2)
	other.State, other.Labels = "Blocked", []string{"maestro", "run:mac"}
	e := newEnv(t, todo(1), other)
	if res := unblockNow(e, "2"); !res.Found || !res.OtherWorker || res.MovedTo != "" {
		t.Fatalf("other worker: %+v", res)
	}
	if got := e.tr.Get("id-2").State; got != "Blocked" {
		t.Fatalf("another worker's issue moved to %q", got)
	}
	// GH-1 is in Todo, not blocked and not held.
	if res := unblockNow(e, "1"); res.Found {
		t.Fatalf("not blocked: %+v", res)
	}
	e.tr.FailReads = 1
	if res := unblockNow(e, "9"); res.Found || res.Err == nil {
		t.Fatalf("read failure: %+v", res)
	}
}

func TestSameIdentifier(t *testing.T) {
	for _, c := range []struct {
		typed, id string
		want      bool
	}{
		{"GH-12", "GH-12", true}, {"gh-12", "GH-12", true}, {"12", "GH-12", true}, {"#12", "GH-12", true},
		{"012", "GH-12", true}, {"1", "GH-12", false}, {"12", "GH-120", false}, {"", "GH-12", false},
		{"#", "GH-12", false}, {"GH-1", "GH-12", false}, {"ENG-12", "GH-12", false}, {"12", "ENG-12", true},
	} {
		if got := SameIdentifier(c.typed, c.id); got != c.want {
			t.Errorf("SameIdentifier(%q, %q) = %v", c.typed, c.id, got)
		}
	}
}

// Without Rework among the active states there is nowhere the service may
// move the card: the count is cleared and the owner is told.
func TestUnblockWithoutReworkOnlyClearsTheCount(t *testing.T) {
	e := newEnv(t, todo(1))
	e.cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	e.o.SetConfig(e.cfg)
	e.tr.Move("id-1", "Blocked")
	res := unblockNow(e, "1")
	if !res.Found || res.Err == nil || res.MovedTo != "" {
		t.Fatalf("result = %+v", res)
	}
	if got := e.tr.Get("id-1").State; got != "Blocked" {
		t.Fatalf("state = %q", got)
	}
}
