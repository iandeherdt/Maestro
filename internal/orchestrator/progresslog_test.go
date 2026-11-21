package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/tracker/trackertest"
)

func TestStatusCommentsAreAlsoLogged(t *testing.T) {
	var buf bytes.Buffer
	o := &Orchestrator{d: Deps{Tracker: trackertest.New(), Log: slog.New(slog.NewTextHandler(&buf, nil))}}
	o.comment(context.Background(), issue.Issue{ID: "1", Identifier: "GH-1"}, "Pull request #3 opened.", "agent text")
	if !strings.Contains(buf.String(), `issue=GH-1 text="Pull request #3 opened."`) || strings.Contains(buf.String(), "agent text") {
		t.Errorf("log:\n%s", buf.String())
	}
}
