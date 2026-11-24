package publish

import (
	"context"
	"encoding/json"
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

// fakeTea is a stand-in for Gitea's API, for the repository team/app.
type fakeTea struct {
	mu       sync.Mutex
	bodies   map[string]map[string]any
	pulls    []map[string]any
	statuses []map[string]any
	reviews  []map[string]any
	inline   map[string][]map[string]any
	talk     []map[string]any
	status   map[string]int
	// pageSize is the instance's largest page.
	pageSize int
}

const teaRepo = "/api/v1/repos/team/app"

// page gives one page of a list, as the instance pages it.
func (f *fakeTea) page(r *http.Request, all []map[string]any) []map[string]any {
	size := f.pageSize
	if size == 0 {
		size = 50
	}
	n := 1
	if p := r.URL.Query().Get("page"); p != "" {
		json.Unmarshal([]byte(p), &n)
	}
	from := (n - 1) * size
	if from >= len(all) {
		return []map[string]any{}
	}
	to := min(from+size, len(all))
	return all[from:to]
}

func (f *fakeTea) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.EscapedPath()
	if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-GitHub-Api-Version") != "" {
		w.WriteHeader(401)
		return
	}
	if code := f.status[key]; code != 0 {
		w.WriteHeader(code)
		return
	}
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	if f.bodies == nil {
		f.bodies = map[string]map[string]any{}
	}
	f.bodies[key] = in
	enc := json.NewEncoder(w)
	switch {
	case key == "GET "+teaRepo:
		enc.Encode(map[string]any{"id": 5, "default_branch": "main"})
	case key == "GET "+teaRepo+"/pulls":
		var out []map[string]any
		for _, p := range f.pulls {
			if r.URL.Query().Get("state") == "open" && p["state"] != "open" {
				continue
			}
			out = append(out, p)
		}
		enc.Encode(f.page(r, out))
	case key == "POST "+teaRepo+"/pulls":
		enc.Encode(map[string]any{"number": 3})
	case key == "PATCH "+teaRepo+"/pulls/3":
		enc.Encode(map[string]any{"number": 3, "title": in["title"], "state": "open"})
	case key == "GET "+teaRepo+"/pulls/3":
		enc.Encode(f.pulls[len(f.pulls)-1])
	case key == "GET "+teaRepo+"/commits/"+headSHA+"/status":
		enc.Encode(map[string]any{"statuses": f.page(r, f.statuses)})
	case key == "GET "+teaRepo+"/git/commits/"+headSHA:
		enc.Encode(map[string]any{"commit": map[string]any{"committer": map[string]any{"date": "2026-11-24T10:00:00Z"}}})
	case key == "GET "+teaRepo+"/pulls/3/reviews":
		enc.Encode(f.page(r, f.reviews))
	case strings.HasPrefix(key, "GET "+teaRepo+"/pulls/3/reviews/"):
		enc.Encode(f.inline[strings.TrimSuffix(strings.TrimPrefix(key, "GET "+teaRepo+"/pulls/3/reviews/"), "/comments")])
	case key == "GET "+teaRepo+"/issues/3/comments":
		enc.Encode(f.page(r, f.talk))
	case key == "POST "+teaRepo+"/issues/3/comments":
		f.talk = append(f.talk, map[string]any{"body": in["body"], "created_at": "2026-11-24T11:30:00+01:00",
			"user": map[string]any{"login": "maestro-bot"}})
		enc.Encode(map[string]any{"id": 99})
	case key == "DELETE "+teaRepo+"/branches/maestro%2FGH-7":
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

func teaPull(n int, state, title, author, from string) map[string]any {
	return map[string]any{"number": n, "state": state, "title": title, "user": map[string]any{"login": author},
		"head": map[string]any{"ref": "maestro/GH-7", "sha": headSHA, "repo": map[string]any{"full_name": from}},
		"base": map[string]any{"ref": "main"}}
}

func fakeTeaPublisher(t *testing.T, f *fakeTea) *Publisher {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	cfg := &config.Config{Workspace: config.Workspace{Repo: "team/app", Host: "gitea", HostURL: srv.URL, BaseBranch: "main"},
		Publish: config.Publish{BranchPrefix: "maestro/", ReworkOnChangesRequested: true, ReworkOnComments: true},
		Tracker: config.Tracker{AllowedCommenters: []string{"ian"}}}
	return &Publisher{Config: func() *config.Config { return cfg }, BotLogin: "maestro-bot",
		Now:   func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) },
		Token: func(context.Context) (string, error) { return token, nil }}
}

func TestGiteaPullRequest(t *testing.T) {
	f := &fakeTea{}
	p := fakeTeaPublisher(t, f)
	h, err := p.host()
	if err != nil {
		t.Fatal(err)
	}
	if got := h.gitURL(); !strings.HasSuffix(got, "/team/app.git") {
		t.Errorf("git URL %q", got)
	}
	n, err := h.upsert(ctx, "maestro/GH-7", "main", "GH-7: Fix it", "body", true)
	if err != nil || n != 3 {
		t.Fatalf("create: %d %v", n, err)
	}
	if got := f.bodies["POST "+teaRepo+"/pulls"]; got["title"] != "WIP: GH-7: Fix it" || got["base"] != "main" || got["head"] != "maestro/GH-7" {
		t.Errorf("created with %v", got)
	}

	// A fork's pull request with the same branch is not ours to update,
	// and one on a later page is found.
	for i := 0; i < 3; i++ {
		f.pulls = append(f.pulls, teaPull(20+i, "open", "other", "ian", "team/app"))
		f.pulls[i]["head"].(map[string]any)["ref"] = "other"
	}
	f.pulls = append(f.pulls, teaPull(8, "open", "x", "mallory", "mallory/app"), teaPull(3, "open", "WIP: GH-7: Fix it", "maestro-bot", "team/app"))
	f.pageSize = 2
	if n, err := h.upsert(ctx, "maestro/GH-7", "main", "GH-7: Fix it again", "body", true); err != nil || n != 3 {
		t.Fatalf("update: %d %v", n, err)
	}
	if got := f.bodies["PATCH "+teaRepo+"/pulls/3"]; got["title"] != "WIP: GH-7: Fix it again" {
		t.Errorf("an update lost the draft: %v", got)
	}
	last := f.pulls[len(f.pulls)-1]
	last["base"] = map[string]any{"ref": "release"}
	if _, err := h.upsert(ctx, "maestro/GH-7", "main", "t", "b", true); err == nil {
		t.Error("a pull request to another base was updated")
	}
	last["base"] = map[string]any{"ref": "main"}

	list, err := h.changes(ctx, "maestro/GH-7")
	if err != nil || len(list) != 1 || list[0].Number != 3 || list[0].State != changeOpen || !list[0].Draft {
		t.Errorf("changes: %+v %v", list, err)
	}
	if got := (giteaPull{State: "closed", Merged: true}).change().State; got != changeMerged {
		t.Errorf("merged: %s", got)
	}

	if err := h.deleteBranch(ctx, "maestro/GH-7"); err != nil {
		t.Errorf("delete: %v", err)
	}
	f.status = map[string]int{"DELETE " + teaRepo + "/branches/maestro%2FGH-7": 404}
	if err := h.deleteBranch(ctx, "maestro/GH-7"); err != nil {
		t.Errorf("a branch that is gone: %v", err)
	}
	if _, err := h.storeShots(ctx, "GH-7", headSHA, []worker.Screenshot{{Caption: "home", PNG: []byte("png")}}); err == nil {
		t.Error("pictures are not stored on Gitea yet")
	}
}

func TestGiteaChecksAndReady(t *testing.T) {
	f := &fakeTea{pulls: []map[string]any{teaPull(3, "open", "WIP: GH-7: Fix it", "maestro-bot", "team/app")}}
	p := fakeTeaPublisher(t, f)
	rec := orchestrator.PublishRecord{PRNumber: 3, HeadSHA: headSHA}
	f.statuses = []map[string]any{{"context": "ci / test", "status": "pending"}}
	if st, err := p.Check(ctx, rec); err != nil || st.State != orchestrator.ChecksPending {
		t.Fatalf("pending: %+v %v", st, err)
	}
	f.statuses = []map[string]any{{"context": "ci / test", "status": "success"}, {"context": "ci / lint", "status": "success"}}
	if st, _ := p.Check(ctx, rec); st.State != orchestrator.ChecksPassed {
		t.Errorf("passed: %+v", st)
	}
	f.pageSize = 1
	f.statuses = append(f.statuses, map[string]any{"context": "ci / build", "status": "failure", "description": "build broke"})
	if st, _ := p.Check(ctx, rec); st.State != orchestrator.ChecksFailed || !strings.Contains(st.Feedback[0].Body, "build broke") {
		t.Errorf("a failure on a later page: %+v", st)
	}
	if err := p.MarkReady(ctx, rec); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if got := f.bodies["PATCH "+teaRepo+"/pulls/3"]; got["title"] != "GH-7: Fix it" {
		t.Errorf("ready sent %v", got)
	}
}

func TestGiteaReviewAndComments(t *testing.T) {
	f := &fakeTea{pulls: []map[string]any{teaPull(3, "open", "GH-7: Fix it", "maestro-bot", "team/app")}}
	p := fakeTeaPublisher(t, f)
	p.Config().Publish.ReworkOnComments = false
	rv := func(id int, state, who, sha string) map[string]any {
		return map[string]any{"id": id, "state": state, "commit_id": sha, "submitted_at": "2026-11-24T10:05:00+01:00",
			"user": map[string]any{"login": who}}
	}
	f.reviews = []map[string]any{rv(1, "REQUEST_CHANGES", "ian", "2222222222222222222222222222222222222222")}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("a verdict on an old head counted: %v", st.State)
	}
	f.reviews = append(f.reviews, rv(2, "REQUEST_CHANGES", "mallory", headSHA))
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("a stranger's verdict counted: %v", st.State)
	}
	dismissed := rv(3, "REQUEST_CHANGES", "ian", headSHA)
	dismissed["dismissed"] = true
	f.reviews = append(f.reviews, dismissed)
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("a dismissed review counted: %v", st.State)
	}
	f.reviews = append(f.reviews, rv(4, "REQUEST_CHANGES", "ian", headSHA))
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewChangesRequested {
		t.Errorf("changes requested: %v", st.State)
	}
	f.reviews = append(f.reviews, rv(5, "APPROVED", "ian", headSHA))
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Errorf("approved after: %v", st.State)
	}

	// Comments, on a line too, come back as feedback and are marked once.
	p.Config().Publish.ReworkOnComments = true
	f.reviews = []map[string]any{rv(6, "COMMENT", "ian", headSHA)}
	f.inline = map[string][]map[string]any{"6": {{"body": "rename this", "path": "main.go", "position": 12,
		"created_at": "2026-11-24T10:00:00+01:00", "user": map[string]any{"login": "ian"}}}}
	f.talk = []map[string]any{{"body": "ignore me", "created_at": "2026-11-24T11:00:00Z", "user": map[string]any{"login": "mallory"}}}
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

func TestGiteaRepos(t *testing.T) {
	for path, ok := range map[string]bool{"team/app": true, "a/b.c_d-e": true,
		"app": false, "team/app.git": false, "team/../app": false, "team/sub/app": false, "-x/app": false} {
		if ValidGiteaRepo(path) != ok {
			t.Errorf("%s: want %v", path, ok)
		}
	}
}
