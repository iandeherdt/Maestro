package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
)

func lastBody(e *env) string {
	ws := e.tr.Writes()
	return ws[len(ws)-1].Value
}

func TestAttemptOverItsTokenLimitIsStoppedAndBlocked(t *testing.T) {
	e := newEnv(t, todo(1))
	limit := int64(1000)
	e.cfg.Budget.MaxTokensPerAttempt = &limit
	e.tick()
	run := e.w.last()
	run.sink.Event(Event{Kind: "assistant", Usage: &Usage{Input: 600, Output: 100}})
	e.o.Drain(e.ctx)
	if run.terminated {
		t.Fatal("stopped under the limit")
	}
	run.sink.Event(Event{Kind: "assistant", Usage: &Usage{Input: 900, CacheRead: 300}})
	e.o.Drain(e.ctx)
	if !run.terminated {
		t.Fatal("attempt over the limit not stopped")
	}
	if got := e.tr.Get("id-1").State; got != "Blocked" {
		t.Fatalf("state = %q, want Blocked", got)
	}
	if body := lastBody(e); !strings.Contains(body, "token_limit") || !strings.Contains(body, "1200 tokens") {
		t.Fatalf("blocked comment = %q", body)
	}
	if e.b.Today(e.clock.Now()).TotalTokens != 1200 {
		t.Fatal("the stopped attempt's tokens were not counted")
	}
	// The worker's own exit afterwards changes nothing.
	run.sink.Exit(Outcome{Kind: Failed, Category: "terminated"})
	e.o.Drain(e.ctx)
	e.tick()
	e.wantStarts(1)
	if e.b.Failures("id-1") != 0 {
		t.Fatal("late exit counted as a failure")
	}
}

func TestTicketTokensAddUpOverAttempts(t *testing.T) {
	e := newEnv(t, todo(1))
	limit := int64(1000)
	e.cfg.Budget.MaxTokensPerTicket = &limit
	e.tick()
	e.w.last().sink.Event(Event{Kind: "result", Usage: &Usage{Input: 700}})
	e.exit(Outcome{Kind: Failed, Category: "turn_failed"})
	if e.b.TicketTokens("id-1") != 700 {
		t.Fatalf("ticket tokens = %d", e.b.TicketTokens("id-1"))
	}

	// The retry is stopped as soon as the two together pass the limit.
	e.after(10 * time.Second)
	e.wantStarts(2)
	run := e.w.last()
	run.sink.Event(Event{Kind: "assistant", Usage: &Usage{Input: 200}})
	e.o.Drain(e.ctx)
	if run.terminated {
		t.Fatal("stopped under the limit")
	}
	run.sink.Event(Event{Kind: "assistant", Usage: &Usage{Input: 400}})
	e.o.Drain(e.ctx)
	if !run.terminated || e.tr.Get("id-1").State != "Blocked" {
		t.Fatal("ticket over its limit not stopped and blocked")
	}

	// Moved back by hand: a fresh budget, tokens included.
	e.tr.Move("id-1", "Todo")
	e.tick()
	e.wantStarts(3)
	if e.b.TicketTokens("id-1") != 0 {
		t.Fatal("ticket tokens not reset")
	}
}

func TestFailedAttemptOverTicketLimitIsNotRetried(t *testing.T) {
	e := newEnv(t, todo(1))
	limit := int64(1000)
	e.cfg.Budget.MaxTokensPerTicket = &limit
	e.tick()
	// The usage arrives with the exit, so nothing stopped the attempt.
	e.w.last().sink.Event(Event{Kind: "result", Usage: &Usage{Input: 100}})
	e.o.Drain(e.ctx)
	e.o.running["id-1"].usage = Usage{Input: 1500}
	e.exit(Outcome{Kind: Failed, Category: "turn_failed", Message: "tests fail"})
	if got := e.tr.Get("id-1").State; got != "Blocked" {
		t.Fatalf("state = %q, want Blocked", got)
	}
	if body := lastBody(e); !strings.Contains(body, "token_limit") || !strings.Contains(body, "tests fail") {
		t.Fatalf("blocked comment = %q", body)
	}
}

func TestSecondFailureTheSameWayBlocks(t *testing.T) {
	e := newEnv(t, todo(1))
	e.cfg.Budget.BlockRepeatedFailure = true
	e.cfg.Budget.MaxFailureAttempts = 5
	fail := func(out string) {
		e.exit(Outcome{Kind: Rejected, Category: "verify_failed",
			Feedback: []issue.Feedback{{Source: "verify", Body: out}}})
	}
	e.tick()
	fail("Assets/Combat.cs(12,5): error CS0103 after 3.2s")
	// A different error is progress: retried.
	e.after(time.Minute)
	e.wantStarts(2)
	fail("Assets/Combat.cs(40,1): error CS1002: ; expected after 2.9s")
	if e.tr.Get("id-1").State == "Blocked" {
		t.Fatal("blocked on a different error")
	}
	// The same error again, with other numbers in it: blocked.
	e.after(time.Minute)
	e.wantStarts(3)
	fail("Assets/Combat.cs(41,7): error CS1002: ; expected after 4.0s")
	if got := e.tr.Get("id-1").State; got != "Blocked" {
		t.Fatalf("state = %q, want Blocked", got)
	}
	if body := lastBody(e); !strings.Contains(body, "failed the same way") || !strings.Contains(body, "verify_failed") {
		t.Fatalf("blocked comment = %q", body)
	}
}

func TestRepeatedFailureRuleCanBeOff(t *testing.T) {
	e := newEnv(t, todo(1))
	e.cfg.Budget.MaxFailureAttempts = 5
	e.tick()
	e.exit(Outcome{Kind: Failed, Category: "turn_failed", Message: "same"})
	e.after(time.Minute)
	e.exit(Outcome{Kind: Failed, Category: "turn_failed", Message: "same"})
	if e.tr.Get("id-1").State == "Blocked" {
		t.Fatal("blocked although block_repeated_failure is off")
	}
}

func TestFailureWithoutTextIsNeverTheSame(t *testing.T) {
	if failureSignature("turn_failed", "  ") != "" {
		t.Fatal("empty text has a signature")
	}
	if failureSignature("a", "x 1") != failureSignature("a", "x  22") {
		t.Fatal("numbers or spacing make a difference")
	}
	if failureSignature("a", "x") == failureSignature("b", "x") {
		t.Fatal("category ignored")
	}
}
