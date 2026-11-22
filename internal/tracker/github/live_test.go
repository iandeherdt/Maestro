//go:build live

// The live test runs the adapter against a real scratch repository. It is
// excluded from normal builds. Run it with:
//
//	MAESTRO_LIVE_REPO=owner/scratch MAESTRO_LIVE_LOGIN=owner \
//	MAESTRO_LIVE_TOKEN_VAR=GH_TOKEN go test -tags live -run TestLive ./internal/tracker/github
//
// It creates three issues and closes them again. The token's account acts
// as both the owner and the service, which is not the deployed setup, so
// the test cannot cover the rule that tells the two apart.
package github

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
)

func TestLive(t *testing.T) {
	repo, login, tokenVar := os.Getenv("MAESTRO_LIVE_REPO"), os.Getenv("MAESTRO_LIVE_LOGIN"), os.Getenv("MAESTRO_LIVE_TOKEN_VAR")
	if repo == "" || login == "" || tokenVar == "" {
		t.Skip("MAESTRO_LIVE_REPO, MAESTRO_LIVE_LOGIN and MAESTRO_LIVE_TOKEN_VAR are not set")
	}
	cfg := &config.Config{
		Tracker: config.Tracker{
			Kind: "github",
			Provider: map[string]any{"repo": repo, "state_source": "labels", "bot_login": login,
				"token": "$" + tokenVar},
			RequiredLabels: []string{"run:linux"},
			ActiveStates:   []string{"Todo", "Rework", "In Progress"}, TerminalStates: []string{"Done", "Cancelled"},
			InProgressState: "In Progress", HandoffState: "Human Review", BlockedState: "Blocked", DoneState: "Done",
			AllowedAuthors: []string{login}, AllowedCommenters: []string{login},
		},
		Worker: config.Worker{Name: "live"},
	}
	a, err := New(cfg, Options{Env: os.Getenv})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	create := func(title string, labels ...string) int {
		t.Helper()
		var gi ghIssue
		body := map[string]any{"title": title + " " + stamp, "body": "Created by the adapter's live test."}
		if len(labels) > 0 {
			body["labels"] = labels
		}
		if _, err := a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues"), nil), body, &gi, true); err != nil {
			t.Fatalf("create issue: %v", err)
		}
		t.Cleanup(func() {
			_, _ = a.c.do(ctx, http.MethodPatch, a.c.url(a.path("/issues/%d", gi.Number), nil),
				map[string]string{"state": "closed"}, nil, true)
		})
		return gi.Number
	}
	atCreate := create("labels at creation", "status:todo", "run:linux")
	later := create("labels added later")
	if _, err := a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/labels", later), nil),
		map[string][]string{"labels": {"status:todo", "run:linux"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	unrouted := create("no routing label", "status:todo")

	find := func(states ...string) map[string]issue.Issue {
		t.Helper()
		list, err := a.FetchIssuesByStates(ctx, states)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		return byIdent(list)
	}
	id := func(n int) string { return fmt.Sprintf("GH-%d", n) }

	// The issue list on github.com lags behind writes by a few seconds.
	var todo map[string]issue.Issue
	started := time.Now()
	for {
		todo = find("Todo")
		_, a1 := todo[id(atCreate)]
		_, a2 := todo[id(later)]
		_, a3 := todo[id(unrouted)]
		if (a1 && a2 && a3) || time.Since(started) > time.Minute {
			break
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("the list showed the new issues after %s", time.Since(started).Round(time.Second))
	for name, n := range map[string]int{"labels at creation": atCreate, "labels added later": later} {
		is, ok := todo[id(n)]
		if !ok {
			t.Fatalf("%s: issue %d is not listed in Todo", name, n)
		}
		if !is.Dispatchable {
			t.Errorf("%s: issue %d is not dispatchable", name, n)
		}
		if is.AuthorID == "" || is.ContentHash == "" || is.CreatedAt.IsZero() {
			t.Errorf("%s: incomplete record %+v", name, is)
		}
	}
	if is, ok := todo[id(unrouted)]; !ok || is.Dispatchable {
		t.Errorf("issue without the routing label: listed=%v dispatchable=%v", ok, is.Dispatchable)
	}

	target := todo[id(atCreate)]
	if err := a.SetState(ctx, target, "In Progress"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, err := a.FetchIssuesByIDs(ctx, []string{target.ID})
	if err != nil || len(got) != 1 {
		t.Fatalf("fetch by id: %v %v", got, err)
	}
	if issue.NormalizeState(got[0].State) != "in progress" || !got[0].Dispatchable {
		t.Errorf("after SetState: state %q dispatchable %v", got[0].State, got[0].Dispatchable)
	}
	if _, still := find("Todo")[id(atCreate)]; still {
		t.Error("the issue is still listed in Todo after SetState")
	}

	for _, body := range []string{"first status", "second status"} {
		if err := a.UpsertStatusComment(ctx, target, body); err != nil {
			t.Fatalf("status comment: %v", err)
		}
	}
	var bodies []string
	if err := a.comments(ctx, atCreate, nil, func(c ghComment) { bodies = append(bodies, c.Body) }); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "second status") {
		t.Errorf("status comments: %q", bodies)
	}

	fb, err := a.FetchFeedback(ctx, target, target.CreatedAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("feedback: %v", err)
	}
	t.Logf("feedback entries: %d", len(fb))

	bl, err := a.blockers(ctx, later)
	t.Logf("dependencies API: %d blockers, error %v", len(bl), err)

	if err := a.SetState(ctx, target, "Done"); err != nil {
		t.Fatalf("set state done: %v", err)
	}
}
