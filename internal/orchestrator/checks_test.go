package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
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
	c.status = CheckStatus{State: ChecksPending, Detail: "1 of 2 check(s) not finished:\nunity-build: not started"}
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
	// The comment says the checks did not finish, and which; not that
	// attempts failed again and again.
	var said strings.Builder
	for _, w := range e.tr.Writes() {
		said.WriteString(w.Value + "\n")
	}
	if !strings.Contains(said.String(), "did not finish within") || !strings.Contains(said.String(), "unity-build: not started") ||
		strings.Contains(said.String(), "repeated failures") {
		t.Errorf("the blocked comment:\n%s", said.String())
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

func TestNoCheckAtAllIsPointedOutOnce(t *testing.T) {
	e := newEnv(t, todo(1))
	e.cfg.Publish.ChecksPollIntervalMS = 60000
	e.cfg.Publish.ChecksTimeoutMS = 60 * 60000
	c := &fakeChecks{status: CheckStatus{State: ChecksPending, NoneReported: true}}
	e.o.d.Checks = c
	e.tick()
	e.exit(Outcome{Kind: Published, Publish: &PublishRecord{IssueID: "id-1", Identifier: "GH-1", PRNumber: 7,
		HeadSHA: "abc", ChecksDeadline: e.clock.Now().Add(60 * time.Minute)}})
	note := "No check has started on pull request #7"
	e.clock.advance(5 * time.Minute)
	e.tick()
	if strings.Contains(comments(e), note) {
		t.Fatal("pointed out before ten minutes")
	}
	e.clock.advance(6 * time.Minute)
	e.tick()
	if n := strings.Count(comments(e), note); n != 1 || !strings.Contains(comments(e), "required_checks: none") {
		t.Fatalf("comments = %q", comments(e))
	}
	// Once only, and it is no pass: the ticket still waits.
	e.clock.advance(5 * time.Minute)
	e.tick()
	if n := strings.Count(comments(e), note); n != 1 || lastState(e) != "In Progress" || c.ready != 0 {
		t.Fatalf("notes %d, state %s, ready %d", n, lastState(e), c.ready)
	}
	// Checks that are reported but still running are not pointed out.
	e, c = published(t)
	e.cfg.Publish.ChecksTimeoutMS = 30 * 60000
	c.status = CheckStatus{State: ChecksPending}
	e.clock.advance(20 * time.Minute)
	e.tick()
	if strings.Contains(comments(e), "No check has started") {
		t.Fatal("pointed out while checks run")
	}
}

type fakeUpdater struct {
	up    BaseUpdate
	err   error
	calls []string
}

func (u *fakeUpdater) UpdateBranch(_ context.Context, is issue.Issue) (BaseUpdate, error) {
	u.calls = append(u.calls, is.Identifier)
	return u.up, u.err
}

func TestAConflictGoesBackToTheAgent(t *testing.T) {
	e, c := published(t)
	c.status = CheckStatus{State: ChecksConflict}
	e.tick()
	r := e.retry("id-1")
	if r.Kind != KindGate || r.Error != "merge_conflict" || len(e.o.awaiting) != 0 || e.b.Failures("id-1") != 1 || lastState(e) != "In Progress" {
		t.Fatalf("retry %+v, failures %d, state %s", r, e.b.Failures("id-1"), lastState(e))
	}
	e.after(time.Second)
	e.wantStarts(2)
	if a := e.w.last().attempt; len(a.Feedback) != 1 || !strings.Contains(a.Feedback[0].Body, "Resolve every conflict") {
		t.Fatalf("attempt %+v", a)
	}

	// A conflict that keeps coming back ends in the blocked state.
	e, c = published(t)
	e.b.IncFailure("id-1")
	e.b.IncFailure("id-1")
	c.status = CheckStatus{State: ChecksConflict}
	e.tick()
	if lastState(e) != "Blocked" || !strings.Contains(comments(e), "merge_conflict") {
		t.Fatalf("state %s, comments %s", lastState(e), comments(e))
	}
}

func TestAMergeBringsAWaitingPullRequestUpToDate(t *testing.T) {
	e, c := published(t)
	e.cfg.Publish.UpdateBranches = true
	e.cfg.Publish.ChecksTimeoutMS = 3600000
	u := &fakeUpdater{up: BaseUpdate{State: BaseUpdated, HeadSHA: "def", PRNumber: 7}}
	e.o.d.Updater = u
	e.tick()
	if len(u.calls) != 0 || c.calls != 1 {
		t.Fatalf("updated without a merge: %v, %d check(s)", u.calls, c.calls)
	}
	e.o.baseMoved()
	e.clock.advance(61 * time.Second)
	e.tick()
	rec := e.o.awaiting["id-1"]
	if len(u.calls) != 1 || c.calls != 1 || rec == nil || rec.HeadSHA != "def" ||
		!rec.ChecksDeadline.Equal(e.clock.Now().Add(time.Hour)) {
		t.Fatalf("updates %v, checks %d, record %+v", u.calls, c.calls, rec)
	}
	// Once per merge: the next look reads the checks of the new commit.
	e.clock.advance(61 * time.Second)
	e.tick()
	if len(u.calls) != 1 || c.calls != 2 {
		t.Fatalf("updates %v, checks %d", u.calls, c.calls)
	}

	// A base that does not merge cleanly is work for the agent.
	u.up = BaseUpdate{State: BaseConflicted, PRNumber: 7}
	e.o.baseMoved()
	e.clock.advance(61 * time.Second)
	e.tick()
	if r := e.retry("id-1"); r.Error != "merge_conflict" || len(e.o.awaiting) != 0 {
		t.Fatalf("retry %+v", r)
	}

	// Switched off: nothing is noted.
	e, _ = published(t)
	e.o.d.Updater = u
	e.o.baseMoved()
	if len(e.o.needUpdate) != 0 || e.o.updateHandoff {
		t.Fatal("update_branches is off, and an update was noted")
	}
}

// A second conflict after the agent resolved the first is a new failure,
// not the same one twice. The same commit conflicting again is.
func TestASecondConflictIsNotARepeatedFailure(t *testing.T) {
	conflictAt := func(e *env, c *fakeChecks, head string) {
		e.exit(Outcome{Kind: Published, Publish: &PublishRecord{IssueID: "id-1", Identifier: "GH-1", PRNumber: 7,
			HeadSHA: head, ChecksDeadline: e.clock.Now().Add(30 * time.Minute)}})
		c.status = CheckStatus{State: ChecksConflict}
		e.clock.advance(61 * time.Second)
		e.tick()
	}
	e, c := published(t)
	e.cfg.Budget.BlockRepeatedFailure = true
	e.cfg.Budget.MaxFailureAttempts = 10
	c.status = CheckStatus{State: ChecksConflict}
	e.tick()
	e.after(time.Second)
	e.wantStarts(2)
	conflictAt(e, c, "bcdefabcdefabcdefabcdefabcdefabcdefabcdef")
	if lastState(e) == "Blocked" {
		t.Fatalf("a conflict on a new commit blocked the ticket: %s", comments(e))
	}
	e.after(time.Minute)
	e.wantStarts(3)
	conflictAt(e, c, "bcdefabcdefabcdefabcdefabcdefabcdefabcdef")
	if lastState(e) != "Blocked" {
		t.Fatalf("the same commit conflicting twice did not block: %s", lastState(e))
	}
}

func TestABlockForFailedChecksNamesThemAndLinksToTheirLogs(t *testing.T) {
	e, c := published(t)
	var logged bytes.Buffer
	e.o.d.Log = slog.New(slog.NewTextHandler(&logged, nil))
	e.b.IncFailure("id-1")
	e.b.IncFailure("id-1")
	link := "https://github.com/ian/app/pull/7/checks"
	c.status = CheckStatus{State: ChecksFailed, Link: link,
		Detail:   "1 required check(s) failed on the pull request: unity-editmode-tests",
		Feedback: []issue.Feedback{{Source: "ci", Body: "The check unity-editmode-tests failed. Its report: PlayerMovesTest failed"}}}
	e.tick()
	if lastState(e) != "Blocked" {
		t.Fatalf("state %s", lastState(e))
	}
	var post string
	for _, w := range e.tr.Writes() {
		if w.Op == "post" {
			post = w.Value
		}
	}
	lead, fenced, _ := strings.Cut(post, "```")
	if !strings.Contains(lead, "The checks and their logs: "+link) {
		t.Errorf("the link is not in the note's own text:\n%s", post)
	}
	if !strings.Contains(fenced, "unity-editmode-tests") || strings.Contains(lead, "unity-editmode-tests") {
		t.Errorf("the check's name is not quoted below the note:\n%s", post)
	}
	// The check's report is the repository's text: the agent gets it and
	// the log keeps it, the ticket does not.
	if strings.Contains(post, "PlayerMovesTest") {
		t.Errorf("the check's report is in the ticket:\n%s", post)
	}
	if out := logged.String(); !strings.Contains(out, "required checks failed") || !strings.Contains(out, "PlayerMovesTest") {
		t.Errorf("log %q", out)
	}
}

func TestABlockSaysWhenTheDiskIsNearlyFull(t *testing.T) {
	e, c := published(t)
	note := "The container engine on this machine is nearly out of disk space (3 GB free)."
	e.o.d.DiskNote = func() string { return note }
	e.b.IncFailure("id-1")
	e.b.IncFailure("id-1")
	c.status = CheckStatus{State: ChecksFailed, Detail: "1 required check(s) failed on the pull request: build"}
	e.tick()
	var post string
	for _, w := range e.tr.Writes() {
		if w.Op == "post" {
			post = w.Value
		}
	}
	if lead, _, _ := strings.Cut(post, "```"); !strings.Contains(lead, note) {
		t.Errorf("the disk is not mentioned:\n%s", post)
	}
}
