package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/ticketimage"
	"github.com/iandeherdt/Maestro/internal/tracker/trackertest"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

type picTracker struct {
	*trackertest.Fake
	res ticketimage.Result
	err error
}

func (p picTracker) TicketImages(context.Context, issue.Issue) (ticketimage.Result, error) {
	return p.res, p.err
}

type picGates struct {
	*fakes
	got []ticketimage.Image
	ok  bool
}

func (g *picGates) TicketImages(_ context.Context, _ workspace.Workspace, imgs []ticketimage.Image) ([]string, bool) {
	g.got = imgs
	var paths []string
	for i := range imgs {
		paths = append(paths, "../ticket-images/"+string(rune('1'+i))+".png")
	}
	return paths, g.ok
}

func TestTicketPicturesReachTheAgent(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	at := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
	r.w.Tracker = picTracker{Fake: r.tr, res: ticketimage.Result{
		Images: []ticketimage.Image{{Where: "description", PNG: []byte("a")}, {Where: "comment", Author: "ian", At: at, PNG: []byte("b")}},
		Note:   "1 picture on the ticket could not be downloaded or read.",
	}}
	g := &picGates{fakes: r.f, ok: true}
	r.w.Gates = g
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Published {
		t.Fatalf("outcome %+v", out)
	}
	if len(g.got) != 2 {
		t.Fatalf("gates got %d pictures", len(g.got))
	}
	p := r.f.prompts[0].Prompt
	for _, want := range []string{
		"- ../ticket-images/1.png, from the description\n",
		"- ../ticket-images/2.png, from a comment by ian written 2026-11-26T09:30Z\n",
		"1 picture on the ticket could not be downloaded or read. Do not try to download them yourself",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	// The pictures come before the untrusted blocks.
	if strings.Index(p, "Pictures on the ticket") > strings.Index(p, "<<<ISSUE_TITLE") {
		t.Fatal("the pictures are described after the task data")
	}
}

func TestTicketPicturesNeverFailTheWork(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.w.Tracker = picTracker{Fake: r.tr, err: errors.New("tracker_status: issue 1 is not readable")}
	r.w.Gates = &picGates{fakes: r.f, ok: true}
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Published {
		t.Fatalf("outcome %+v", out)
	}
	p := r.f.prompts[0].Prompt
	if !strings.Contains(p, "could not be downloaded.") || strings.Contains(p, "../ticket-images/") {
		t.Fatalf("prompt:\n%s", p)
	}
}

func TestNoPicturesNoWords(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.w.Tracker = picTracker{Fake: r.tr}
	r.w.Gates = &picGates{fakes: r.f, ok: true}
	r.run(orchestrator.Attempt{})
	if strings.Contains(r.f.prompts[0].Prompt, "Pictures on the ticket") {
		t.Fatal("a ticket without pictures got a pictures section")
	}
}
