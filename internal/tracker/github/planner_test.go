package github

import (
	"strings"
	"testing"

	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

func TestPlannerCreatesRoutesAndOrdersTasks(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Planned", "linux")
	a := boardAdapter(t, h)
	parent, _, err := a.read(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	idea, err := a.normalize(ctx, parent, nil)
	if err != nil {
		t.Fatal(err)
	}

	workers, err := a.Workers(ctx)
	if err != nil || strings.Join(workers, ",") != "linux,Mac" {
		t.Fatalf("workers %v, %v", workers, err)
	}
	first, err := a.CreateTask(ctx, tracker.Task{Title: "Add the model", Body: "Details."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.CreateTask(ctx, tracker.Task{Title: "Add the screen", Body: "More."})
	if err != nil || first.ID == second.ID || first.NativeID == 0 || !strings.HasPrefix(first.Identifier, "GH-") {
		t.Fatalf("%+v %+v %v", first, second, err)
	}
	// Created, but in no column: nothing may pick it up yet.
	if got, _ := a.FetchIssuesByStates(ctx, active); len(got) != 0 {
		t.Fatalf("a task without a card is active: %v", got)
	}
	if err := a.AddBlocker(ctx, second, first); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []tracker.TaskRef{first, second} {
		if err := a.PlaceTask(ctx, idea, ref, "mac", "Todo"); err != nil {
			t.Fatal(err)
		}
	}
	// Doing all of it again changes nothing and is not an error.
	if err := a.AddBlocker(ctx, second, first); err != nil {
		t.Fatalf("a second AddBlocker: %v", err)
	}
	if err := a.PlaceTask(ctx, idea, first, "mac", "Todo"); err != nil {
		t.Fatalf("a second PlaceTask: %v", err)
	}
	n1, _ := refNumber(first)
	n2, _ := refNumber(second)
	if len(h.issues[1].children) != 2 || len(h.issues[n2].blockers) != 1 || h.board.cards[n1].worker != "Mac" || h.board.cards[n1].status != "Todo" {
		t.Fatalf("children %v, blockers %v, card %+v", h.issues[1].children, h.issues[n2].blockers, h.board.cards[n1])
	}

	// The tasks were written by the service's own account and still count.
	cfg := boardCfg(h.srv.URL)
	cfg.Tracker.RequiredLabels = []string{"run:mac"}
	mac, err := New(cfg, Options{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mac.FetchIssuesByStates(ctx, []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	m := byIdent(got)
	if !m[first.Identifier].Dispatchable || m[second.Identifier].Dispatchable || len(m[second.Identifier].BlockedBy) != 1 {
		t.Fatalf("first %+v\nsecond %+v", m[first.Identifier], m[second.Identifier])
	}
}

func TestPlannerRefusals(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Planned", "linux")
	a := boardAdapter(t, h)
	gi, _, _ := a.read(ctx, 1)
	idea, _ := a.normalize(ctx, gi, nil)
	ref, err := a.CreateTask(ctx, tracker.Task{Title: "T", Body: "B"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ worker, state, want string }{
		"unknown worker": {"windows", "Todo", `no worker "windows"`},
		"unknown column": {"linux", "Later", `no column "Later"`},
	} {
		err := a.PlaceTask(ctx, idea, ref, c.worker, c.state)
		if tracker.CategoryOf(err) != tracker.WriteRejected || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	n, _ := refNumber(ref)
	if h.board.cards[n] != nil {
		t.Error("a refused task got a card")
	}
	if _, err := a.CreateTask(ctx, tracker.Task{Title: "  "}); tracker.CategoryOf(err) != tracker.WriteRejected {
		t.Errorf("a task without a title: %v", err)
	}
	h.noDeps = true
	if err := a.AddBlocker(ctx, ref, ref); err == nil || !strings.Contains(err.Error(), "order of the tasks") {
		t.Errorf("without dependencies: %v", err)
	}
	h.board.noWorkers = true
	if _, err := boardAdapter(t, h).Workers(ctx); err == nil {
		t.Error("a board without a Worker field lists workers")
	}
	// Planning needs a board.
	labels := adapter(t, h)
	if _, err := labels.Workers(ctx); tracker.CategoryOf(err) != tracker.InvalidConfig {
		t.Errorf("labels mode: %v", err)
	}
	if _, err := labels.CreateTask(ctx, tracker.Task{Title: "T"}); tracker.CategoryOf(err) != tracker.InvalidConfig {
		t.Errorf("labels mode: %v", err)
	}
}

func TestPostCommentIsANewComment(t *testing.T) {
	h := newBoardHub(t)
	h.card(1, "ian", "Idea", "linux")
	a := boardAdapter(t, h)
	gi, _, _ := a.read(ctx, 1)
	is, _ := a.normalize(ctx, gi, nil)
	for _, body := range []string{"First question?", "Second question?"} {
		if err := a.PostComment(ctx, is, body); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.issues[1].comments) != 2 || h.issues[1].comments[1].Body != "Second question?" {
		t.Fatalf("comments %+v", h.issues[1].comments)
	}
}
