package publish

import (
	"context"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// fakeHost is a code host in memory. It shows that the rules of the checks
// gate and the hand-off watch are the publisher's, whatever the host.
type fakeHost struct {
	list     []change
	results  map[string]check
	revs     []prReview
	notes    []comment
	written  []string
	readied  int
	deleted  []string
	commitAt time.Time
}

func (f *fakeHost) gitURL() string { return "https://example.org/ian/app.git" }
func (f *fakeHost) upsert(context.Context, string, string, string, string, bool) (int, error) {
	return 1, nil
}
func (f *fakeHost) change(_ context.Context, n int) (change, error) {
	for _, c := range f.list {
		if c.Number == n {
			return c, nil
		}
	}
	return change{}, nil
}
func (f *fakeHost) changes(context.Context, string) ([]change, error) { return f.list, nil }
func (f *fakeHost) checks(context.Context, string) (map[string]check, bool, error) {
	return f.results, true, nil
}
func (f *fakeHost) markReady(context.Context, change) error { f.readied++; return nil }
func (f *fakeHost) reviews(context.Context, int) ([]prReview, bool, error) {
	return f.revs, true, nil
}
func (f *fakeHost) comments(context.Context, int, []prReview) ([]comment, bool, error) {
	return f.notes, true, nil
}
func (f *fakeHost) comment(_ context.Context, _ int, body string) error {
	f.written = append(f.written, body)
	return nil
}
func (f *fakeHost) commitTime(context.Context, string) (time.Time, error) { return f.commitAt, nil }
func (f *fakeHost) deleteBranch(_ context.Context, b string) error {
	f.deleted = append(f.deleted, b)
	return nil
}
func (f *fakeHost) storeShots(context.Context, string, string, []worker.Screenshot) ([]shotLink, error) {
	return nil, nil
}

func fakePublisher(f *fakeHost) *Publisher {
	cfg := &config.Config{Workspace: config.Workspace{Repo: "ian/app", BaseBranch: "main"},
		Publish: config.Publish{BranchPrefix: "maestro/", ReworkOnChangesRequested: true, ReworkOnComments: true},
		Tracker: config.Tracker{AllowedCommenters: []string{"ian"}}}
	return &Publisher{Config: func() *config.Config { return cfg }, BotLogin: "bot", testHost: f,
		Now: func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) }}
}

func TestSharedRulesOnAnyHost(t *testing.T) {
	open := change{Number: 5, State: changeOpen, Draft: true, HeadSHA: headSHA, Author: "bot"}
	f := &fakeHost{list: []change{{Number: 9, State: changeOpen, Author: "mallory"}, open}}
	p := fakePublisher(f)

	// Someone else's change request on the branch is not the service's.
	st, err := p.Review(ctx, ticket)
	if err != nil || st.State != orchestrator.ReviewOpen || st.PRNumber != 5 {
		t.Fatalf("review: %+v %v", st, err)
	}

	// Only an allowlisted reviewer's verdict on the current head counts.
	f.revs = []prReview{{Verdict: verdictChanges, CommitSHA: headSHA, Login: "mallory", At: "2026-11-24T10:00:00Z"},
		{Verdict: verdictChanges, CommitSHA: "2222222222222222222222222222222222222222", Login: "ian", At: "2026-11-24T10:00:00Z"}}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("a verdict that does not count sent the ticket back: %v", st.State)
	}
	f.revs = append(f.revs, prReview{Verdict: verdictChanges, CommitSHA: headSHA, Login: "Ian", At: "2026-11-24T10:00:00Z"})
	// Its summary is empty, so it is no comment and the verdict decides.
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewChangesRequested {
		t.Errorf("changes requested: %v", st.State)
	}

	// A comment by the owner comes first, and a mark by the bot hides it.
	f.revs = nil
	f.notes = []comment{{Login: "ian", At: "2026-11-24T11:00:00Z", Body: "please rename it"}}
	st, _ = p.Review(ctx, ticket)
	if st.State != orchestrator.ReviewFeedback || st.FeedbackCount != 1 {
		t.Fatalf("comment: %+v", st)
	}
	if err := p.Acknowledge(ctx, ticket, st); err != nil || len(f.written) != 1 {
		t.Fatalf("acknowledge: %v %q", err, f.written)
	}
	f.notes = append(f.notes, comment{Login: "bot", At: "2026-11-24T11:01:00Z", Body: f.written[0]})
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("after the mark: %v", st.State)
	}
	// A mark typed by someone else is not a mark.
	f.notes = []comment{{Login: "ian", At: "2026-11-24T11:00:00Z", Body: "please rename it"},
		{Login: "mallory", At: "2026-11-24T11:01:00Z", Body: f.written[0]}}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewFeedback {
		t.Errorf("a forged mark counted: %v", st.State)
	}

	// Checks: every reported check must pass, and a draft is made ready.
	rec := orchestrator.PublishRecord{PRNumber: 5, HeadSHA: headSHA}
	f.results = map[string]check{"build": {resPassed, ""}, "lint": {resFailed, "two warnings"}}
	if cs, _ := p.Check(ctx, rec); cs.State != orchestrator.ChecksFailed || len(cs.Feedback) != 1 {
		t.Errorf("failed check: %+v", cs)
	}
	f.results = map[string]check{}
	if cs, _ := p.Check(ctx, rec); cs.State != orchestrator.ChecksPending {
		t.Errorf("no checks yet is not green: %+v", cs)
	}
	f.results = map[string]check{"build": {resPassed, ""}}
	if cs, _ := p.Check(ctx, rec); cs.State != orchestrator.ChecksPassed {
		t.Errorf("passed: %+v", cs)
	}
	if err := p.MarkReady(ctx, rec); err != nil || f.readied != 1 {
		t.Errorf("mark ready: %v %d", err, f.readied)
	}
	f.list[1].HeadSHA = "3333333333333333333333333333333333333333"
	if cs, _ := p.Check(ctx, rec); cs.State != orchestrator.ChecksForeignPush {
		t.Errorf("foreign push: %+v", cs)
	}

	if err := p.DeleteBranch(ctx, ticket); err != nil || len(f.deleted) != 1 || f.deleted[0] != "maestro/GH-7" {
		t.Errorf("delete: %v %q", err, f.deleted)
	}
}
