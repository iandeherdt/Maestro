package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/orchestrator"
	"github.com/iandeherdt/Symphony-beads/internal/plan"
)

type fakeBrief struct {
	brief plan.Brief
	err   error
}

func (f fakeBrief) Brief(context.Context, issue.Issue) (plan.Brief, error) { return f.brief, f.err }

const onePlan = "```json\n{\"tasks\":[{\"title\":\"Add login\",\"description\":\"Do it.\",\"worker\":\"mac\"}]}\n```\nSYMPHONY_PLAN: tasks"

// planRig is a rig whose issue is an idea.
func planRig(t *testing.T, turns ...script) *rig {
	t.Helper()
	r := newRig(t, turns...)
	r.cfg.Tracker.PlanningState, r.cfg.Tracker.PlannedState = "Idea", "Planned"
	r.cfg.Tracker.ActiveStates = append(r.cfg.Tracker.ActiveStates, "Idea")
	r.is.State = "Idea"
	r.tr.Put(r.is)
	r.w.Planning = fakeBrief{brief: plan.Brief{Workers: []string{"mac", "server"},
		History: []issue.Feedback{{Source: "planner", Body: "Which provider?"}, {Source: "tracker", Author: "ian", Body: "Stripe."}}}}
	return r
}

func TestPlanningRunReturnsAPlanAndPublishesNothing(t *testing.T) {
	r := planRig(t, script{text: onePlan, verifyOK: true})
	out := r.run(orchestrator.Attempt{Planning: true})
	if out.Kind != orchestrator.Planned || out.Plan == nil || len(out.Plan.Tasks) != 1 || out.Plan.Tasks[0].Worker != "mac" {
		t.Fatalf("outcome %+v", out)
	}
	if r.f.verifies != 0 || r.f.publishes != 0 || strings.Contains(r.sink.phases(), "Exporting") {
		t.Errorf("a planning run reached the gates: verifies=%d publishes=%d phases=%s", r.f.verifies, r.f.publishes, r.sink.phases())
	}
	p := r.f.prompts[0].Prompt
	for _, want := range []string{"You are a planning agent", "IDEA_TITLE", "Fix login", "WORKERS", "mac\nserver", "EARLIER_ROUND", "Which provider?", "OWNER_COMMENT", "author=ian", "Stripe."} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	// The coding instructions are not part of a planning prompt.
	if strings.Contains(p, "Implement GH-1") || strings.Contains(p, "SYMPHONY_STATUS") || strings.Contains(p, "Commit your work") {
		t.Errorf("coding instructions in a planning prompt:\n%s", p)
	}
	if r.lastSandboxStep() != "hook:after_run" && !strings.Contains(r.lastSandboxStep(), "after_run") {
		t.Errorf("after_run did not run last: %s", r.lastSandboxStep())
	}
}

func TestPlanningQuestions(t *testing.T) {
	r := planRig(t, script{text: "Which screen first?\nSYMPHONY_PLAN: questions"})
	out := r.run(orchestrator.Attempt{Planning: true})
	if out.Kind != orchestrator.Planned || out.Plan.Questions != "Which screen first?" {
		t.Fatalf("outcome %+v", out)
	}
}

func TestMalformedPlanIsSentBackThenFails(t *testing.T) {
	r := planRig(t, script{text: "I think we should do it."}, script{text: onePlan})
	out := r.run(orchestrator.Attempt{Planning: true})
	if out.Kind != orchestrator.Planned {
		t.Fatalf("outcome %+v", out)
	}
	second := r.f.prompts[1]
	if second.ResumeSessionID != "sess-1" || !strings.Contains(second.Prompt, "could not be used") || !strings.Contains(second.Prompt, "SYMPHONY_PLAN") ||
		strings.Contains(second.Prompt, "I think we should") {
		t.Errorf("second prompt %+v", second)
	}

	bad := script{text: "```json\n{\"tasks\":[{\"title\":\"T\",\"description\":\"D\",\"worker\":\"windows\"}]}\n```\nSYMPHONY_PLAN: tasks"}
	r = planRig(t, bad, bad, bad)
	out = r.run(orchestrator.Attempt{Planning: true})
	if out.Kind != orchestrator.Failed || out.Category != PlanMalformed || len(r.f.prompts) != 3 {
		t.Fatalf("outcome %+v after %d turns", out, len(r.f.prompts))
	}
}

func TestPlanForAnIdeaThatMovedOrChangedIsDropped(t *testing.T) {
	for name, during := range map[string]func(r *rig){
		"moved":  func(r *rig) { r.tr.Move(r.is.ID, "Todo") },
		"edited": func(r *rig) { is := r.is; is.ContentHash = "other"; r.tr.Put(is) },
		"gone":   func(r *rig) { r.tr.Delete(r.is.ID) },
	} {
		var r *rig
		r = planRig(t, script{text: onePlan, during: func() { during(r) }})
		if out := r.run(orchestrator.Attempt{Planning: true}); out.Kind != orchestrator.Normal || out.Plan != nil {
			t.Errorf("%s: outcome %+v", name, out)
		}
	}
}

func TestPlanningNeedsItsBrief(t *testing.T) {
	r := planRig(t, script{text: onePlan})
	r.w.Planning = fakeBrief{err: errors.New("board unreachable")}
	if out := r.run(orchestrator.Attempt{Planning: true}); out.Kind != orchestrator.Failed || out.Category != PlanBriefError || len(r.f.prompts) != 0 {
		t.Fatalf("outcome %+v", out)
	}
	r = planRig(t, script{text: onePlan})
	r.w.Planning = nil
	if out := r.run(orchestrator.Attempt{Planning: true}); out.Kind != orchestrator.Failed || out.Category != PlanBriefError {
		t.Fatalf("outcome %+v", out)
	}
}

func TestPlanningQuotaIsNotAFailure(t *testing.T) {
	r := planRig(t, script{err: &AgentError{Category: QuotaExhausted, Msg: "limit"}})
	if out := r.run(orchestrator.Attempt{Planning: true}); out.Kind != orchestrator.Quota {
		t.Fatalf("outcome %+v", out)
	}
}
