package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/plan"
)

type fakeDesigns struct {
	plans, designs map[string]bool
	designed       []plan.Design
	err            error
}

func (f *fakeDesigns) HasPlan(id string) bool   { return f.plans[id] }
func (f *fakeDesigns) HasDesign(id string) bool { return f.designs[id] }
func (f *fakeDesigns) Designed(_ context.Context, is issue.Issue, d plan.Design) error {
	if f.err != nil {
		return f.err
	}
	f.designed = append(f.designed, d)
	f.designs[is.ID] = true
	return nil
}

// identReviews answers by identifier, so that an idea and its design are
// told apart.
type identReviews struct {
	fakeReviews
	byIdent map[string]ReviewStatus
	asked   []string
}

func (r *identReviews) Review(_ context.Context, is issue.Issue) (ReviewStatus, error) {
	r.asked = append(r.asked, is.Identifier)
	return r.byIdent[is.Identifier], nil
}

func (r *identReviews) Acknowledge(_ context.Context, is issue.Issue, st ReviewStatus) error {
	r.asked = append(r.asked, "ack "+is.Identifier)
	return nil
}

func designEnv(t *testing.T, issues ...issue.Issue) (*env, *fakePlans, *fakeDesigns, *identReviews) {
	t.Helper()
	e := newEnv(t, issues...)
	e.cfg.Tracker.PlanningState, e.cfg.Tracker.PlannedState, e.cfg.Tracker.DesignState = "Idea", "Planned", "Design"
	e.cfg.Tracker.ActiveStates = append(e.cfg.Tracker.ActiveStates, "Idea", "Design")
	e.cfg.Publish.ChecksPollIntervalMS = 30000
	p := &fakePlans{}
	d := &fakeDesigns{plans: map[string]bool{}, designs: map[string]bool{}}
	r := &identReviews{byIdent: map[string]ReviewStatus{}}
	e.o = New(e.cfg, Deps{Tracker: e.tr, Worker: e.w, Workspaces: e.ws, Budget: e.b, Clock: e.clock,
		Plans: p, Designs: d, Reviews: r})
	return e, p, d, r
}

func inDesign(n int) issue.Issue {
	is := todo(n)
	is.State = "Design"
	return is
}

func TestDesignIsMadeInItsOwnStateAndHandedToTheOwner(t *testing.T) {
	e, _, d, _ := designEnv(t, inDesign(1))
	d.plans["id-1"] = true
	e.tick()
	e.wantStarts(1)
	a := e.w.last().attempt
	if !a.Design || a.Planning {
		t.Fatalf("attempt %+v", a)
	}
	// Like an idea being planned, the card stays where the owner put it.
	if got := e.tr.StateWrites("id-1"); len(got) != 0 {
		t.Fatalf("the card was moved: %v", got)
	}
	if !strings.Contains(lastComment(e, "id-1"), "Making a design on worker") {
		t.Errorf("comment %q", lastComment(e, "id-1"))
	}
	e.exit(Outcome{Kind: Designed, Design: &plan.Design{PRNumber: 9, Folder: "design/GH-1"}})
	if len(d.designed) != 1 || e.tr.Get("id-1").State != "Human Review" || e.o.claimed["id-1"] {
		t.Fatalf("designed %v, state %q, claimed %v", d.designed, e.tr.Get("id-1").State, e.o.claimed["id-1"])
	}
	e.clock.advance(time.Minute)
	e.tick()
	e.wantStarts(1)
}

func TestDesignWithoutAPlanIsExplainedOnce(t *testing.T) {
	e, _, _, _ := designEnv(t, inDesign(1))
	e.tick()
	e.wantStarts(0)
	if !strings.Contains(lastComment(e, "id-1"), "move the card to Idea first") {
		t.Fatalf("comment %q", lastComment(e, "id-1"))
	}
	writes := len(e.tr.Writes())
	e.clock.advance(time.Minute)
	e.tick()
	if len(e.tr.Writes()) != writes {
		t.Error("the note was written again")
	}
}

func TestDesignOutcomeWithoutADesignIsAFailure(t *testing.T) {
	e, _, d, _ := designEnv(t, inDesign(1))
	d.plans["id-1"] = true
	e.tick()
	e.exit(Outcome{Kind: Designed})
	if e.tr.Get("id-1").State != "Design" || e.b.Failures("id-1") != 1 {
		t.Fatalf("state %q, failures %d", e.tr.Get("id-1").State, e.b.Failures("id-1"))
	}
}

func TestCommentsOnTheDesignSendTheIdeaBackToDesign(t *testing.T) {
	e, _, d, r := designEnv(t, inReview(1))
	d.designs["id-1"] = true
	r.byIdent["GH-1-design"] = ReviewStatus{State: ReviewFeedback, PRNumber: 9, FeedbackCount: 2}
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) != 1 || got[0] != "Design" {
		t.Fatalf("state writes %v", got)
	}
	if strings.Join(r.asked, ",") != "GH-1,GH-1-design,ack GH-1-design" {
		t.Errorf("asked %v", r.asked)
	}
}

func TestMergedDesignDoesNotFinishTheIdea(t *testing.T) {
	e, _, d, r := designEnv(t, inReview(1))
	d.designs["id-1"] = true
	r.byIdent["GH-1-design"] = ReviewStatus{State: ReviewMerged, PRNumber: 9}
	e.tick()
	if got := e.tr.StateWrites("id-1"); len(got) != 0 {
		t.Fatalf("state writes %v", got)
	}
}

func TestIdeaWithoutDesignIsNotLookedUpTwice(t *testing.T) {
	e, _, _, r := designEnv(t, inReview(1))
	e.tick()
	if strings.Join(r.asked, ",") != "GH-1" {
		t.Errorf("asked %v", r.asked)
	}
}

func TestTasksWaitForTheDesignAndTidyUpAfter(t *testing.T) {
	is := idea(1)
	is.State = "Planned"
	e, p, d, r := designEnv(t, is)
	d.designs["id-1"] = true
	p.err = &plan.WaitingForDesign{PRNumber: 9}
	e.tick()
	if c := lastComment(e, "id-1"); !strings.Contains(c, "design in pull request #9 is merged") {
		t.Fatalf("comment %q", c)
	}
	p.err, p.summary = nil, "Created."
	e.clock.advance(time.Minute)
	e.tick()
	if lastComment(e, "id-1") != "Created." {
		t.Fatalf("comment %q", lastComment(e, "id-1"))
	}
	if strings.Join(r.deleted, ",") != "GH-1-design" {
		t.Errorf("deleted %v", r.deleted)
	}
}
