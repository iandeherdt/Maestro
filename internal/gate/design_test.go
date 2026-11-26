package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

type designPub struct {
	current []worker.Screenshot
	shots   []worker.Screenshot
	note    string
	folder  string
}

func (p *designPub) Publish(context.Context, issue.Issue, workspace.Workspace, worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error) {
	return nil, nil, nil
}

func (p *designPub) PublishDesign(_ context.Context, _ issue.Issue, _ workspace.Workspace, folder string, current, shots []worker.Screenshot, note string) (*plan.Design, *worker.Rejection, error) {
	p.current, p.shots, p.note, p.folder = current, shots, note, folder
	return &plan.Design{PRNumber: 3, Pictures: len(shots)}, nil, nil
}

func (l *local) write(files map[string]string) {
	l.t.Helper()
	for name, body := range files {
		p := filepath.Join(l.repo, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
	l.git("add", "-A")
	l.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "design")
}

func TestDesignPagesStartWithTheOverview(t *testing.T) {
	l := newLocal(t)
	l.write(map[string]string{
		"design/GH-1/b.html": "b", "design/GH-1/a/z.html": "z", "design/GH-1/index.html": "i",
		"design/GH-1/style.css": "c", "design/GH-1/bad name.html": "x", "design/GH-10/other.html": "o", "other.html": "o",
	})
	g := l.gates(config.Screenshots{})
	pages, ok := g.designPages(ctx, "GH-1-design", "design/GH-1")
	if !ok || strings.Join(pages, ",") != "design/GH-1/index.html,design/GH-1/a/z.html,design/GH-1/b.html" {
		t.Fatalf("pages %q, ok %v", pages, ok)
	}
	if pages, ok := g.designPages(ctx, "GH-1-design", "design/GH-2"); !ok || len(pages) != 0 {
		t.Fatalf("a missing folder: %q %v", pages, ok)
	}
}

func TestDesignWithoutPagesIsPublishedWithANote(t *testing.T) {
	l := newLocal(t)
	l.write(map[string]string{"design/GH-1/README.md": "Only words."})
	pub := &designPub{}
	g := l.gates(config.Screenshots{})
	g.Publisher = pub
	d, rej, err := g.Design(ctx, issue.Issue{Identifier: "GH-1"}, workspace.Workspace{Key: "GH-1-design"}, "design/GH-1")
	if err != nil || rej != nil || d.PRNumber != 3 || pub.folder != "design/GH-1" || pub.note != "No pictures: the design has no HTML page." {
		t.Fatalf("%+v %+v %v, note %q", d, rej, err, pub.note)
	}
}

func TestDesignPicturesWithARealBrowser(t *testing.T) {
	path := browser(t)
	l := newLocal(t)
	os.WriteFile(filepath.Join(l.bin, "chromium"), []byte("#!/bin/sh\nexec "+path+" \"$@\"\n"), 0o755)
	l.write(map[string]string{
		"design/GH-1/index.html": "<!doctype html><h1 style='color:#c00'>Overview</h1>",
		"design/GH-1/play.html":  "<!doctype html><canvas></canvas>",
	})
	pub := &designPub{}
	g := l.gates(config.Screenshots{Width: 640, Height: 400})
	g.Publisher = pub
	if _, _, err := g.Design(ctx, issue.Issue{Identifier: "GH-1"}, workspace.Workspace{Key: "GH-1-design"}, "design/GH-1"); err != nil {
		t.Fatal(err)
	}
	if len(pub.shots) != 2 || pub.shots[0].Caption != "design/GH-1/index.html" || pub.shots[1].Caption != "design/GH-1/play.html" || pub.note != "" {
		t.Fatalf("shots %d, note %q", len(pub.shots), pub.note)
	}
}
