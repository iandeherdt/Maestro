package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
	"github.com/iandeherdt/Maestro/internal/tracker/trackertest"
)

type fakeDesignReviews struct {
	state DesignState
	asked []string
}

func (f *fakeDesignReviews) DesignState(_ context.Context, is issue.Issue) (DesignState, error) {
	f.asked = append(f.asked, is.Identifier)
	return f.state, nil
}

// designDesk is a desk with the design stage on and a plan proposed.
func designDesk(t *testing.T) (*Desk, *trackertest.Fake, *fakeDesignReviews) {
	t.Helper()
	d, f := desk(t)
	r := &fakeDesignReviews{}
	d.DesignState, d.DesignFolder, d.Designs = "Design", "design", r
	p, _ := Parse(twoTasks, workers)
	if err := d.Propose(ctx, idea, p); err != nil {
		t.Fatal(err)
	}
	return d, f, r
}

func created(f *trackertest.Fake) []string {
	var out []string
	for _, w := range f.Writes() {
		if w.Op == "create" {
			out = append(out, w.Value)
		}
	}
	return out
}

func TestDesignIssueAndFolder(t *testing.T) {
	di := DesignIssue(idea)
	if di.Identifier != "GH-1-design" || di.ID != idea.ID || idea.Identifier != "GH-1" {
		t.Fatalf("design issue %+v", di)
	}
	if got := DesignFolder("docs/design/", "GH-1"); got != "docs/design/GH-1" {
		t.Errorf("folder %q", got)
	}
}

func TestDesignBriefNeedsAPlan(t *testing.T) {
	d, _ := desk(t)
	d.DesignFolder = "design"
	if _, err := d.DesignBrief(ctx, idea); !errors.Is(err, ErrNoPlanToDesign) || d.HasPlan(idea.ID) {
		t.Fatalf("without a plan: %v", err)
	}
	if err := d.Propose(ctx, idea, &Plan{Questions: "Which?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DesignBrief(ctx, idea); !errors.Is(err, ErrNoPlanToDesign) || d.HasPlan(idea.ID) {
		t.Fatalf("questions are not a plan: %v", err)
	}
}

func TestDesignBriefHasThePlanAndWhatWasSaidSince(t *testing.T) {
	d, f, _ := designDesk(t)
	if !d.HasPlan(idea.ID) || d.HasDesign(idea.ID) {
		t.Fatal("plan and design flags")
	}
	// The plan was proposed at 13:00.
	f.Comments = []tracker.Feedback{
		{Author: "ian", Body: "Before the plan.", CreatedAt: time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC)},
		{Author: "ian", Body: "Make it dark.", CreatedAt: time.Date(2026, 11, 21, 13, 30, 0, 0, time.UTC)},
	}
	b, err := d.DesignBrief(ctx, idea)
	if err != nil {
		t.Fatal(err)
	}
	if b.Folder != "design/GH-1" || len(b.Tasks) != 2 || len(b.History) != 1 || b.History[0].Body != "Make it dark." {
		t.Fatalf("brief %+v", b)
	}
	if err := d.Designed(ctx, idea, Design{PRNumber: 9, Folder: "design/GH-1", Pictures: 3}); err != nil {
		t.Fatal(err)
	}
	if !d.HasDesign(idea.ID) {
		t.Fatal("the design was not recorded")
	}
	c := f.Writes()[len(f.Writes())-1].Value
	for _, want := range []string{"pull request #9", "`design/GH-1`", "3 picture(s)", "merge the pull request and move this card to **Planned**",
		"back to **Design**", "close the pull request"} {
		if !strings.Contains(c, want) {
			t.Errorf("comment lacks %q:\n%s", want, c)
		}
	}
	// A second design run is told about the first.
	if b, _ = d.DesignBrief(ctx, idea); len(b.History) != 2 || b.History[1].Source != "designer" || !strings.Contains(b.History[1].Body, "#9") {
		t.Fatalf("history %+v", b.History)
	}
}

func TestTasksWaitForAnOpenDesignAndFollowAMergedOne(t *testing.T) {
	d, f, r := designDesk(t)
	if err := d.Designed(ctx, idea, Design{PRNumber: 9, Folder: "design/GH-1"}); err != nil {
		t.Fatal(err)
	}
	var w *WaitingForDesign
	if _, err := d.Materialize(ctx, idea); !errors.As(err, &w) || w.PRNumber != 9 || len(created(f)) != 0 {
		t.Fatalf("open design: %v, created %d", err, len(created(f)))
	}
	r.state = DesignMerged
	s, err := d.Materialize(ctx, idea)
	if err != nil || !strings.Contains(s, "follow the design of pull request #9") {
		t.Fatalf("summary %q, %v", s, err)
	}
	if strings.Join(r.asked, ",") != "GH-1,GH-1" {
		t.Errorf("asked %v", r.asked)
	}
	for _, body := range created(f) {
		if !strings.Contains(body, "in the folder `design/GH-1`") || !strings.Contains(body, "pull request #9") {
			t.Errorf("task body without the design:\n%s", body)
		}
	}
}

func TestAClosedDesignIsNotUsed(t *testing.T) {
	d, f, r := designDesk(t)
	if err := d.Designed(ctx, idea, Design{PRNumber: 9, Folder: "design/GH-1"}); err != nil {
		t.Fatal(err)
	}
	r.state = DesignClosed
	s, err := d.Materialize(ctx, idea)
	if err != nil || strings.Contains(s, "design") || len(created(f)) != 2 {
		t.Fatalf("summary %q, %v", s, err)
	}
	for _, body := range created(f) {
		if strings.Contains(body, "Design:") {
			t.Errorf("a closed design reached a task:\n%s", body)
		}
	}
}

func TestANewPlanDropsTheDesign(t *testing.T) {
	d, f, r := designDesk(t)
	if err := d.Designed(ctx, idea, Design{PRNumber: 9, Folder: "design/GH-1"}); err != nil {
		t.Fatal(err)
	}
	p, _ := Parse(twoTasks, workers)
	if err := d.Propose(ctx, idea, p); err != nil {
		t.Fatal(err)
	}
	if d.HasDesign(idea.ID) {
		t.Fatal("the design of the old plan is still there")
	}
	if _, err := d.Materialize(ctx, idea); err != nil || len(r.asked) != 0 || len(created(f)) != 2 {
		t.Fatalf("materialize: %v, asked %v", err, r.asked)
	}
}
