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

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
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
	// The branch with the screenshots: its tip, and how many times moving
	// it is refused first.
	shotTip string
	refuse  int
	// head is the branch pull requests are looked up for; empty means the
	// ticket's.
	head string
}

const (
	blobSHA   = "b10b000000000000000000000000000000000000"
	treeSHA   = "74ee000000000000000000000000000000000000"
	commitSHA = "c0aa000000000000000000000000000000000000"
	oldTip    = "01d0000000000000000000000000000000000000"
	oldTree   = "01d7000000000000000000000000000000000000"
)

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	h.requests = append(h.requests, key)
	if r.Header.Get("Authorization") != "Bearer "+token {
		h.bodies = append(h.bodies, nil)
		h.badAuth = true
		w.WriteHeader(401)
		return
	}
	if code := h.status[key]; code != 0 {
		h.bodies = append(h.bodies, nil)
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
		head := h.head
		if head == "" {
			head = "maestro/GH-7-abc"
		}
		if r.URL.Query().Get("head") != "ian:"+head || (r.URL.Query().Get("state") != "open" && r.URL.Query().Get("state") != "all") {
			w.WriteHeader(422)
			return
		}
		json.NewEncoder(w).Encode(h.pulls)
	case key == "POST /repos/ian/app/pulls":
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"number": 31})
	case key == "PATCH /repos/ian/app/pulls/31":
		json.NewEncoder(w).Encode(map[string]any{"number": 31})
	case key == "POST /repos/ian/app/git/blobs":
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"sha": blobSHA})
	case key == "GET /repos/ian/app/git/ref/heads/maestro/screenshots":
		if h.shotTip == "" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": h.shotTip}})
	case key == "GET /repos/ian/app/git/commits/"+oldTip:
		json.NewEncoder(w).Encode(map[string]any{"tree": map[string]any{"sha": oldTree}})
	case key == "POST /repos/ian/app/git/trees":
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"sha": treeSHA})
	case key == "POST /repos/ian/app/git/commits":
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"sha": commitSHA})
	case key == "POST /repos/ian/app/git/refs" || key == "PATCH /repos/ian/app/git/refs/heads/maestro/screenshots":
		if h.refuse > 0 {
			h.refuse--
			w.WriteHeader(422)
			return
		}
		h.shotTip = commitSHA
		json.NewEncoder(w).Encode(map[string]any{})
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
		Publish:   config.Publish{BranchPrefix: "maestro/", DraftUntilGreen: true, ChecksTimeoutMS: 600000},
		Policy:    config.Policy{MaxChangedFiles: 100, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20, SecretScan: true},
		Verify:    config.Verify{Command: "npm test"},
		Worker:    config.Worker{Name: "linux"},
	}
	l.ws = workspace.Workspace{Key: "GH-7-abc", Branch: "maestro/GH-7-abc", BaseSHA: l.base}
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
	if rec.PRNumber != 31 || rec.HeadSHA != head || rec.Branch != "maestro/GH-7-abc" || rec.IssueID != "I_7" ||
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
	if created["title"] != "GH-7: Fix the login page" || created["head"] != "maestro/GH-7-abc" || created["base"] != "main" || created["draft"] != true {
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
		{"number": 31, "head": map[string]any{"ref": "maestro/GH-7-abc"}, "base": map[string]any{"ref": "main"}},
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
	for _, branch := range []string{"main", "maestro/", "feature/x", "maestro/../main", "maestrox"} {
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
	l.hub.pulls = []map[string]any{{"number": 31, "head": map[string]any{"ref": "maestro/GH-7-abc"}, "base": map[string]any{"ref": "release"}}}
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
		if none := len(tc.runs)+len(tc.statuses) == 0; st.NoneReported != none {
			t.Errorf("%s: NoneReported %v", name, st.NoneReported)
		}
	}
}

func TestCheckSeesAConflict(t *testing.T) {
	c, p := newCI(t)
	c.pr["mergeable_state"] = "dirty"
	if st, err := p.Check(ctx, record); err != nil || st.State != orchestrator.ChecksConflict {
		t.Errorf("conflicted: %+v %v", st, err)
	}
	c.pr["mergeable_state"] = "unknown" // not computed yet: keep waiting
	if st, _ := p.Check(ctx, record); st.State != orchestrator.ChecksPending {
		t.Errorf("unknown: %+v", st)
	}
}

func TestPendingChecksAreNamed(t *testing.T) {
	c, p := newCI(t)
	c.runs = []map[string]any{run("test", "completed", "success"), run("build", "in_progress", "")}
	p.Config().Publish.RequiredChecks = []string{"build", "deploy-preview", "test"}
	st, err := p.Check(ctx, record)
	if err != nil || st.Detail != "2 of 3 check(s) not finished:\nbuild: still running\ndeploy-preview: not started" {
		t.Errorf("detail %q, error %v", st.Detail, err)
	}
	c.runs = nil
	p.Config().Publish.RequiredChecks = nil
	if st, _ := p.Check(ctx, record); st.Detail != "no check has started on the pull request" {
		t.Errorf("nothing reported: %q", st.Detail)
	}
}

func TestCheckWithoutChecksPassesButStillWatchesThePullRequest(t *testing.T) {
	c, p := newCI(t)
	p.Config().Publish.NoChecks = true
	c.runs = []map[string]any{run("lint", "completed", "failure")}
	if st, err := p.Check(ctx, record); err != nil || st.State != orchestrator.ChecksPassed {
		t.Errorf("no checks required: %+v %v", st, err)
	}
	c.pr["head"] = map[string]any{"sha": strings.Repeat("2", 40)}
	if st, _ := p.Check(ctx, record); st.State != orchestrator.ChecksForeignPush {
		t.Errorf("foreign push: %+v", st)
	}
	c.pr["head"] = map[string]any{"sha": headSHA}
	c.pr["state"] = "closed"
	if st, _ := p.Check(ctx, record); st.State != orchestrator.ChecksClosed {
		t.Errorf("closed: %+v", st)
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
	talk    []map[string]any
	inline  []map[string]any
	posted  []string
	headAt  string
	// one is what reading pull request 31 by itself answers.
	one map[string]any
	// merge is what MergeBase answers; merges and pushes count the calls.
	merge          mirror.BaseMerge
	merges, pushes int
}

func (g *gh) Import(context.Context, string, string, string) (mirror.Imported, error) {
	return mirror.Imported{}, nil
}
func (g *gh) Diff(context.Context, string, string, mirror.Limits) (mirror.Diff, error) {
	return mirror.Diff{}, nil
}
func (g *gh) OnBranch(context.Context, string, string, string) (bool, error) { return true, nil }
func (g *gh) HistoryAdded(context.Context, string, string, int64) ([]string, bool, error) {
	return nil, true, nil
}
func (g *gh) Push(context.Context, string, string, string, string) error {
	g.pushes++
	return nil
}
func (g *gh) MergeBase(_ context.Context, key, branch, base, name, email string) (mirror.BaseMerge, string, error) {
	g.merges++
	if key != "GH-7" || branch != "maestro/GH-7" || base != "main" || name == "" || email == "" {
		return mirror.BaseSkipped, "", context.Canceled
	}
	return g.merge, "3333333333333333333333333333333333333333", nil
}
func (g *gh) ForgetPush(_ context.Context, key string) error {
	g.forgot = append(g.forgot, key)
	return nil
}

func pullOn(n int, state, merged, login, repo string) map[string]any {
	p := map[string]any{"number": n, "state": state, "user": map[string]any{"login": login},
		"head": map[string]any{"ref": "maestro/GH-7", "sha": headSHA, "repo": map[string]any{"full_name": repo}}}
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
		case "GET /repos/ian/app/pulls/31":
			if g.one == nil {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(g.one)
		case "GET /repos/ian/app/pulls/31/reviews":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			start := min((page-1)*100, len(g.reviews))
			json.NewEncoder(w).Encode(g.reviews[start:min(start+100, len(g.reviews))])
		case "GET /repos/ian/app/issues/31/comments":
			if r.URL.Query().Get("page") != "1" {
				json.NewEncoder(w).Encode([]any{})
				return
			}
			json.NewEncoder(w).Encode(append([]map[string]any{}, g.talk...))
		case "GET /repos/ian/app/pulls/31/comments":
			json.NewEncoder(w).Encode(append([]map[string]any{}, g.inline...))
		case "POST /repos/ian/app/issues/31/comments":
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in)
			g.posted = append(g.posted, in.Body)
			g.talk = append(g.talk, said("sym[bot]", "2026-11-21T12:00:00Z", in.Body))
			w.WriteHeader(201)
			w.Write([]byte("{}"))
		case "GET /repos/ian/app/commits/" + headSHA:
			if g.headAt == "" {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"committer": map[string]any{"date": g.headAt}}})
		case "DELETE /repos/ian/app/git/refs/heads/maestro/GH-7":
			g.deleted = append(g.deleted, r.URL.Path)
			w.WriteHeader(g.delCode)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{Workspace: config.Workspace{Repo: "ian/app", BaseBranch: "main"},
		Publish: config.Publish{BranchPrefix: "maestro/", ReworkOnChangesRequested: true},
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
		if q := g.queries[0]; !strings.Contains(q, "head=ian%3Amaestro%2FGH-7") || !strings.Contains(q, "state=all") {
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
	g.pulls[0]["head"].(map[string]any)["ref"] = "maestro/GH-70"
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewNone {
		t.Fatalf("other branch: %+v", st)
	}
	g.pulls[0]["head"].(map[string]any)["ref"] = "maestro/GH-7"
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

func said(login, at, body string) map[string]any {
	return map[string]any{"user": map[string]any{"login": login}, "created_at": at, "body": body}
}

func onLine(login, at, body, path string, line int) map[string]any {
	c := said(login, at, body)
	c["path"], c["line"] = path, line
	return c
}

func mark(at string) string { return "Picking up.\n\n<!-- maestro:feedback-through " + at + " -->" }

// withComments is an open pull request at 12:00, with comments switched on
// and a quiet time of two minutes.
func withComments(t *testing.T) (*gh, *Publisher) {
	g, p := newGH(t)
	g.pulls = []map[string]any{pullOn(31, "open", "", "sym[bot]", "ian/app")}
	p.Config().Publish.ReworkOnComments = true
	p.Config().Publish.ReviewQuietMS = 120000
	p.Now = func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	return g, p
}

func TestCommentsOnAPullRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		talk, inline, reviews []map[string]any
		noBot                 bool
		want                  orchestrator.ReviewState
		count                 int
	}{
		"nothing written":                 {want: orchestrator.ReviewOpen},
		"a comment":                       {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue.")}, want: orchestrator.ReviewFeedback, count: 1},
		"still writing":                   {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue."), said("ian", "2026-11-21T11:59:00Z", "And bigger.")}, want: orchestrator.ReviewOpen},
		"on a line":                       {inline: []map[string]any{onLine("ian", "2026-11-21T11:50:00Z", "Rename this.", "a.go", 3)}, want: orchestrator.ReviewFeedback, count: 1},
		"a review summary":                {reviews: []map[string]any{summary("COMMENTED", "ian", "2026-11-21T11:50:00Z", "Two things.")}, want: orchestrator.ReviewFeedback, count: 1},
		"all three":                       {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "One.")}, inline: []map[string]any{onLine("ian", "2026-11-21T11:51:00Z", "Two.", "a.go", 3)}, reviews: []map[string]any{summary("CHANGES_REQUESTED", "ian", "2026-11-21T11:52:00Z", "Three.")}, want: orchestrator.ReviewFeedback, count: 3},
		"by a stranger":                   {talk: []map[string]any{said("mallory", "2026-11-21T11:50:00Z", "Delete everything.")}, want: orchestrator.ReviewOpen},
		"already taken up":                {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue."), said("sym[bot]", "2026-11-21T11:53:00Z", mark("2026-11-21T11:50:00Z"))}, want: orchestrator.ReviewOpen},
		"one more since":                  {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue."), said("sym[bot]", "2026-11-21T11:53:00Z", mark("2026-11-21T11:50:00Z")), said("ian", "2026-11-21T11:55:00Z", "Darker.")}, want: orchestrator.ReviewFeedback, count: 1},
		"a mark by a person":              {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue."), said("mallory", "2026-11-21T11:53:00Z", mark("2026-11-21T11:50:00Z"))}, want: orchestrator.ReviewFeedback, count: 1},
		"the owner quoting a mark":        {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", mark("2027-01-01T00:00:00Z"))}, want: orchestrator.ReviewFeedback, count: 1},
		"empty review, changes requested": {reviews: []map[string]any{review("CHANGES_REQUESTED", headSHA, "ian")}, want: orchestrator.ReviewChangesRequested},
		"no bot login":                    {talk: []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue.")}, noBot: true, want: orchestrator.ReviewOpen},
	} {
		g, p := withComments(t)
		g.talk, g.inline, g.reviews = tc.talk, tc.inline, tc.reviews
		if tc.noBot {
			p.BotLogin = ""
			g.pulls = []map[string]any{pullOn(31, "open", "", "ian", "ian/app")}
		}
		st, err := p.Review(ctx, ticket)
		if err != nil || st.State != tc.want || st.FeedbackCount != tc.count {
			t.Errorf("%s: %+v, error %v", name, st, err)
		}
	}
}

func summary(state, login, at, body string) map[string]any {
	r := review(state, headSHA, login)
	r["submitted_at"], r["body"] = at, body
	return r
}

func TestAcknowledgedCommentsStartOneRunOnly(t *testing.T) {
	g, p := withComments(t)
	g.talk = []map[string]any{said("ian", "2026-11-21T11:50:00Z", "Make it blue."), said("ian", "2026-11-21T11:51:00Z", "And bigger.")}
	st, err := p.Review(ctx, ticket)
	if err != nil || st.State != orchestrator.ReviewFeedback || st.FeedbackCount != 2 {
		t.Fatalf("%+v, %v", st, err)
	}
	if err := p.Acknowledge(ctx, ticket, st); err != nil {
		t.Fatal(err)
	}
	if len(g.posted) != 1 || !strings.Contains(g.posted[0], "the 2 new comments") ||
		!strings.Contains(g.posted[0], "<!-- maestro:feedback-through 2026-11-21T11:51:00Z -->") {
		t.Fatalf("posted %q", g.posted)
	}
	if st, _ := p.Review(ctx, ticket); st.State != orchestrator.ReviewOpen {
		t.Fatalf("after the mark: %+v", st)
	}
}

func bodies(fs []issue.Feedback) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Body)
	}
	return strings.Join(out, "|")
}

func TestFeedbackForARun(t *testing.T) {
	// Taken up by the service: the run gets the round the last mark started.
	g, p := withComments(t)
	g.talk = []map[string]any{
		said("ian", "2026-11-21T10:00:00Z", "Old."), said("sym[bot]", "2026-11-21T10:05:00Z", mark("2026-11-21T10:00:00Z")),
		said("ian", "2026-11-21T11:50:00Z", "Make it blue."), said("sym[bot]", "2026-11-21T11:53:00Z", mark("2026-11-21T11:50:00Z")),
	}
	g.inline = []map[string]any{onLine("ian", "2026-11-21T11:49:00Z", "Rename this.", "a.go", 3)}
	fs, err := p.Feedback(ctx, ticket)
	if err != nil || bodies(fs) != "Rename this.|Make it blue." || len(g.posted) != 0 {
		t.Fatalf("%+v, %v, posted %q", fs, err, g.posted)
	}
	if f := fs[0]; f.Source != "review" || f.Author != "ian" || f.Path != "a.go" || f.Line != 3 {
		t.Fatalf("%+v", f)
	}
	// A commit followed the mark: that round is over.
	g.headAt = "2026-11-21T11:58:00Z"
	if fs, err := p.Feedback(ctx, ticket); err != nil || len(fs) != 0 {
		t.Fatalf("after the commit: %+v, %v", fs, err)
	}
	// The owner moved the card by hand before the service took a comment
	// up: the run gets it and it is marked, so it starts no second run.
	g.talk = append(g.talk, said("ian", "2026-11-21T11:59:30Z", "Darker."))
	fs, err = p.Feedback(ctx, ticket)
	if err != nil || bodies(fs) != "Darker." || len(g.posted) != 1 || !strings.Contains(g.posted[0], "through 2026-11-21T11:59:30Z") {
		t.Fatalf("by hand: %+v, %v, posted %q", fs, err, g.posted)
	}
	// A retry of that run gets the same comment again.
	g.headAt = "2026-11-21T11:58:00Z"
	if fs, _ := p.Feedback(ctx, ticket); bodies(fs) != "Darker." || len(g.posted) != 1 {
		t.Fatalf("retry: %+v, posted %q", fs, g.posted)
	}

	// No pull request, a merged one, switched off: nothing.
	g.pulls = nil
	if fs, err := p.Feedback(ctx, ticket); err != nil || fs != nil {
		t.Fatalf("no pull request: %+v, %v", fs, err)
	}
	g.pulls = []map[string]any{pullOn(31, "closed", "2026-11-21T10:00:00Z", "sym[bot]", "ian/app")}
	if fs, _ := p.Feedback(ctx, ticket); fs != nil {
		t.Fatalf("merged: %+v", fs)
	}
	g.pulls = []map[string]any{pullOn(31, "open", "", "sym[bot]", "ian/app")}
	p.Config().Publish.ReworkOnComments = false
	if fs, _ := p.Feedback(ctx, ticket); fs != nil {
		t.Fatalf("switched off: %+v", fs)
	}
}

func TestALongCommentIsCut(t *testing.T) {
	g, p := withComments(t)
	g.talk = []map[string]any{said("ian", "2026-11-21T11:50:00Z", strings.Repeat("é", maxNoteRunes+50))}
	fs, err := p.Feedback(ctx, ticket)
	if err != nil || len(fs) != 1 || !strings.HasSuffix(fs[0].Body, "[cut off here]") || len([]rune(fs[0].Body)) > maxNoteRunes+20 {
		t.Fatalf("%d notes, %v", len(fs), err)
	}
}

func (h *hub) sent(key string) []map[string]any {
	var out []map[string]any
	for i, r := range h.requests {
		if r == key {
			out = append(out, h.bodies[i])
		}
	}
	return out
}

func TestScreenshotsAreStoredAndShown(t *testing.T) {
	l := newLab(t)
	l.pub.GitURL = "" // links go to github.com
	head := l.change("index.html", "<h1>Hello</h1>\n")
	v := worker.VerifyResult{Passed: true, ScreenshotNote: "Left out: \"c\" is not a PNG picture of a usable size.",
		Screenshots: []worker.Screenshot{{Caption: "index.html", PNG: []byte("one")}, {Caption: "a `b` @ian", PNG: []byte("two")}}}
	links, err := l.pub.storeFor(ctx, l.ws.Key, head, v.Screenshots)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/ian/app/blob/" + commitSHA + "/GH-7-abc/" + head[:12] + "/1.png?raw=true"
	if len(links) != 2 || links[0].url != want || links[0].caption != "index.html" {
		t.Fatalf("links %+v", links)
	}
	// A new branch: one commit without a parent, holding both pictures below the ticket and the commit.
	tree := l.hub.sent("POST /repos/ian/app/git/trees")[0]
	entries := tree["tree"].([]any)
	if _, based := tree["base_tree"]; based || len(entries) != 2 || entries[1].(map[string]any)["path"] != "GH-7-abc/"+head[:12]+"/2.png" ||
		entries[1].(map[string]any)["sha"] != blobSHA {
		t.Fatalf("tree %v", tree)
	}
	if c := l.hub.sent("POST /repos/ian/app/git/commits")[0]; len(c["parents"].([]any)) != 0 || c["tree"] != treeSHA {
		t.Fatalf("commit %v", c)
	}
	if r := l.hub.sent("POST /repos/ian/app/git/refs")[0]; r["ref"] != "refs/heads/maestro/screenshots" || r["sha"] != commitSHA {
		t.Fatalf("ref %v", r)
	}
	if b := l.hub.sent("POST /repos/ian/app/git/blobs")[0]; b["content"] != "b25l" || b["encoding"] != "base64" {
		t.Fatalf("blob %v", b)
	}

	// The branch exists: the commit is added to it, and it is never forced.
	l.hub.shotTip, l.hub.refuse = oldTip, 1
	if _, err := l.pub.storeFor(ctx, l.ws.Key, head, v.Screenshots); err != nil {
		t.Fatal(err)
	}
	trees := l.hub.sent("POST /repos/ian/app/git/trees")
	if trees[1]["base_tree"] != oldTree || len(trees) != 3 {
		t.Fatalf("%d trees, base %v", len(trees), trees[1]["base_tree"])
	}
	moves := l.hub.sent("PATCH /repos/ian/app/git/refs/heads/maestro/screenshots")
	if len(moves) != 2 || moves[1]["force"] != false || moves[1]["sha"] != commitSHA {
		t.Fatalf("moves %v", moves)
	}

	// In the description.
	text := body("", nil, Proof{Screenshots: []shotLink{{"index.html", want}, {"a `b` @ian", want}}, ScreenshotNote: v.ScreenshotNote}, "")
	for _, part := range []string{"## Screenshots", "`index.html`\n\n![Screenshot 1](" + want + ")", "`a 'b' @\u200bian`", "Left out: \"c\""} {
		if !strings.Contains(text, part) {
			t.Errorf("the description lacks %q:\n%s", part, text)
		}
	}
	if strings.Contains(body("", nil, Proof{}, ""), "Screenshots") {
		t.Error("a section without screenshots")
	}
}

func TestPublishGoesOnWithoutScreenshotsThatCannotBeStored(t *testing.T) {
	l := newLab(t)
	l.change("index.html", "<h1>Hello</h1>\n")
	l.hub.status["POST /repos/ian/app/git/blobs"] = 403
	v := worker.VerifyResult{Passed: true, Screenshots: []worker.Screenshot{{Caption: "index.html", PNG: []byte("one")}}}
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, v)
	if err != nil || rej != nil || rec.PRNumber != 31 {
		t.Fatalf("record %+v, rejection %+v, error %v", rec, rej, err)
	}
	text := l.hub.sent("POST /repos/ian/app/pulls")[0]["body"].(string)
	if !strings.Contains(text, "## Screenshots\n\nThe screenshots were taken but could not be stored in the repository.") || strings.Contains(text, "![") {
		t.Fatalf("description:\n%s", text)
	}
}

func TestPublishShowsScreenshots(t *testing.T) {
	l := newLab(t)
	head := l.change("index.html", "<h1>Hello</h1>\n")
	v := worker.VerifyResult{Passed: true, Screenshots: []worker.Screenshot{{Caption: "index.html", PNG: []byte("one")}}}
	if _, rej, err := l.pub.Publish(ctx, l.is, l.ws, v); err != nil || rej != nil {
		t.Fatalf("rejection %+v, error %v", rej, err)
	}
	text := l.hub.sent("POST /repos/ian/app/pulls")[0]["body"].(string)
	if !strings.Contains(text, "/ian/app/blob/"+commitSHA+"/GH-7-abc/"+head[:12]+"/1.png?raw=true)") {
		t.Fatalf("description:\n%s", text)
	}
}

// A comment on a line was drafted before the service took up another
// comment, and submitted with its review afterwards. It must not be lost.
func TestACommentDraftedBeforeAMarkCountsFromItsReview(t *testing.T) {
	g, p := withComments(t)
	drafted := onLine("ian", "2026-11-21T11:40:00Z", "Rename this.", "a.go", 3)
	drafted["pull_request_review_id"] = 77
	g.inline = []map[string]any{drafted}
	g.talk = []map[string]any{said("ian", "2026-11-21T11:45:00Z", "Make it blue."), said("sym[bot]", "2026-11-21T11:48:00Z", mark("2026-11-21T11:45:00Z"))}
	rev := summary("COMMENTED", "ian", "2026-11-21T11:50:00Z", "")
	rev["id"] = 77
	g.reviews = []map[string]any{rev}
	st, err := p.Review(ctx, ticket)
	if err != nil || st.State != orchestrator.ReviewFeedback || st.FeedbackCount != 1 ||
		!st.FeedbackThrough.Equal(time.Date(2026, 11, 21, 11, 50, 0, 0, time.UTC)) {
		t.Fatalf("%+v, %v", st, err)
	}
}

func TestReviewSeesAConflict(t *testing.T) {
	g, p := newGH(t)
	g.pulls = []map[string]any{pullOn(31, "open", "", "sym[bot]", "ian/app")}
	if st, err := p.Review(ctx, ticket); err != nil || st.State != orchestrator.ReviewOpen || st.Conflicted {
		t.Fatalf("pull request that cannot be read by itself: %+v, %v", st, err)
	}
	g.one = pullOn(31, "open", "", "sym[bot]", "ian/app")
	g.one["mergeable_state"] = "unknown"
	if st, _ := p.Review(ctx, ticket); st.Conflicted {
		t.Fatalf("not known yet: %+v", st)
	}
	g.one["mergeable_state"] = "dirty"
	if st, err := p.Review(ctx, ticket); err != nil || st.State != orchestrator.ReviewOpen || !st.Conflicted {
		t.Fatalf("conflicting: %+v, %v", st, err)
	}
}

func TestUpdateBranch(t *testing.T) {
	g, p := newGH(t)
	p.Config().Publish.UpdateBranches = true
	p.BotName, p.BotEmail = "sym[bot]", "sym@example.com"
	fetched := 0
	p.Fetch = func(context.Context) error { fetched++; return nil }

	// No open pull request: nothing is fetched or merged.
	g.pulls = []map[string]any{pullOn(31, "closed", "2026-11-21T10:00:00Z", "sym[bot]", "ian/app")}
	if up, err := p.UpdateBranch(ctx, ticket); err != nil || up.State != orchestrator.BaseUnchanged || fetched != 0 || g.merges != 0 {
		t.Fatalf("merged pull request: %+v, %v", up, err)
	}
	g.pulls = []map[string]any{pullOn(31, "open", "", "sym[bot]", "ian/app")}
	for merge, want := range map[mirror.BaseMerge]orchestrator.BaseState{
		mirror.BaseSkipped: orchestrator.BaseUnchanged, mirror.BaseCurrent: orchestrator.BaseUnchanged,
		mirror.BaseConflict: orchestrator.BaseConflicted, mirror.BaseMerged: orchestrator.BaseUpdated,
	} {
		g.merge, g.pushes = merge, 0
		up, err := p.UpdateBranch(ctx, ticket)
		if err != nil || up.State != want || up.PRNumber != 31 {
			t.Fatalf("merge %v: %+v, %v", merge, up, err)
		}
		if pushed := g.pushes == 1; pushed != (merge == mirror.BaseMerged) {
			t.Fatalf("merge %v: %d push(es)", merge, g.pushes)
		}
	}
	if fetched != 4 {
		t.Fatalf("fetched %d times", fetched)
	}

	// Switched off.
	p.Config().Publish.UpdateBranches = false
	g.merges = 0
	if up, err := p.UpdateBranch(ctx, ticket); err != nil || up.State != orchestrator.BaseUnchanged || g.merges != 0 {
		t.Fatalf("switched off: %+v, %v", up, err)
	}
}
