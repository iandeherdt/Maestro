package publish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/mirror"
	"github.com/iandeherdt/Symphony-beads/internal/orchestrator"
	"github.com/iandeherdt/Symphony-beads/internal/worker"
	"github.com/iandeherdt/Symphony-beads/internal/workspace"
)

var ctx = context.Background()

const token = "ghs_publish_test_value"

// hub is a stand-in for the pull request endpoints.
type hub struct {
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	pulls    []map[string]any
	status   map[string]int
	srv      *httptest.Server
	badAuth  bool
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	h.requests = append(h.requests, key)
	if r.Header.Get("Authorization") != "Bearer "+token {
		h.badAuth = true
		w.WriteHeader(401)
		return
	}
	if code := h.status[key]; code != 0 {
		w.WriteHeader(code)
		io.WriteString(w, `{"message":"server detail"}`)
		return
	}
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	h.bodies = append(h.bodies, in)
	switch {
	case key == "GET /repos/ian/app":
		json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	case key == "GET /repos/ian/app/pulls":
		if r.URL.Query().Get("head") != "ian:symphony/GH-7-abc" || r.URL.Query().Get("state") != "open" {
			w.WriteHeader(422)
			return
		}
		json.NewEncoder(w).Encode(h.pulls)
	case key == "POST /repos/ian/app/pulls":
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"number": 31})
	case key == "PATCH /repos/ian/app/pulls/31":
		json.NewEncoder(w).Encode(map[string]any{"number": 31})
	default:
		w.WriteHeader(404)
	}
}

func (h *hub) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.requests {
		if r == key {
			n++
		}
	}
	return n
}

type lab struct {
	t      *testing.T
	root   string
	work   string
	remote string
	base   string
	hub    *hub
	cfg    *config.Config
	pub    *Publisher
	ws     workspace.Workspace
	is     issue.Issue
}

func (l *lab) git(dir string, args ...string) string {
	l.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+l.root,
		"GIT_AUTHOR_NAME=bot", "GIT_AUTHOR_EMAIL=bot@example.com", "GIT_COMMITTER_NAME=bot", "GIT_COMMITTER_EMAIL=bot@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		l.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// change commits a file in the workspace and writes the hand-off bundle.
func (l *lab) change(path, content string) string {
	l.t.Helper()
	full := filepath.Join(l.work, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		l.t.Fatal(err)
	}
	l.git(l.work, "add", "-A")
	l.git(l.work, "commit", "-q", "-m", "change "+path)
	bundle := filepath.Join(l.root, "handoff", l.ws.Key+".bundle")
	os.Remove(bundle)
	l.git(l.work, "bundle", "create", "-q", bundle, l.base+"..HEAD")
	return l.git(l.work, "rev-parse", "HEAD")
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	l := &lab{t: t, root: root, work: filepath.Join(root, "work")}
	seed := filepath.Join(root, "seed")
	os.MkdirAll(seed, 0o755)
	os.MkdirAll(filepath.Join(root, "handoff"), 0o755)
	l.git(seed, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644)
	l.git(seed, "add", "-A")
	l.git(seed, "commit", "-q", "-m", "base")
	l.base = l.git(seed, "rev-parse", "HEAD")
	l.remote = filepath.Join(root, "remotes", "ian", "app.git")
	os.MkdirAll(filepath.Dir(l.remote), 0o755)
	l.git(root, "clone", "-q", "--bare", seed, l.remote)
	bare := filepath.Join(root, "mirror.git")
	l.git(root, "clone", "-q", "--bare", seed, bare)
	l.git(root, "clone", "-q", seed, l.work)

	l.hub = &hub{status: map[string]int{}}
	l.hub.srv = httptest.NewServer(http.HandlerFunc(l.hub.serve))
	t.Cleanup(l.hub.srv.Close)

	l.cfg = &config.Config{
		Tracker:   config.Tracker{Kind: "github", Provider: map[string]any{"repo": "ian/app"}},
		Workspace: config.Workspace{Repo: "ian/app", BaseBranch: "main"},
		Publish:   config.Publish{BranchPrefix: "symphony/", DraftUntilGreen: true, ChecksTimeoutMS: 600000},
		Policy:    config.Policy{MaxChangedFiles: 100, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20, SecretScan: true},
		Verify:    config.Verify{Command: "npm test"},
		Worker:    config.Worker{Name: "linux"},
	}
	l.ws = workspace.Workspace{Key: "GH-7-abc", Branch: "symphony/GH-7-abc", BaseSHA: l.base}
	l.is = issue.Issue{ID: "I_7", Identifier: "GH-7", Title: "Fix the login page"}
	l.pub = &Publisher{
		Config:     func() *config.Config { return l.cfg },
		Mirror:     &mirror.Mirror{Dir: bare, OwnerUID: -1, MaxBundleBytes: 1 << 20, AllowFileRemote: true},
		HandoffDir: filepath.Join(root, "handoff"),
		Token:      func(context.Context) (string, error) { return token, nil },
		BotName:    "bot", BotEmail: "bot@example.com",
		APIURL: l.hub.srv.URL, GitURL: filepath.Join(root, "remotes"),
		Now: func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) },
	}
	return l
}

func (l *lab) remoteHead() string {
	out, err := exec.Command("git", "--git-dir="+l.remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+l.ws.Branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var passed = worker.VerifyResult{Passed: true}

func TestPublishOpensAPullRequest(t *testing.T) {
	l := newLab(t)
	l.change("src/login.go", "package src\n")
	head := l.change("package.json", "{}\n")
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rej != nil {
		t.Fatalf("rejection %+v, error %v", rej, err)
	}
	if rec.PRNumber != 31 || rec.HeadSHA != head || rec.Branch != "symphony/GH-7-abc" || rec.IssueID != "I_7" ||
		rec.Identifier != "GH-7" || !rec.ChecksDeadline.Equal(time.Date(2026, 11, 21, 12, 10, 0, 0, time.UTC)) {
		t.Errorf("record %+v", rec)
	}
	if len(rec.Flags) != 2 || !strings.HasPrefix(rec.Flags[0], "F3 package.json") {
		t.Errorf("flags %q", rec.Flags)
	}
	if l.remoteHead() != head {
		t.Errorf("remote branch at %q", l.remoteHead())
	}
	if l.hub.count("POST /repos/ian/app/pulls") != 1 || l.hub.count("GET /repos/ian/app") != 0 || l.hub.badAuth {
		t.Errorf("requests %v", l.hub.requests)
	}
	created := l.hub.bodies[len(l.hub.bodies)-1]
	if created["title"] != "GH-7: Fix the login page" || created["head"] != "symphony/GH-7-abc" || created["base"] != "main" || created["draft"] != true {
		t.Errorf("created with %v", created)
	}
	text := created["body"].(string)
	for _, want := range []string{"Closes #7\n", "**F3** dependency manifest or lockfile changed: `package.json`",
		"Verify command exited with status 0", "npm test", "2 commit(s), 2 file(s) changed", "Worker: `linux`", footer} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %q:\n%s", want, text)
		}
	}
}

func TestSecondPublishUpdatesTheSamePullRequest(t *testing.T) {
	l := newLab(t)
	l.change("a.go", "package a\n")
	if _, _, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil {
		t.Fatal(err)
	}
	l.hub.pulls = []map[string]any{
		{"number": 99, "head": map[string]any{"ref": "other"}, "base": map[string]any{"ref": "main"}},
		{"number": 31, "head": map[string]any{"ref": "symphony/GH-7-abc"}, "base": map[string]any{"ref": "main"}},
	}
	head := l.change("b.go", "package a\n")
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rej != nil || rec.PRNumber != 31 || rec.HeadSHA != head {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
	if l.hub.count("POST /repos/ian/app/pulls") != 1 || l.hub.count("PATCH /repos/ian/app/pulls/31") != 1 {
		t.Errorf("requests %v", l.hub.requests)
	}
	if l.remoteHead() != head {
		t.Error("the branch was not updated")
	}
}

func TestPolicyDenialPublishesNothing(t *testing.T) {
	l := newLab(t)
	l.change(".github/workflows/ci.yml", "on: push\n")
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rec != nil || rej == nil {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
	if rej.Category != PolicyDenied || len(rej.Feedback) != 1 || rej.Feedback[0].Source != "policy" ||
		!strings.Contains(rej.Feedback[0].Body, "D1 .github/workflows/ci.yml") {
		t.Errorf("rejection %+v", rej)
	}
	if l.remoteHead() != "" || len(l.hub.requests) != 0 {
		t.Errorf("something was published: branch %q, requests %v", l.remoteHead(), l.hub.requests)
	}
}

func TestHeldSecretInTheDiffIsDenied(t *testing.T) {
	l := newLab(t)
	l.pub.HeldSecrets = func() []string { return []string{token} }
	l.change("notes.md", "the value is "+token+"\n")
	_, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rej == nil || rej.Category != PolicyDenied {
		t.Fatalf("%+v %v", rej, err)
	}
	if strings.Contains(rej.Feedback[0].Body, token) {
		t.Error("the feedback repeats the secret")
	}
	if l.remoteHead() != "" {
		t.Error("pushed")
	}
}

func TestImportRejection(t *testing.T) {
	l := newLab(t)
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed) // no bundle was written
	if err != nil || rec != nil || rej == nil || rej.Category != ImportRejected || !strings.Contains(rej.Feedback[0].Body, "no bundle") {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
}

func TestBranchMustBeUnderThePrefix(t *testing.T) {
	for _, branch := range []string{"main", "symphony/", "feature/x", "symphony/../main", "symphonyx"} {
		l := newLab(t)
		l.change("a.go", "package a\n")
		l.ws.Branch = branch
		rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
		if err == nil || rec != nil || rej != nil {
			t.Errorf("branch %q: %+v %+v %v", branch, rec, rej, err)
		}
		if len(l.hub.requests) != 0 {
			t.Errorf("branch %q: requests %v", branch, l.hub.requests)
		}
		out, _ := exec.Command("git", "--git-dir="+l.remote, "for-each-ref", "--format=%(refname)").Output()
		if strings.TrimSpace(string(out)) != "refs/heads/main" {
			t.Errorf("branch %q: remote refs changed: %s", branch, out)
		}
	}
}

func TestDefaultBaseBranchIsLookedUp(t *testing.T) {
	l := newLab(t)
	l.cfg.Workspace.BaseBranch = ""
	l.change("a.go", "package a\n")
	if _, _, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil {
		t.Fatal(err)
	}
	if l.hub.count("GET /repos/ian/app") != 1 || l.hub.bodies[len(l.hub.bodies)-1]["base"] != "main" {
		t.Errorf("requests %v", l.hub.requests)
	}
}

func TestExistingPullRequestWithAnotherBaseIsAnError(t *testing.T) {
	l := newLab(t)
	l.hub.pulls = []map[string]any{{"number": 31, "head": map[string]any{"ref": "symphony/GH-7-abc"}, "base": map[string]any{"ref": "release"}}}
	l.change("a.go", "package a\n")
	rec, _, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err == nil || rec != nil || l.hub.count("PATCH /repos/ian/app/pulls/31") != 0 {
		t.Fatalf("%+v %v", rec, err)
	}
}

func TestAPIFailureIsAnErrorWithoutServerText(t *testing.T) {
	l := newLab(t)
	l.hub.status["POST /repos/ian/app/pulls"] = 422
	l.change("a.go", "package a\n")
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err == nil || rec != nil || rej != nil {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
	if strings.Contains(err.Error(), "server detail") || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "422") {
		t.Errorf("error %q", err)
	}
}

func TestTokenIsNotSentAcrossARedirect(t *testing.T) {
	l := newLab(t)
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("Authorization") != ""
		io.WriteString(w, "[]")
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirect.Close()
	l.pub.APIURL = redirect.URL
	l.pub.HTTP = &http.Client{}
	l.change("a.go", "package a\n")
	if _, _, err := l.pub.Publish(ctx, l.is, l.ws, passed); err == nil || leaked {
		t.Fatalf("error %v, leaked %v", err, leaked)
	}
}

func TestTitleAndBodyDefuseUntrustedText(t *testing.T) {
	got := title("GH-7", "Fix @everyone \u202Ethe #12 thing\n"+strings.Repeat("x", 300))
	if strings.Contains(got, "@everyone") || strings.Contains(got, "#12") || strings.ContainsAny(got, "\n\u202E") {
		t.Errorf("title %q", got)
	}
	if n := len([]rune(strings.NewReplacer("\u200b", "").Replace(got))); n > 120 {
		t.Errorf("title of %d characters", n)
	}
	text := body("Closes #7", []flagLine{{"F7", "docs/`x` @team\n## Approved", "path listed"}},
		Proof{VerifyCommand: "make test\n```\n## Approved by @owner", VerifyStatus: 0, Worker: "linux"}, "All done @owner\n```\n# LGTM")
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "## Approved") || strings.HasPrefix(trim, "# LGTM") {
			// Allowed only inside a fence, where it is not a heading.
			if !insideFence(text, line) {
				t.Errorf("untrusted text became a heading: %q", line)
			}
		}
	}
	if strings.Contains(text, "@owner") || strings.Contains(text, "@team") {
		t.Errorf("a mention survived:\n%s", text)
	}
}

// insideFence reports whether the first occurrence of line sits between an
// opening and a closing fence.
func insideFence(text, line string) bool {
	open := ""
	for _, l := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(l)
		if open == "" {
			if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
				open = strings.TrimRight(trim, "abcdefghijklmnopqrstuvwxyz")
				continue
			}
		} else if trim == open {
			open = ""
			continue
		}
		if l == line {
			return open != ""
		}
	}
	return false
}

func TestIssueInAnotherRepositoryIsLinkedNotClosed(t *testing.T) {
	l := newLab(t)
	l.cfg.Tracker.Provider["repo"] = "ian/board"
	l.change("a.go", "package a\n")
	if _, _, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil {
		t.Fatal(err)
	}
	text := l.hub.bodies[len(l.hub.bodies)-1]["body"].(string)
	if strings.Contains(text, "Closes") || !strings.Contains(text, "Issue: ian/board#7") {
		t.Errorf("body:\n%s", text)
	}
}

// ci is a stand-in for the endpoints the checks gate reads.
type ci struct {
	mu       sync.Mutex
	pr       map[string]any
	runs     []map[string]any
	statuses []map[string]any
	graphql  []map[string]any
	gqlErr   bool
	srv      *httptest.Server
}

const headSHA = "1111111111111111111111111111111111111111"

func newCI(t *testing.T) (*ci, *Publisher) {
	c := &ci{pr: map[string]any{"state": "open", "merged": false, "draft": true, "node_id": "PR_node1",
		"head": map[string]any{"sha": headSHA}}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/ian/app/pulls/31":
			json.NewEncoder(w).Encode(c.pr)
		case "GET /repos/ian/app/commits/" + headSHA + "/check-runs":
			page := r.URL.Query().Get("page")
			start := 0
			if page == "2" {
				start = 100
			} else if page != "1" {
				start = len(c.runs)
			}
			end := min(start+100, len(c.runs))
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(c.runs), "check_runs": c.runs[min(start, len(c.runs)):end]})
		case "GET /repos/ian/app/commits/" + headSHA + "/status":
			json.NewEncoder(w).Encode(map[string]any{"statuses": c.statuses})
		case "POST /graphql":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			c.graphql = append(c.graphql, in)
			if c.gqlErr {
				io.WriteString(w, `{"errors":[{"type":"FORBIDDEN"}]}`)
				return
			}
			c.pr["draft"] = false
			io.WriteString(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(c.srv.Close)
	cfg := &config.Config{Workspace: config.Workspace{Repo: "ian/app"}}
	p := &Publisher{Config: func() *config.Config { return cfg }, APIURL: c.srv.URL,
		Token: func(context.Context) (string, error) { return token, nil }}
	return c, p
}

func run(name, status, conclusion string) map[string]any {
	return map[string]any{"name": name, "status": status, "conclusion": conclusion,
		"output": map[string]any{"title": name + " title", "summary": "summary of " + name}}
}

var record = orchestrator.PublishRecord{IssueID: "I_7", Identifier: "GH-7", PRNumber: 31, HeadSHA: headSHA}

func TestCheckStates(t *testing.T) {
	ok, bad, wait := run("test", "completed", "success"), run("lint", "completed", "failure"), run("build", "in_progress", "")
	for name, tc := range map[string]struct {
		runs     []map[string]any
		statuses []map[string]any
		required []string
		want     orchestrator.CheckState
	}{
		"nothing reported yet":         {nil, nil, nil, orchestrator.ChecksPending},
		"all green":                    {[]map[string]any{ok, run("skip", "completed", "skipped")}, nil, nil, orchestrator.ChecksPassed},
		"one running":                  {[]map[string]any{ok, wait}, nil, nil, orchestrator.ChecksPending},
		"one red":                      {[]map[string]any{ok, bad, wait}, nil, nil, orchestrator.ChecksFailed},
		"cancelled counts as failed":   {[]map[string]any{run("test", "completed", "cancelled")}, nil, nil, orchestrator.ChecksFailed},
		"required green, other red":    {[]map[string]any{ok, bad}, nil, []string{"test"}, orchestrator.ChecksPassed},
		"required missing":             {[]map[string]any{ok}, nil, []string{"test", "deploy-preview"}, orchestrator.ChecksPending},
		"required red":                 {[]map[string]any{ok, bad}, nil, []string{"lint"}, orchestrator.ChecksFailed},
		"status green":                 {nil, []map[string]any{{"context": "ci/legacy", "state": "success"}}, nil, orchestrator.ChecksPassed},
		"status pending":               {[]map[string]any{ok}, []map[string]any{{"context": "ci/legacy", "state": "pending"}}, nil, orchestrator.ChecksPending},
		"status red":                   {[]map[string]any{ok}, []map[string]any{{"context": "ci/legacy", "state": "error"}}, nil, orchestrator.ChecksFailed},
		"same name, worse result wins": {[]map[string]any{ok, run("test", "completed", "failure")}, nil, nil, orchestrator.ChecksFailed},
		"status worse than run":        {[]map[string]any{ok}, []map[string]any{{"context": "test", "state": "failure"}}, nil, orchestrator.ChecksFailed},
		"run worse than status":        {[]map[string]any{run("test", "completed", "failure")}, []map[string]any{{"context": "test", "state": "success"}}, nil, orchestrator.ChecksFailed},
	} {
		c, p := newCI(t)
		c.runs, c.statuses = tc.runs, tc.statuses
		p.Config().Publish.RequiredChecks = tc.required
		st, err := p.Check(ctx, record)
		if err != nil || st.State != tc.want {
			t.Errorf("%s: state %v, error %v", name, st.State, err)
		}
	}
}

func TestCheckFailureFeedback(t *testing.T) {
	c, p := newCI(t)
	hostile := run("lint @owner\n## Approved", "completed", "failure")
	hostile["output"] = map[string]any{"title": "2 problems", "summary": "line one\nline two " + strings.Repeat("x", 5000)}
	c.runs = []map[string]any{hostile}
	st, err := p.Check(ctx, record)
	if err != nil || st.State != orchestrator.ChecksFailed || len(st.Feedback) != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	fb := st.Feedback[0]
	if fb.Source != "ci" || !strings.Contains(fb.Body, "2 problems line one line two") || strings.Contains(fb.Body, "\n") || len(fb.Body) > 1800 {
		t.Errorf("feedback (%d bytes): %q", len(fb.Body), fb.Body[:min(len(fb.Body), 200)])
	}
}

func TestCheckSeesForeignPushAndClosedPullRequest(t *testing.T) {
	c, p := newCI(t)
	c.runs = []map[string]any{run("test", "completed", "success")}
	c.pr["head"] = map[string]any{"sha": strings.Repeat("2", 40)}
	if st, err := p.Check(ctx, record); err != nil || st.State != orchestrator.ChecksForeignPush {
		t.Errorf("foreign push: %+v %v", st, err)
	}
	if err := p.MarkReady(ctx, record); err == nil || len(c.graphql) != 0 {
		t.Errorf("a pull request with someone else's commits was marked ready: %v", err)
	}
	c.pr["head"] = map[string]any{"sha": headSHA}
	c.pr["state"] = "closed"
	if st, _ := p.Check(ctx, record); st.State != orchestrator.ChecksClosed {
		t.Errorf("closed: %+v", st)
	}
	c.pr["merged"] = true
	if st, _ := p.Check(ctx, record); st.State != orchestrator.ChecksClosed {
		t.Errorf("merged: %+v", st)
	}
}

func TestCheckWithManyRuns(t *testing.T) {
	c, p := newCI(t)
	for i := 0; i < 150; i++ {
		c.runs = append(c.runs, run("job-"+strings.Repeat("a", i%7)+string(rune('a'+i%26))+strings.Repeat("b", i/26), "completed", "success"))
	}
	c.runs[140] = run("late-failure", "completed", "failure")
	if st, err := p.Check(ctx, record); err != nil || st.State != orchestrator.ChecksFailed {
		t.Fatalf("a failure on the second page was missed: %+v %v", st, err)
	}
}

func TestCheckRejectsIncompleteRecordsAndReportsErrors(t *testing.T) {
	c, p := newCI(t)
	for _, rec := range []orchestrator.PublishRecord{{PRNumber: 0, HeadSHA: headSHA}, {PRNumber: 31, HeadSHA: "main"}} {
		if _, err := p.Check(ctx, rec); err == nil {
			t.Errorf("record %+v accepted", rec)
		}
	}
	c.srv.Close()
	if _, err := p.Check(ctx, record); err == nil {
		t.Error("an unreachable API was read as a result")
	}
}

func TestMarkReady(t *testing.T) {
	c, p := newCI(t)
	if err := p.MarkReady(ctx, record); err != nil {
		t.Fatal(err)
	}
	if len(c.graphql) != 1 {
		t.Fatalf("%d GraphQL calls", len(c.graphql))
	}
	vars, _ := c.graphql[0]["variables"].(map[string]any)
	query, _ := c.graphql[0]["query"].(string)
	if vars["id"] != "PR_node1" || !strings.Contains(query, "markPullRequestReadyForReview") || strings.Contains(query, "PR_node1") {
		t.Errorf("request %v", c.graphql[0])
	}
	// Already ready: nothing is sent.
	if err := p.MarkReady(ctx, record); err != nil || len(c.graphql) != 1 {
		t.Errorf("%v, %d calls", err, len(c.graphql))
	}
	c.pr["draft"] = true
	c.gqlErr = true
	if err := p.MarkReady(ctx, record); err == nil {
		t.Error("a GraphQL error was read as success")
	}
}

// gh is a stand-in for the endpoints the hand-off watch uses.
type gh struct {
	mu      sync.Mutex
	pulls   []map[string]any
	reviews []map[string]any
	queries []string
	deleted []string
	delCode int
	forgot  []string
}

func (g *gh) Import(context.Context, string, string, string) (mirror.Imported, error) {
	return mirror.Imported{}, nil
}
func (g *gh) Diff(context.Context, string, string, mirror.Limits) (mirror.Diff, error) {
	return mirror.Diff{}, nil
}
func (g *gh) Push(context.Context, string, string, string, string) error { return nil }
func (g *gh) ForgetPush(_ context.Context, key string) error {
	g.forgot = append(g.forgot, key)
	return nil
}

func pullOn(n int, state, merged, login, repo string) map[string]any {
	p := map[string]any{"number": n, "state": state, "user": map[string]any{"login": login},
		"head": map[string]any{"ref": "symphony/GH-7", "sha": headSHA, "repo": map[string]any{"full_name": repo}}}
	if merged != "" {
		p["merged_at"] = merged
	}
	return p
}

func review(state, commit, login string) map[string]any {
	return map[string]any{"state": state, "commit_id": commit, "user": map[string]any{"login": login}}
}

func newGH(t *testing.T) (*gh, *Publisher) {
	g := &gh{delCode: 204}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/ian/app/pulls":
			g.queries = append(g.queries, r.URL.RawQuery)
			json.NewEncoder(w).Encode(g.pulls)
		case "GET /repos/ian/app/pulls/31/reviews":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			start := min((page-1)*100, len(g.reviews))
			json.NewEncoder(w).Encode(g.reviews[start:min(start+100, len(g.reviews))])
		case "DELETE /repos/ian/app/git/refs/heads/symphony/GH-7":
			g.deleted = append(g.deleted, r.URL.Path)
			w.WriteHeader(g.delCode)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{Workspace: config.Workspace{Repo: "ian/app", BaseBranch: "main"},
		Publish: config.Publish{BranchPrefix: "symphony/", ReworkOnChangesRequested: true},
		Tracker: config.Tracker{AllowedCommenters: []string{"Ian"}}}
	p := &Publisher{Config: func() *config.Config { return cfg }, APIURL: srv.URL, Mirror: g, BotLogin: "sym[bot]",
		Token: func(context.Context) (string, error) { return token, nil }}
	return g, p
}

var ticket = issue.Issue{ID: "I_7", Identifier: "GH-7"}

func TestReviewStates(t *testing.T) {
	const bot, repo = "sym[bot]", "ian/app"
	old := "2222222222222222222222222222222222222222"
	for name, tc := range map[string]struct {
		pulls   []map[string]any
		reviews []map[string]any
		want    orchestrator.ReviewState
		number  int
	}{
		"no pull request":             {nil, nil, orchestrator.ReviewNone, 0},
		"open":                        {[]map[string]any{pullOn(31, "open", "", bot, repo)}, nil, orchestrator.ReviewOpen, 31},
		"merged":                      {[]map[string]any{pullOn(31, "closed", "2026-11-21T10:00:00Z", bot, repo)}, nil, orchestrator.ReviewMerged, 31},
		"closed":                      {[]map[string]any{pullOn(31, "closed", "", bot, repo)}, nil, orchestrator.ReviewClosed, 31},
		"newest counts":               {[]map[string]any{pullOn(31, "open", "", bot, repo), pullOn(12, "closed", "2026-11-01T10:00:00Z", bot, repo)}, nil, orchestrator.ReviewOpen, 31},
		"from a fork":                 {[]map[string]any{pullOn(40, "closed", "2026-11-21T10:00:00Z", bot, "mallory/app")}, nil, orchestrator.ReviewNone, 0},
		"opened by someone else":      {[]map[string]any{pullOn(40, "closed", "2026-11-21T10:00:00Z", "mallory", repo)}, nil, orchestrator.ReviewNone, 0},
		"skips to the service's own":  {[]map[string]any{pullOn(40, "closed", "2026-11-21T10:00:00Z", "mallory", repo), pullOn(31, "open", "", bot, repo)}, nil, orchestrator.ReviewOpen, 31},
		"changes requested":           {[]map[string]any{pullOn(31, "open", "", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", headSHA, "ian")}, orchestrator.ReviewChangesRequested, 31},
		"then approved":               {[]map[string]any{pullOn(31, "open", "", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", headSHA, "ian"), review("APPROVED", headSHA, "ian")}, orchestrator.ReviewOpen, 31},
		"then only a comment":         {[]map[string]any{pullOn(31, "open", "", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", headSHA, "ian"), review("COMMENTED", headSHA, "ian")}, orchestrator.ReviewChangesRequested, 31},
		"on an earlier commit":        {[]map[string]any{pullOn(31, "open", "", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", old, "ian")}, orchestrator.ReviewOpen, 31},
		"by a stranger":               {[]map[string]any{pullOn(31, "open", "", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", headSHA, "mallory")}, orchestrator.ReviewOpen, 31},
		"stranger after the reviewer": {[]map[string]any{pullOn(31, "open", "", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", headSHA, "ian"), review("APPROVED", headSHA, "mallory")}, orchestrator.ReviewChangesRequested, 31},
		"merged, with old reviews":    {[]map[string]any{pullOn(31, "closed", "2026-11-21T10:00:00Z", bot, repo)}, []map[string]any{review("CHANGES_REQUESTED", headSHA, "ian")}, orchestrator.ReviewMerged, 31},
	} {
		g, p := newGH(t)
		g.pulls, g.reviews = tc.pulls, tc.reviews
		st, err := p.Review(ctx, ticket)
		if err != nil || st.State != tc.want || st.PRNumber != tc.number {
			t.Errorf("%s: %+v, error %v", name, st, err)
		}
		if q := g.queries[0]; !strings.Contains(q, "head=ian%3Asymphony%2FGH-7") || !strings.Contains(q, "state=all") {
			t.Errorf("%s: query %s", name, q)
		}
	}
}

func TestReviewEdges(t *testing.T) {
	g, p := newGH(t)
	g.pulls = []map[string]any{pullOn(31, "open", "", "sym[bot]", "ian/app")}
	// The verdict is on the second page.
	for i := 0; i < 100; i++ {
		g.reviews = append(g.reviews, review("COMMENTED", headSHA, "ian"))
	}
	g.reviews = append(g.reviews, review("CHANGES_REQUESTED", headSHA, "ian"))
	if st, err := p.Review(ctx, ticket); err != nil || st.State != orchestrator.ReviewChangesRequested {
		t.Fatalf("second page: %+v, %v", st, err)
	}
	// A listed pull request on another branch is not the ticket's.
	g.pulls[0]["head"].(map[string]any)["ref"] = "symphony/GH-70"
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewNone {
		t.Fatalf("other branch: %+v", st)
	}
	g.pulls[0]["head"].(map[string]any)["ref"] = "symphony/GH-7"
	// Switched off: reviews are not read.
	p.Config().Publish.ReworkOnChangesRequested = false
	if st, err := p.Review(ctx, ticket); err != nil || st.State != orchestrator.ReviewOpen {
		t.Fatalf("switched off: %+v, %v", st, err)
	}
	// Without a bot login, any pull request from the repository's own branch counts.
	p.BotLogin = ""
	g.pulls = []map[string]any{pullOn(31, "closed", "2026-11-21T10:00:00Z", "ian", "ian/app")}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewMerged {
		t.Fatalf("no bot login: %+v", st)
	}
	// An identifier that cannot name a branch, and a missing prefix.
	if _, err := p.Review(ctx, issue.Issue{Identifier: "../x"}); err == nil {
		t.Fatal("a bad identifier was accepted")
	}
	p.Config().Publish.BranchPrefix = ""
	if _, err := p.Review(ctx, ticket); err == nil {
		t.Fatal("an empty prefix was accepted")
	}
	if err := p.DeleteBranch(ctx, ticket); err == nil || len(g.deleted) != 0 {
		t.Fatal("a branch outside the prefix was deleted")
	}
}

func TestDeleteBranch(t *testing.T) {
	for code, ok := range map[int]bool{204: true, 404: true, 422: true, 403: false, 500: false} {
		g, p := newGH(t)
		g.delCode = code
		err := p.DeleteBranch(ctx, ticket)
		if (err == nil) != ok || len(g.deleted) != 1 {
			t.Errorf("status %d: error %v, %d requests", code, err, len(g.deleted))
		}
		if forgot := len(g.forgot) == 1 && g.forgot[0] == "GH-7"; forgot != ok {
			t.Errorf("status %d: forgot %v", code, g.forgot)
		}
	}
}
