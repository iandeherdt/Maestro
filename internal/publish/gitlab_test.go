package publish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// fakeLab is a stand-in for GitLab's API, for the project group/sub/app.
type fakeLab struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]map[string]any
	mrs      []map[string]any
	statuses []map[string]any
	notes    []map[string]any
	headAt   string
	status   map[string]int
}

const labProject = "/api/v4/projects/group%2Fsub%2Fapp"

func (l *fakeLab) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	l.requests = append(l.requests, key)
	if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-GitHub-Api-Version") != "" {
		w.WriteHeader(401)
		return
	}
	if code := l.status[key]; code != 0 {
		w.WriteHeader(code)
		return
	}
	var in map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		json.NewDecoder(r.Body).Decode(&in)
	} else {
		io.Copy(io.Discard, r.Body)
	}
	if l.bodies == nil {
		l.bodies = map[string]map[string]any{}
	}
	l.bodies[key] = in
	enc := json.NewEncoder(w)
	switch {
	case key == "GET "+labProject:
		enc.Encode(map[string]any{"id": 77, "default_branch": "main"})
	case key == "GET "+labProject+"/merge_requests":
		var out []map[string]any
		for _, m := range l.mrs {
			if r.URL.Query().Get("state") == "opened" && m["state"] != "opened" {
				continue
			}
			out = append(out, m)
		}
		enc.Encode(out)
	case key == "POST "+labProject+"/merge_requests":
		enc.Encode(map[string]any{"iid": 3})
	case strings.HasPrefix(key, "PUT "+labProject+"/merge_requests/"):
		enc.Encode(map[string]any{"iid": 3, "draft": strings.HasPrefix(in["title"].(string), "Draft:")})
	case key == "GET "+labProject+"/merge_requests/3":
		enc.Encode(l.mrs[0])
	case key == "GET "+labProject+"/repository/commits/"+headSHA+"/statuses":
		enc.Encode(l.statuses)
	case key == "GET "+labProject+"/repository/commits/"+headSHA:
		enc.Encode(map[string]any{"committed_date": l.headAt})
	case key == "GET "+labProject+"/merge_requests/3/notes":
		enc.Encode(l.notes)
	case key == "POST "+labProject+"/merge_requests/3/notes":
		l.notes = append(l.notes, map[string]any{"id": 99, "body": in["body"], "created_at": "2026-11-24T11:30:00Z",
			"author": map[string]any{"username": "project_77_bot"}})
		enc.Encode(map[string]any{"id": 99})
	case key == "DELETE "+labProject+"/repository/branches/maestro%2FGH-7":
		w.WriteHeader(204)
	case key == "POST "+labProject+"/uploads":
		enc.Encode(map[string]any{"url": "/uploads/abc/GH-7.png", "full_path": "/-/project/77/uploads/abc/GH-7.png"})
	default:
		w.WriteHeader(404)
	}
}

func mr(iid int, state, title, author string, source int) map[string]any {
	return map[string]any{"iid": iid, "state": state, "title": title, "draft": strings.HasPrefix(title, "Draft:"),
		"sha": headSHA, "source_branch": "maestro/GH-7", "target_branch": "main",
		"source_project_id": source, "target_project_id": 77, "author": map[string]any{"username": author}}
}

func fakeLabPublisher(t *testing.T, l *fakeLab) *Publisher {
	srv := httptest.NewServer(http.HandlerFunc(l.serve))
	t.Cleanup(srv.Close)
	cfg := &config.Config{Workspace: config.Workspace{Repo: "group/sub/app", Host: "gitlab", HostURL: srv.URL, BaseBranch: "main"},
		Publish: config.Publish{BranchPrefix: "maestro/", ReworkOnChangesRequested: true, ReworkOnComments: true},
		Tracker: config.Tracker{AllowedCommenters: []string{"ian"}}}
	return &Publisher{Config: func() *config.Config { return cfg }, BotLogin: "project_77_bot",
		Token: func(context.Context) (string, error) { return token, nil }}
}

func TestGitLabMergeRequest(t *testing.T) {
	l := &fakeLab{}
	p := fakeLabPublisher(t, l)
	h, err := p.host()
	if err != nil {
		t.Fatal(err)
	}
	if got := h.gitURL(); !strings.HasSuffix(got, "/group/sub/app.git") {
		t.Errorf("git URL %q", got)
	}
	n, err := h.upsert(ctx, "maestro/GH-7", "main", "GH-7: Fix it", "body", true)
	if err != nil || n != 3 {
		t.Fatalf("create: %d %v", n, err)
	}
	if got := l.bodies["POST "+labProject+"/merge_requests"]; got["title"] != "Draft: GH-7: Fix it" || got["target_branch"] != "main" {
		t.Errorf("created with %v", got)
	}

	// A fork's merge request with the same branch is not ours to update.
	l.mrs = []map[string]any{mr(8, "opened", "x", "mallory", 12), mr(3, "opened", "Draft: GH-7: Fix it", "project_77_bot", 77)}
	if n, err := h.upsert(ctx, "maestro/GH-7", "main", "GH-7: Fix it again", "body", true); err != nil || n != 3 {
		t.Fatalf("update: %d %v", n, err)
	}
	if got := l.bodies["PUT "+labProject+"/merge_requests/3"]; got["title"] != "Draft: GH-7: Fix it again" {
		t.Errorf("an update lost the draft: %v", got)
	}
	l.mrs[1]["target_branch"] = "release"
	if _, err := h.upsert(ctx, "maestro/GH-7", "main", "t", "b", true); err == nil {
		t.Error("a merge request to another base was updated")
	}
	l.mrs[1]["target_branch"] = "main"

	list, err := h.changes(ctx, "maestro/GH-7")
	if err != nil || len(list) != 1 || list[0].Number != 3 || list[0].State != changeOpen || !list[0].Draft {
		t.Errorf("changes: %+v %v", list, err)
	}
	for state, want := range map[string]string{"merged": changeMerged, "closed": changeClosed, "locked": changeClosed} {
		if got := (mergeRequest{State: state}).change().State; got != want {
			t.Errorf("%s: %s", state, got)
		}
	}

	if err := h.deleteBranch(ctx, "maestro/GH-7"); err != nil {
		t.Errorf("delete: %v", err)
	}
	l.status = map[string]int{"DELETE " + labProject + "/repository/branches/maestro%2FGH-7": 404}
	if err := h.deleteBranch(ctx, "maestro/GH-7"); err != nil {
		t.Errorf("a branch that is gone: %v", err)
	}

	links, err := h.storeShots(ctx, "GH-7", headSHA, []worker.Screenshot{{Caption: "home", PNG: []byte("png")}})
	if err != nil || len(links) != 1 || !strings.HasSuffix(links[0].url, "/-/project/77/uploads/abc/GH-7.png") {
		t.Errorf("pictures: %+v %v", links, err)
	}
}

func TestGitLabChecksAndReady(t *testing.T) {
	l := &fakeLab{mrs: []map[string]any{mr(3, "opened", "Draft: GH-7: Fix it", "project_77_bot", 77)}}
	p := fakeLabPublisher(t, l)
	rec := orchestrator.PublishRecord{PRNumber: 3, HeadSHA: headSHA}
	l.statuses = []map[string]any{{"name": "test", "status": "running"}}
	if st, err := p.Check(ctx, rec); err != nil || st.State != orchestrator.ChecksPending {
		t.Fatalf("running: %+v %v", st, err)
	}
	l.statuses = []map[string]any{{"name": "test", "status": "success"}, {"name": "lint", "status": "failed", "allow_failure": true}}
	if st, _ := p.Check(ctx, rec); st.State != orchestrator.ChecksPassed {
		t.Errorf("passed, with a failure that is allowed: %+v", st)
	}
	l.statuses = []map[string]any{{"name": "test", "status": "failed", "description": "3 tests failed"}}
	if st, _ := p.Check(ctx, rec); st.State != orchestrator.ChecksFailed || !strings.Contains(st.Feedback[0].Body, "3 tests failed") {
		t.Errorf("failed: %+v", st)
	}
	if err := p.MarkReady(ctx, rec); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if got := l.bodies["PUT "+labProject+"/merge_requests/3"]; got["title"] != "GH-7: Fix it" {
		t.Errorf("ready sent %v", got)
	}
}

func TestGitLabReviewAndComments(t *testing.T) {
	l := &fakeLab{mrs: []map[string]any{mr(3, "opened", "GH-7: Fix it", "project_77_bot", 77)}, headAt: "2026-11-24T10:00:00Z"}
	p := fakeLabPublisher(t, l)
	p.Now = func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) }
	p.Config().Publish.ReworkOnComments = false
	sys := func(body, at, who string) map[string]any {
		return map[string]any{"id": 1, "body": body, "created_at": at, "system": true, "author": map[string]any{"username": who}}
	}
	// Changes requested before the head commit were answered by it.
	l.notes = []map[string]any{sys("requested changes", "2026-11-24T09:00:00Z", "ian")}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("an old verdict counted: %v", st.State)
	}
	l.notes = append(l.notes, sys("requested changes", "2026-11-24T10:05:00Z", "mallory"))
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("a stranger's verdict counted: %v", st.State)
	}
	l.notes = append(l.notes, sys("requested changes", "2026-11-24T10:06:00Z", "ian"))
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewChangesRequested {
		t.Errorf("changes requested: %v", st.State)
	}
	l.notes = append(l.notes, sys("approved this merge request", "2026-11-24T10:07:00Z", "ian"))
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("approved after: %v", st.State)
	}

	// Comments, on a line too, come back as feedback and are marked once.
	p.Config().Publish.ReworkOnComments = true
	l.notes = []map[string]any{
		{"id": 2, "body": "rename this", "created_at": "2026-11-24T11:00:00Z", "author": map[string]any{"username": "ian"},
			"type": "DiffNote", "position": map[string]any{"new_path": "main.go", "new_line": 12}},
		{"id": 3, "body": "ignore me", "created_at": "2026-11-24T11:00:00Z", "author": map[string]any{"username": "mallory"}},
	}
	st, err := p.Review(ctx, ticket)
	if err != nil || st.State != orchestrator.ReviewFeedback || st.FeedbackCount != 1 {
		t.Fatalf("comment: %+v %v", st, err)
	}
	fb, err := p.Feedback(ctx, ticket)
	if err != nil || len(fb) != 1 || fb[0].Path != "main.go" || fb[0].Line != 12 || fb[0].Body != "rename this" {
		t.Fatalf("feedback: %+v %v", fb, err)
	}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("after the mark: %v", st.State)
	}
}

func TestGitLabPaths(t *testing.T) {
	for path, ok := range map[string]bool{"group/app": true, "group/sub/app": true, "a/b.c_d-e": true,
		"app": false, "group/app.git": false, "group/../app": false, "-x/app": false, "group//app": false, "group/app/": false} {
		if ValidGitLabPath(path) != ok {
			t.Errorf("%s: want %v", path, ok)
		}
	}
}
