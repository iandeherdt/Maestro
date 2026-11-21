package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/issue"
)

type fakeChecks struct {
	status   CheckStatus
	err      error
	readyErr error
	calls    int
	ready    int
}

func (c *fakeChecks) Check(context.Context, PublishRecord) (CheckStatus, error) {
	c.calls++
	return c.status, c.err
}

func (c *fakeChecks) MarkReady(context.Context, PublishRecord) error {
	if c.readyErr != nil {
		return c.readyErr
	}
	c.ready++
	return nil
}

// published brings one ticket to the point where its pull request is open.
func published(t *testing.T) (*env, *fakeChecks) {
	t.Helper()
	e := newEnv(t, todo(1))
	e.cfg.Publish.ChecksPollIntervalMS = 60000
	c := &fakeChecks{}
	e.o.d.Checks = c
	e.tick()
	e.exit(Outcome{Kind: Published, Publish: &PublishRecord{IssueID: "id-1", Identifier: "GH-1", PRNumber: 7,
		HeadSHA: "abc", ChecksDeadline: e.clock.Now().Add(30 * time.Minute)}})
	return e, c
}

func lastState(e *env) string {
	w := e.tr.StateWrites("id-1")
	return w[len(w)-1]
}

func TestChecksPassHandsOff(t *testing.T) {
	e, c := published(t)
	e.tick()
	if c.calls != 1 || lastState(e) != "In Progress" || !e.o.claimed["id-1"] {
		t.Fatalf("pending: calls %d, state %s", c.calls, lastState(e))
	}
	// Not looked at again before the poll interval has passed.
	e.clock.advance(30 * time.Second)
	e.tick()
	if c.calls != 1 {
		t.Fatalf("polled after 30s: %d calls", c.calls)
	}
	e.b.IncFailure("id-1")
	c.status = CheckStatus{State: ChecksPassed}
	e.clock.advance(31 * time.Second)
	e.tick()
	if c.ready != 1 || lastState(e) != "Human Review" || e.o.claimed["id-1"] || len(e.o.awaiting) != 0 {
		t.Fatalf("ready %d, state %s, claimed %v", c.ready, lastState(e), e.o.claimed["id-1"])
	}
	if e.b.Failures("id-1") != 0 {
		t.Error("the failure count was kept after a successful hand-off")
	}
	e.wantStarts(1)
}

func TestHandOffIsRetriedWhenAWriteFails(t *testing.T) {
	e, c := published(t)
	c.status = CheckStatus{State: ChecksPassed}
	c.readyErr = errors.New("api down")
	e.tick()
	if lastState(e) != "In Progress" || len(e.o.awaiting) != 1 {
		t.Fatal("handed off although the pull request is still a draft")
	}
	c.readyErr = nil
	e.tr.FailWrites = 1
	e.clock.advance(61 * time.Second)
	e.tick()
	if len(e.o.awaiting) != 1 || !e.o.claimed["id-1"] {
		t.Fatal("the record was dropped although the state was not written")
	}
	e.clock.advance(61 * time.Second)
	e.tick()
	if lastState(e) != "Human Review" || len(e.o.awaiting) != 0 {
		t.Fatalf("state %s", lastState(e))
	}
}

func TestFailedChecksGoBackToTheAgentWithFeedback(t *testing.T) {
	e, c := published(t)
	c.status = CheckStatus{State: ChecksFailed, Feedback: []issue.Feedback{{Source: "ci", Body: "test: 2 failed"}}}
	e.tick()
	r := e.retry("id-1")
	if r.Kind != KindGate || r.Error != "checks_failed" || r.Attempt != 1 || len(e.o.awaiting) != 0 || e.b.Failures("id-1") != 1 {
		t.Fatalf("retry %+v, failures %d", r, e.b.Failures("id-1"))
	}
	e.after(time.Second)
	e.wantStarts(2)
	if a := e.w.last().attempt; len(a.Feedback) != 1 || a.Feedback[0].Body != "test: 2 failed" || a.RetryKind != KindGate {
		t.Fatalf("attempt %+v", a)
	}
}

func TestFailedChecksCountAgainstTheBudget(t *testing.T) {
	e, c := published(t)
	e.b.IncFailure("id-1")
	e.b.IncFailure("id-1")
	c.status = CheckStatus{State: ChecksFailed}
	e.tick()
	if lastState(e) != "Blocked" || e.o.claimed["id-1"] {
		t.Fatalf("state %s", lastState(e))
	}
}

func TestForeignPushAndClosedPullRequestBlock(t *testing.T) {
	for state, reason := range map[CheckState]string{ChecksForeignPush: "foreign_push", ChecksClosed: "pull_request_closed"} {
		e, c := published(t)
		c.status = CheckStatus{State: state}
		e.tick()
		if lastState(e) != "Blocked" || e.o.claimed["id-1"] || len(e.o.awaiting) != 0 || c.ready != 0 {
			t.Fatalf("%s: state %s", reason, lastState(e))
		}
		found := false
		for _, w := range e.tr.Writes() {
			found = found || strings.Contains(w.Value, reason)
		}
		if !found {
			t.Errorf("%s is not named in the status comment: %+v", reason, e.tr.Writes())
		}
	}
}

func TestChecksDeadline(t *testing.T) {
	e, c := published(t)
	e.clock.advance(29 * time.Minute)
	e.tick()
	if lastState(e) != "In Progress" {
		t.Fatal("blocked before the deadline")
	}
	e.clock.advance(2 * time.Minute)
	e.tick()
	if lastState(e) != "Blocked" || len(e.o.awaiting) != 0 {
		t.Fatalf("state %s", lastState(e))
	}
	// An API that keeps failing must not hold the ticket forever either.
	e, c = published(t)
	c.err = errors.New("api down")
	e.tick()
	if lastState(e) != "In Progress" || len(e.o.awaiting) != 1 {
		t.Fatal("a read error before the deadline changed something")
	}
	e.clock.advance(31 * time.Minute)
	e.tick()
	if lastState(e) != "Blocked" {
		t.Fatalf("state %s", lastState(e))
	}
}

func TestTicketMovedByAPersonWhileWaiting(t *testing.T) {
	// Moved to another state: the service lets go and does not overwrite it.
	e, c := published(t)
	c.status = CheckStatus{State: ChecksPassed}
	e.tr.Move("id-1", "Backlog")
	e.tick()
	if c.calls != 0 || c.ready != 0 || e.o.claimed["id-1"] || len(e.o.awaiting) != 0 {
		t.Fatalf("calls %d ready %d", c.calls, c.ready)
	}
	if e.tr.Get("id-1").State != "Backlog" {
		t.Errorf("the person's state was overwritten with %s", e.tr.Get("id-1").State)
	}
	// Closed: also cleaned up.
	e, c = published(t)
	e.tr.Move("id-1", "Done")
	e.tick()
	if c.calls != 0 || e.o.claimed["id-1"] || len(e.ws.removed) != 1 {
		t.Fatalf("calls %d, removed %v", c.calls, e.ws.removed)
	}
	// Deleted.
	e, c = published(t)
	e.tr.Delete("id-1")
	e.tick()
	if c.calls != 0 || e.o.claimed["id-1"] || len(e.o.awaiting) != 0 {
		t.Fatal("a deleted ticket is still held")
	}
}

func TestChecksWaitWhenTheBoardCannotBeRead(t *testing.T) {
	e, c := published(t)
	c.status = CheckStatus{State: ChecksPassed}
	e.tr.FailReads = 1
	e.tick()
	if c.calls != 0 || len(e.o.awaiting) != 1 || !e.o.claimed["id-1"] {
		t.Fatal("acted without knowing the ticket's state")
	}
	e.tick() // not rate limited by the failed look
	if lastState(e) != "Human Review" {
		t.Fatalf("state %s", lastState(e))
	}
}
