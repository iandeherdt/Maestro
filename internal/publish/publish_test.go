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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/mirror"
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
