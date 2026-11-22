package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

type fakePlans struct {
	proposed     []*plan.Plan
	proposeErr   error
	materialized []string
	summary      string
	err          error
}

func (f *fakePlans) Propose(_ context.Context, _ issue.Issue, p *plan.Plan) error {
	if f.proposeErr != nil {
		return f.proposeErr
	}
	f.proposed = append(f.proposed, p)
	return nil
}

func (f *fakePlans) Materialize(_ context.Context, is issue.Issue) (string, error) {
	f.materialized = append(f.materialized, is.ID)
	s := f.summary
	f.summary = ""
	return s, f.err
}

func idea(n int) issue.Issue {
	is := todo(n)
	is.State = "Idea"
	return is
}

// planEnv is an env with the planning stage switched on.
func planEnv(t *testing.T, issues ...issue.Issue) (*env, *fakePlans) {
	t.Helper()
	e := newEnv(t, issues...)
	e.cfg.Tracker.PlanningState, e.cfg.Tracker.PlannedState = "Idea", "Planned"
	e.cfg.Tracker.ActiveStates = append(e.cfg.Tracker.ActiveStates, "Idea")
	e.cfg.Publish.ChecksPollIntervalMS = 30000
	p := &fakePlans{}
	b := e.b
	e.o = New(e.cfg, Deps{Tracker: e.tr, Worker: e.w, Workspaces: e.ws, Budget: b, Clock: e.clock, Plans: p})
	return e, p
}

func lastComment(e *env, id string) string {
	out := ""
	for _, w := range e.tr.Writes() {
		if w.Op == "comment" && w.ID == id {
			out = w.Value
		}
	}
	return out
}

var somePlan = &plan.Plan{Tasks: []plan.Task{{Title: "A", Body: "B", Worker: "linux"}}}

func TestIdeaIsPlannedWithoutBecomingWork(t *testing.T) {
	e, p := planEnv(t, idea(1), todo(2))
	e.tick()
	e.wantStarts(2)
	var planning, coding *start
	for _, s := range e.w.starts {
		if s.attempt.Issue.ID == "id-1" {
			planning = s
		} else {
			coding = s
		}
	}
	if !planning.attempt.Planning || coding.attempt.Planning {
		t.Fatalf("planning flags: idea=%v task=%v", planning.attempt.Planning, coding.attempt.Planning)
	}
	// The idea is never moved to In Progress: after a restart it would be
	// implemented as it stands.
	if got := e.tr.StateWrites("id-1"); len(got) != 0 {
		t.Fatalf("the idea's card was moved: %v", got)
	}
	if e.tr.Get("id-1").State != "Idea" || e.tr.Get("id-2").State != "In Progress" {
		t.Fatalf("states %q %q", e.tr.Get("id-1").State, e.tr.Get("id-2").State)
	}
	if !strings.Contains(lastComment(e, "id-1"), "Planning on worker") {
		t.Errorf("comment %q", lastComment(e, "id-1"))
	}

	planning.sink.Exit(Outcome{Kind: Planned, Plan: somePlan})
	e.o.Drain(e.ctx)
	if len(p.proposed) != 1 || e.tr.Get("id-1").State != "Human Review" || e.o.claimed["id-1"] {
		t.Fatalf("proposed %d, state %q, claimed %v", len(p.proposed), e.tr.Get("id-1").State, e.o.claimed["id-1"])
	}
	if c := lastComment(e, "id-1"); !strings.Contains(c, "Move the card to Planned to approve") {
		t.Errorf("comment %q", c)
	}
	// Waiting for the owner: nothing is planned again.
	e.clock.advance(time.Minute)
	e.tick()
	e.wantStarts(2)

	// The owner answers and moves it back: a fresh planning run.
	e.tr.Move("id-1", "Idea")
	e.tick()
	e.wantStarts(3)
	if !e.w.last().attempt.Planning || e.w.last().attempt.Number != 0 {
		t.Fatalf("attempt %+v", e.w.last().attempt)
	}
	e.exit(Outcome{Kind: Planned, Plan: &plan.Plan{Questions: "Which one?"}})
	if c := lastComment(e, "id-1"); !strings.Contains(c, "has questions") || !strings.Contains(c, "back to Idea") {
		t.Errorf("comment %q", c)
	}
}

func TestPlanningFailuresAreBounded(t *testing.T) {
	e, p := planEnv(t, idea(1))
	e.tick()
	// The card cannot be moved to the hand-off state: no comment is
	// posted, and the failure counts.
	e.tr.FailWrites = 1
	e.exit(Outcome{Kind: Planned, Plan: somePlan})
	if len(p.proposed) != 0 || e.b.Failures("id-1") != 1 || e.retry("id-1").Kind != KindFailure {
		t.Fatalf("proposed %d, failures %d", len(p.proposed), e.b.Failures("id-1"))
	}
	e.after(time.Minute)
	e.wantStarts(2)
	if !e.w.last().attempt.Planning {
		t.Fatal("the retry is not a planning run")
	}
	// A plan that cannot be posted leaves the card with the owner and says so.
	p.proposeErr = errors.New("comment refused")
	e.exit(Outcome{Kind: Planned, Plan: somePlan})
	if e.tr.Get("id-1").State != "Human Review" || !strings.Contains(lastComment(e, "id-1"), "could not be posted") || e.o.claimed["id-1"] {
		t.Fatalf("state %q, comment %q", e.tr.Get("id-1").State, lastComment(e, "id-1"))
	}
	// A planning outcome without a plan is a failure, not a hand-off.
	e.tr.Move("id-1", "Idea")
	e.tick()
	e.exit(Outcome{Kind: Planned})
	if e.tr.Get("id-1").State != "Idea" || e.b.Failures("id-1") == 0 {
		t.Fatalf("state %q", e.tr.Get("id-1").State)
	}
}

func TestIdeaLeavingItsStateStopsThePlanningRun(t *testing.T) {
	e, _ := planEnv(t, idea(1))
	e.tick()
	e.tr.Move("id-1", "Backlog")
	e.tick()
	if !e.w.last().terminated || e.o.claimed["id-1"] {
		t.Fatal("the planning run kept going after the card left Idea")
	}
}

func TestWithoutPlansAnIdeaIsLeftAlone(t *testing.T) {
	e := newEnv(t, idea(1))
	e.cfg.Tracker.PlanningState, e.cfg.Tracker.PlannedState = "Idea", "Planned"
	e.cfg.Tracker.ActiveStates = append(e.cfg.Tracker.ActiveStates, "Idea")
	e.o.SetConfig(e.cfg)
	e.tick()
	e.wantStarts(0)
}

func TestApprovedPlanBecomesTasksOnlyForOwnRoutableIdeas(t *testing.T) {
	mine, other, stranger := idea(1), idea(2), idea(3)
	mine.State, other.State, stranger.State = "Planned", "Planned", "Planned"
	other.Labels = []string{"maestro", "run:mac"}
	stranger.Dispatchable = false
	e, p := planEnv(t, mine, other, stranger)
	p.summary = "The plan was approved. 2 task(s) were created."
	e.tick()
	if strings.Join(p.materialized, ",") != "id-1" {
		t.Fatalf("materialized %v", p.materialized)
	}
	if c := lastComment(e, "id-1"); !strings.Contains(c, "2 task(s) were created") {
		t.Errorf("comment %q", c)
	}
	if len(e.tr.StateWrites("id-1")) != 0 {
		t.Errorf("the approved idea was moved: %v", e.tr.StateWrites("id-1"))
	}
	e.wantStarts(0)
	// Throttled, then looked at again without a new comment.
	writes := len(e.tr.Writes())
	e.tick()
	if len(p.materialized) != 1 {
		t.Fatal("approved plans were read twice within the interval")
	}
	e.clock.advance(time.Minute)
	e.tick()
	if len(p.materialized) != 2 || len(e.tr.Writes()) != writes {
		t.Fatalf("materialized %d times, %d new writes", len(p.materialized), len(e.tr.Writes())-writes)
	}
}

func TestPlannedCardWithoutAUsablePlanIsExplainedOnce(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"no plan": {plan.ErrNoPlan, "no plan for it that you have seen"},
		"stale":   {plan.ErrStale, "edited after its plan was proposed"},
		"failing": {&tracker.Error{Category: tracker.WriteRejected, Msg: "secret detail"}, "tracker_write_rejected"},
	} {
		is := idea(1)
		is.State = "Planned"
		e, p := planEnv(t, is)
		p.err = c.err
		e.tick()
		got := lastComment(e, "id-1")
		if !strings.Contains(got, c.want) || strings.Contains(got, "secret detail") {
			t.Errorf("%s: comment %q", name, got)
		}
		writes := len(e.tr.Writes())
		e.clock.advance(time.Minute)
		e.tick()
		if len(e.tr.Writes()) != writes {
			t.Errorf("%s: the note was written again", name)
		}
		// Once it works, the result is reported.
		p.err, p.summary = nil, "Created."
		e.clock.advance(time.Minute)
		e.tick()
		if lastComment(e, "id-1") != "Created." {
			t.Errorf("%s: comment %q", name, lastComment(e, "id-1"))
		}
	}
}
