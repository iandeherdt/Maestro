package orchestrator

import (
	"strings"
	"sync"
	"testing"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/plan"
)

type notices struct {
	mu   sync.Mutex
	sent []string
}

func (n *notices) add(is issue.Issue, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, is.Identifier+": "+text)
}

func (n *notices) all() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.sent, "\n---\n")
}

func notifyEnv(t *testing.T, issues ...issue.Issue) (*env, *fakePlans, *notices) {
	t.Helper()
	e := newEnv(t, issues...)
	e.cfg.Tracker.PlanningState, e.cfg.Tracker.PlannedState = "Idea", "Planned"
	e.cfg.Tracker.ActiveStates = append(e.cfg.Tracker.ActiveStates, "Idea")
	e.cfg.Publish.ChecksPollIntervalMS = 30000
	p := &fakePlans{}
	n := &notices{}
	e.o = New(e.cfg, Deps{Tracker: e.tr, Worker: e.w, Workspaces: e.ws, Budget: e.b, Clock: e.clock, Plans: p, Notify: n.add})
	return e, p, n
}

func TestTheOwnerIsToldOfQuestionsAndPlans(t *testing.T) {
	e, _, n := notifyEnv(t, idea(1))
	e.tick()
	e.exit(Outcome{Kind: Planned, Plan: &plan.Plan{Questions: "Which colour should the button be?"}})
	if got := n.all(); !strings.Contains(got, "questions") || !strings.Contains(got, "Which colour should the button be?") {
		t.Fatalf("notices:\n%s", got)
	}
	e.tr.Move("id-1", "Idea")
	e.tick()
	e.exit(Outcome{Kind: Planned, Plan: &plan.Plan{Tasks: []plan.Task{{Title: "Paint it blue", Worker: "linux"}, {Title: "Test it", Worker: "linux"}}}})
	got := n.all()
	if !strings.Contains(got, "A plan with 2 task(s)") || !strings.Contains(got, "1. Paint it blue") || !strings.Contains(got, "2. Test it") ||
		!strings.Contains(got, "to Planned") {
		t.Fatalf("notices:\n%s", got)
	}
}

func TestTheOwnerIsToldOfABlockedTicket(t *testing.T) {
	e, _, n := notifyEnv(t, todo(1))
	e.o.block(e.ctx, e.tr.Get("id-1"), "", strings.Repeat("x", 3000)+"the last line", "", 0)
	got := n.all()
	if !strings.Contains(got, "Blocked") && !strings.Contains(got, "blocked") {
		t.Fatalf("notices:\n%s", got)
	}
	if !strings.Contains(got, "the last line") || len(got) > 2500 {
		t.Fatalf("the detail is not cut to its end: %d bytes", len(got))
	}
}

func TestNoNotifierIsFine(t *testing.T) {
	e, _ := planEnv(t, idea(1))
	e.tick()
	e.exit(Outcome{Kind: Planned, Plan: somePlan})
}
