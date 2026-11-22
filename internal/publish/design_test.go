package publish

import (
	"regexp"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

func designLab(t *testing.T) *lab {
	t.Helper()
	l := newLab(t)
	l.ws = workspace.Workspace{Key: "GH-7-design", Branch: "maestro/GH-7-design", BaseSHA: l.base}
	l.hub.head = l.ws.Branch
	return l
}

func TestDesignOpensAPullRequestThatClosesNothing(t *testing.T) {
	l := designLab(t)
	l.change("design/GH-7/index.html", "<h1>Shop</h1>\n")
	head := l.change("design/GH-7/README.md", "Colours.\n")
	shots := []worker.Screenshot{{Caption: "design/GH-7/index.html", PNG: []byte("one")}}
	d, rej, err := l.pub.PublishDesign(ctx, l.is, l.ws, "design/GH-7", shots, "")
	if err != nil || rej != nil {
		t.Fatalf("rejection %+v, error %v", rej, err)
	}
	if d.PRNumber != 31 || d.HeadSHA != head || d.Branch != "maestro/GH-7-design" || d.Folder != "design/GH-7" || d.Pictures != 1 {
		t.Errorf("design %+v", d)
	}
	if l.remoteHead() != head {
		t.Errorf("remote branch at %q", l.remoteHead())
	}
	created := l.hub.sent("POST /repos/ian/app/pulls")[0]
	if created["title"] != "Design for GH-7: Fix the login page" || created["draft"] != false || created["head"] != "maestro/GH-7-design" {
		t.Errorf("created with %v", created)
	}
	text := created["body"].(string)
	for _, want := range []string{"A design for the plan of #7", "`design/GH-7/`", "To approve it", "merge this pull request",
		"## Pictures", "![Picture 1](", "2 commit(s), 2 file(s) changed, all below the design folder", footer} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %q:\n%s", want, text)
		}
	}
	// No word that makes GitHub close the idea when the design is merged.
	if regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?):? +#`).MatchString(text) {
		t.Errorf("a design closes the idea:\n%s", text)
	}
}

func TestDesignOutsideItsFolderIsRefused(t *testing.T) {
	l := designLab(t)
	l.change("design/GH-7/index.html", "<h1>Shop</h1>\n")
	l.change("src/app.js", "x()\n")
	d, rej, err := l.pub.PublishDesign(ctx, l.is, l.ws, "design/GH-7", nil, "No pictures.")
	if err != nil || d != nil || rej == nil {
		t.Fatalf("%+v %+v %v", d, rej, err)
	}
	if rej.Category != DesignOutside || len(rej.Feedback) != 1 || !strings.Contains(rej.Feedback[0].Body, "src/app.js") {
		t.Errorf("rejection %+v", rej)
	}
	if l.remoteHead() != "" || len(l.hub.requests) != 0 {
		t.Errorf("something was published: branch %q, requests %v", l.remoteHead(), l.hub.requests)
	}
	// A folder of the same name but longer is outside too.
	l = designLab(t)
	l.change("design/GH-70/index.html", "x\n")
	if _, rej, _ := l.pub.PublishDesign(ctx, l.is, l.ws, "design/GH-7", nil, ""); rej == nil {
		t.Error("design/GH-70 counted as inside design/GH-7")
	}
}

func TestDesignStillGoesThroughThePolicy(t *testing.T) {
	l := designLab(t)
	l.change("design/GH-7/.github/workflows/ci.yml", "on: push\n")
	_, rej, err := l.pub.PublishDesign(ctx, l.is, l.ws, "design/GH-7", nil, "")
	if err != nil || rej == nil || rej.Category != PolicyDenied {
		t.Fatalf("%+v %v", rej, err)
	}
}

func TestDesignStateFollowsThePullRequestOfTheDesignBranch(t *testing.T) {
	g, pub := newGH(t)
	for state, want := range map[string]plan.DesignState{"merged": plan.DesignMerged, "closed": plan.DesignClosed, "open": plan.DesignOpen, "none": plan.DesignClosed} {
		merged, st := "", state
		if state == "merged" {
			merged, st = "2026-11-21T12:00:00Z", "closed"
		}
		pr := pullOn(31, st, merged, "sym[bot]", "ian/app")
		pr["head"].(map[string]any)["ref"] = "maestro/GH-7-design"
		g.pulls = []map[string]any{pr}
		if state == "none" {
			// Only the idea's own branch has a pull request.
			g.pulls = []map[string]any{pullOn(31, "open", "", "sym[bot]", "ian/app")}
		}
		got, err := pub.DesignState(ctx, ticket)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", state, got, err)
		}
	}
	if q := g.queries[0]; !strings.Contains(q, "maestro%2FGH-7-design") {
		t.Errorf("query %q", q)
	}
}
