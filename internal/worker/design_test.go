package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

type fakeDesignBrief struct {
	brief plan.DesignBrief
	err   error
}

func (f fakeDesignBrief) DesignBrief(context.Context, issue.Issue) (plan.DesignBrief, error) {
	return f.brief, f.err
}

type fakeNotes []issue.Feedback

func (f fakeNotes) Feedback(_ context.Context, is issue.Issue) ([]issue.Feedback, error) {
	if is.Identifier != "GH-1-design" {
		return nil, nil
	}
	return f, nil
}

// designRig is a rig whose issue is an idea in the design state.
func designRig(t *testing.T, turns ...script) *rig {
	t.Helper()
	r := newRig(t, turns...)
	r.cfg.Tracker.PlanningState, r.cfg.Tracker.PlannedState, r.cfg.Tracker.DesignState = "Idea", "Planned", "Design"
	r.cfg.Tracker.ActiveStates = append(r.cfg.Tracker.ActiveStates, "Idea", "Design")
	r.cfg.Design.Kind = "game"
	r.is.State = "Design"
	r.tr.Put(r.is)
	r.w.Designing = fakeDesignBrief{brief: plan.DesignBrief{Folder: "design/GH-1",
		Tasks:   []plan.Task{{Title: "Add the title screen", Body: "A start button."}},
		History: []issue.Feedback{{Source: "tracker", Author: "ian", Body: "Make it dark."}}}}
	r.w.Notes = fakeNotes{{Source: "review", Author: "ian", Body: "Bigger button.", Path: "design/GH-1/index.html", Line: 3}}
	return r
}

func TestDesignRunPublishesTheDesignWithoutVerify(t *testing.T) {
	r := designRig(t, script{text: "Done.\nMAESTRO_STATUS: done"})
	out := r.run(orchestrator.Attempt{Design: true, Feedback: []issue.Feedback{{Source: "policy", Body: "outside the folder"}}})
	if out.Kind != orchestrator.Designed || out.Design == nil || out.Design.PRNumber != 7 {
		t.Fatalf("outcome %+v", out)
	}
	if r.f.verifies != 0 || r.f.publishes != 0 {
		t.Errorf("a design ran verify or the ticket gate: verifies=%d publishes=%d", r.f.verifies, r.f.publishes)
	}
	// Its own workspace and branch, next to the idea's.
	if len(r.f.designs) != 1 || r.f.designs[0] != "GH-1-design maestro/GH-1-design design/GH-1" {
		t.Errorf("design published as %q", r.f.designs)
	}
	p := r.f.prompts[0].Prompt
	for _, want := range []string{"You are a design agent", "DESIGN_FOLDER: design/GH-1", "DESIGN_KIND: game",
		"Add the title screen", "Make it dark.", "Bigger button.", "path: design/GH-1/index.html", "GATE_FEEDBACK", "outside the folder"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "Implement GH-1") {
		t.Error("the workflow body is in a design prompt")
	}
}

func TestDesignRunStopsWhenTheCardLeavesTheDesignState(t *testing.T) {
	r := designRig(t, script{text: "MAESTRO_STATUS: done", during: func() {}})
	r.f.turns[0].during = func() {
		moved := r.is
		moved.State = "Idea"
		r.tr.Put(moved)
	}
	out := r.run(orchestrator.Attempt{Design: true})
	if out.Kind != orchestrator.Normal || len(r.f.designs) != 0 {
		t.Fatalf("outcome %+v, designs %q", out, r.f.designs)
	}
}

func TestDesignRunRejectionGoesBackAsFeedback(t *testing.T) {
	r := designRig(t, script{text: "MAESTRO_STATUS: done"})
	r.f.reject = &Rejection{Category: "design_outside_folder", Feedback: []issue.Feedback{{Source: "policy", Body: "README.md"}}}
	out := r.run(orchestrator.Attempt{Design: true})
	if out.Kind != orchestrator.Rejected || out.Category != "design_outside_folder" || len(out.Feedback) != 1 {
		t.Fatalf("outcome %+v", out)
	}
}

func TestDesignRunWithoutAPlan(t *testing.T) {
	r := designRig(t)
	r.w.Designing = fakeDesignBrief{err: plan.ErrNoPlanToDesign}
	out := r.run(orchestrator.Attempt{Design: true})
	if out.Kind != orchestrator.Failed || out.Category != DesignBriefError || len(r.f.prompts) != 0 {
		t.Fatalf("outcome %+v", out)
	}
}

func TestDesignRunNoChanges(t *testing.T) {
	r := designRig(t, script{text: "MAESTRO_STATUS: done", noChanges: true})
	out := r.run(orchestrator.Attempt{Design: true})
	if out.Kind != orchestrator.Failed || out.Category != NoChanges {
		t.Fatalf("outcome %+v", out)
	}
}

// withCurrent is the gates with pictures of the current state.
type withCurrent struct {
	Gates
	keys []string
}

func (g *withCurrent) Current(_ context.Context, ws workspace.Workspace) CurrentState {
	g.keys = append(g.keys, ws.Key)
	return CurrentState{Files: []CurrentFile{{Caption: "1-Title", Path: "../current-screenshots/1.png"}}}
}

func TestDesignRunGetsPicturesOfTheCurrentState(t *testing.T) {
	r := designRig(t, script{text: "Done.\nMAESTRO_STATUS: done"})
	g := &withCurrent{Gates: r.w.Gates}
	r.w.Gates = g
	if out := r.run(orchestrator.Attempt{Design: true}); out.Kind != orchestrator.Designed {
		t.Fatalf("outcome %+v", out)
	}
	if len(g.keys) != 1 || g.keys[0] != "GH-1-design" {
		t.Fatalf("current pictures taken for %q", g.keys)
	}
	p := r.f.prompts[0].Prompt
	for _, want := range []string{"<<<CURRENT_PICTURES", "Picture 1: 1-Title\nFile: ../current-screenshots/1.png", "Open and study every one of them"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}

func TestDesignRunWithoutCurrentPicturesSaysNothingOfThem(t *testing.T) {
	r := designRig(t, script{text: "Done.\nMAESTRO_STATUS: done"})
	r.run(orchestrator.Attempt{Design: true})
	if strings.Contains(r.f.prompts[0].Prompt, "<<<CURRENT_PICTURES") {
		t.Error("a block of current pictures without any")
	}
}
