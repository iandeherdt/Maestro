package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
)

type fakeReviews struct {
	status  map[string]ReviewStatus
	err     error
	delErr  error
	calls   int
	deleted []string
	acked   []ReviewStatus
	ackErr  error
}

func (r *fakeReviews) Acknowledge(_ context.Context, _ issue.Issue, st ReviewStatus) error {
	r.acked = append(r.acked, st)
	return r.ackErr
}

func (r *fakeReviews) Review(_ context.Context, is issue.Issue) (ReviewStatus, error) {
	r.calls++
	return r.status[is.ID], r.err
}

func (r *fakeReviews) DeleteBranch(_ context.Context, is issue.Issue) error {
	r.deleted = append(r.deleted, is.Identifier)
	return r.delErr
}

func inReview(n int) issue.Issue {
	is := todo(n)
	is.State = "Human Review"
	return is
}

func handedOff(t *testing.T, st ReviewState) (*env, *fakeReviews) {
	t.Helper()
	e := newEnv(t, inReview(1))
	e.cfg.Publish.ChecksPollIntervalMS = 60000
	e.cfg.Publish.ReworkOnChangesRequested = true
	e.cfg.Tracker.DoneState = "Done"
	r := &fakeReviews{status: map[string]ReviewStatus{"id-1": {State: st, PRNumber: 7}}}
	e.o.d.Reviews = r
	return e, r
}

func comments(e *env) string {
	var out []string
	for _, w := range e.tr.Writes() {
		if w.Op == "comment" {
			out = append(out, w.Value)
		}
	}
	return strings.Join(out, "\n")
}

func TestMergedPullRequestClosesTheTicket(t *testing.T) {
	e, r := handedOff(t, ReviewMerged)
	e.b.IncFailure("id-1")
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) != 1 || got[0] != "Done" {
		t.Fatalf("state writes = %v", got)
	}
	if len(r.deleted) != 1 || r.deleted[0] != "GH-1" || len(e.ws.removed) != 1 || e.b.Failures("id-1") != 0 {
		t.Fatalf("deleted %v, removed %v, failures %d", r.deleted, e.ws.removed, e.b.Failures("id-1"))
	}
	if !strings.Contains(comments(e), "Pull request #7 was merged.") {
		t.Fatalf("comments = %q", comments(e))
	}
	e.wantStarts(0)
}

func TestMergeIsRetriedWhenTheStateWriteFails(t *testing.T) {
	e, r := handedOff(t, ReviewMerged)
	r.delErr = errors.New("gone")
	e.tr.FailWrites = 1
	e.tick()
	if len(e.tr.StateWrites("id-1")) != 0 || len(e.ws.removed) != 0 {
		t.Fatalf("writes %v, removed %v", e.tr.StateWrites("id-1"), e.ws.removed)
	}
	// Not before the interval has passed.
	e.clock.advance(30 * time.Second)
	e.tick()
	if r.calls != 1 {
		t.Fatalf("looked again after 30s: %d calls", r.calls)
	}
	e.clock.advance(31 * time.Second)
	e.tick()
	// A branch that could not be deleted does not stop the ticket closing.
	if got := e.tr.StateWrites("id-1"); len(got) != 1 || got[0] != "Done" || len(e.ws.removed) != 1 {
		t.Fatalf("writes %v, removed %v", got, e.ws.removed)
	}
}

func TestChangesRequestedSendsTheTicketToRework(t *testing.T) {
	e, _ := handedOff(t, ReviewChangesRequested)
	e.tick()
	// The same tick picks the ticket up again as active work.
	if got := e.tr.StateWrites("id-1"); len(got) != 2 || got[0] != "Rework" || got[1] != "In Progress" {
		t.Fatalf("state writes = %v", got)
	}
	e.wantStarts(1)

	e, _ = handedOff(t, ReviewChangesRequested)
	e.cfg.Publish.ReworkOnChangesRequested = false
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) != 0 {
		t.Fatalf("switched off, state writes = %v", got)
	}

	// A board without a Rework state is not given one.
	e, _ = handedOff(t, ReviewChangesRequested)
	e.cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) != 0 {
		t.Fatalf("no rework state, state writes = %v", got)
	}
}

func TestClosedPullRequestIsNotedOnce(t *testing.T) {
	e, r := handedOff(t, ReviewClosed)
	for i := 0; i < 3; i++ {
		e.tick()
		e.clock.advance(61 * time.Second)
	}
	if n := strings.Count(comments(e), "closed without being merged"); n != 1 || len(e.tr.StateWrites("id-1")) != 0 || len(r.deleted) != 0 {
		t.Fatalf("%d notes, writes %v", n, e.tr.StateWrites("id-1"))
	}
	// Reopened and closed again: noted again.
	r.status["id-1"] = ReviewStatus{State: ReviewOpen, PRNumber: 7}
	e.tick()
	e.clock.advance(61 * time.Second)
	r.status["id-1"] = ReviewStatus{State: ReviewClosed, PRNumber: 7}
	e.tick()
	if n := strings.Count(comments(e), "closed without being merged"); n != 2 {
		t.Fatalf("%d notes after reopening", n)
	}
}

func TestHandoffWatchLeavesOtherTicketsAlone(t *testing.T) {
	for name, st := range map[string]ReviewState{"open": ReviewOpen, "no pull request": ReviewNone} {
		e, r := handedOff(t, st)
		e.tick()
		if len(e.tr.Writes()) != 0 || len(r.deleted) != 0 {
			t.Errorf("%s: writes %v", name, e.tr.Writes())
		}
	}
	// Another worker's ticket is not even looked at.
	other := inReview(1)
	other.Labels = []string{"maestro", "run:mac"}
	e := newEnv(t, other)
	r := &fakeReviews{status: map[string]ReviewStatus{"id-1": {State: ReviewMerged, PRNumber: 7}}}
	e.o.d.Reviews = r
	e.tick()
	if r.calls != 0 || len(e.tr.Writes()) != 0 {
		t.Fatalf("calls %d, writes %v", r.calls, e.tr.Writes())
	}
	// A read error changes nothing.
	e, r = handedOff(t, ReviewMerged)
	r.err = errors.New("status 502")
	e.tick()
	if len(e.tr.Writes()) != 0 || len(r.deleted) != 0 {
		t.Fatalf("writes %v", e.tr.Writes())
	}
}

func TestCommentsOnThePullRequestSendTheTicketBack(t *testing.T) {
	e, r := handedOff(t, ReviewFeedback)
	st := r.status["id-1"]
	st.FeedbackCount, st.FeedbackThrough = 2, time.Unix(1000, 0)
	r.status["id-1"] = st
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) == 0 || got[0] != "Rework" {
		t.Fatalf("state writes = %v", got)
	}
	if len(r.acked) != 1 || r.acked[0].FeedbackCount != 2 || !r.acked[0].FeedbackThrough.Equal(time.Unix(1000, 0)) {
		t.Fatalf("acknowledged = %+v", r.acked)
	}
}

func TestCommentsAreNotAcknowledgedWhenTheStateWriteFails(t *testing.T) {
	e, r := handedOff(t, ReviewFeedback)
	e.tr.FailWrites = 1
	e.tick()
	if len(r.acked) != 0 || len(e.tr.StateWrites("id-1")) != 0 {
		t.Fatalf("acknowledged %v, writes %v", r.acked, e.tr.StateWrites("id-1"))
	}
}

func TestAFailedAcknowledgementStillSendsTheTicketBack(t *testing.T) {
	e, r := handedOff(t, ReviewFeedback)
	r.ackErr = errors.New("no")
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) == 0 || got[0] != "Rework" {
		t.Fatalf("state writes = %v", got)
	}
}
