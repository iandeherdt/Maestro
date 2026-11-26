package worker

import (
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

type fakeTalks struct {
	pending map[string]string
	done    []string
}

func (f *fakeTalks) Pending(key string) string { return f.pending[key] }
func (f *fakeTalks) Done(key string) {
	f.done = append(f.done, key)
	delete(f.pending, key)
}

const talkedID = "0b6c4a52-6f11-4c1e-9a3e-2d7f8f0d1a11"

func TestPlanningContinuesTheOwnersConversation(t *testing.T) {
	r := planRig(t, script{text: onePlan, verifyOK: true})
	talks := &fakeTalks{pending: map[string]string{"GH-1": talkedID}}
	r.w.Conversations = talks
	out := r.run(orchestrator.Attempt{Planning: true})
	if out.Kind != orchestrator.Planned {
		t.Fatalf("outcome %+v", out)
	}
	first := r.f.prompts[0]
	if first.ResumeSessionID != talkedID {
		t.Errorf("the first turn resumed %q, not the conversation", first.ResumeSessionID)
	}
	if !strings.HasPrefix(first.Prompt, talkedNote) || !strings.Contains(first.Prompt, "You are a planning agent") {
		t.Errorf("prompt:\n%s", first.Prompt)
	}
	if len(talks.done) != 1 || talks.done[0] != "GH-1" {
		t.Errorf("the conversation was not forgotten after it was continued: %v", talks.done)
	}
}

func TestRunContinuesTheOwnersConversation(t *testing.T) {
	r := newRig(t, script{text: "done\nMAESTRO_STATUS: complete", verifyOK: true})
	talks := &fakeTalks{pending: map[string]string{"GH-1": talkedID}}
	r.w.Conversations = talks
	r.run(orchestrator.Attempt{})
	if r.f.prompts[0].ResumeSessionID != talkedID || !strings.HasPrefix(r.f.prompts[0].Prompt, talkedNote) {
		t.Errorf("first turn %+v", r.f.prompts[0])
	}
	if len(talks.done) != 1 {
		t.Errorf("done %v", talks.done)
	}
}

func TestNoConversationStartsANewSession(t *testing.T) {
	r := planRig(t, script{text: onePlan, verifyOK: true})
	r.w.Conversations = &fakeTalks{pending: map[string]string{"GH-9": talkedID}}
	r.run(orchestrator.Attempt{Planning: true})
	if r.f.prompts[0].ResumeSessionID != "" || strings.HasPrefix(r.f.prompts[0].Prompt, talkedNote) {
		t.Errorf("first turn %+v", r.f.prompts[0])
	}
}
