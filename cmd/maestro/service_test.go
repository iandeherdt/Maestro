package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/setup"
)

// The image of this test has a stand-in for Claude Code: it is "logged in"
// when a file exists in its state directory, and a turn commits one file.
const e2eDockerfile = `FROM docker.io/alpine/git:latest
RUN addgroup -g 10001 agent \
 && adduser -D -u 10001 -G agent agent \
 && mkdir -p /workspace /claude /handoff \
 && chown agent:agent /workspace /claude /handoff \
 && git config --system --add safe.directory /mirror \
 && git config --system --add safe.directory /workspace/repo \
 && printf '%s\n' '#!/bin/sh' \
    'if [ "$1" = auth ]; then test -e /claude/login; exit $?; fi' \
    'sid=""' \
    'while [ $# -gt 0 ]; do case "$1" in --session-id|--resume) sid="$2"; shift;; esac; shift; done' \
    'cat > /tmp/prompt' \
    'grep -q "Add the agent file" /tmp/prompt || exit 9' \
    'test -e agent.txt || { echo "made by the agent" > agent.txt && git add agent.txt && git commit -q -m "Add agent.txt" || exit 8; }' \
    'echo "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$sid\"}"' \
    'echo "{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"Done.\",\"session_id\":\"$sid\"}"' \
    > /usr/local/bin/claude \
 && chmod 755 /usr/local/bin/claude
COPY maestro /usr/local/bin/maestro
`

// fakeHub is GitHub as far as the service uses it for one issue.
type fakeHub struct {
	mu       sync.Mutex
	labels   []string
	events   []map[string]any
	comments []map[string]any
	pull     map[string]any
	unknown  []string
	remote   string
}

func (h *fakeHub) has(label string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

func (h *fakeHub) issue() map[string]any {
	labels := []map[string]string{}
	for _, l := range h.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]any{"node_id": "I_7", "number": 7, "title": "Add the agent file", "body": "Please add agent.txt.",
		"state": "open", "html_url": "https://github.com/ian/app/issues/7", "user": map[string]string{"login": "ian"},
		"labels": labels, "created_at": "2026-11-01T10:00:00Z", "updated_at": time.Now().UTC().Format(time.RFC3339)}
}

func (h *fakeHub) label(actor, name string) {
	h.labels = append(h.labels, name)
	h.events = append(h.events, map[string]any{"event": "labeled", "actor": map[string]string{"login": actor}, "label": map[string]string{"name": name}})
}

func (h *fakeHub) head() string {
	out, _ := exec.Command("git", "--git-dir", h.remote, "rev-parse", "refs/heads/maestro/GH-7").Output()
	return strings.TrimSpace(string(out))
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	page := func(items []map[string]any) {
		if p := r.URL.Query().Get("page"); p != "" && p != "1" {
			items = nil
		}
		if items == nil {
			items = []map[string]any{}
		}
		send(items)
	}
	const repo = "/repos/ian/app"
	key := r.Method + " " + r.URL.Path
	switch {
	case key == "POST /app/installations/42/access_tokens":
		w.WriteHeader(201)
		send(map[string]any{"token": "ghs_e2etoken0123456789", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case r.Header.Get("Authorization") != "Bearer ghs_e2etoken0123456789":
		w.WriteHeader(401)
	case key == "GET "+repo:
		send(map[string]any{"default_branch": "main"})
	case key == "GET "+repo+"/issues":
		want := strings.ToLower(r.URL.Query().Get("labels"))
		var items []map[string]any
		for _, l := range h.labels {
			if strings.ToLower(l) == want {
				items = append(items, h.issue())
			}
		}
		page(items)
	case key == "GET "+repo+"/issues/7":
		send(h.issue())
	case key == "GET "+repo+"/issues/7/timeline":
		page(h.events)
	case key == "GET "+repo+"/issues/7/dependencies/blocked_by":
		page(nil)
	case key == "GET "+repo+"/issues/7/comments":
		page(h.comments)
	case key == "POST "+repo+"/issues/7/comments":
		h.comments = append(h.comments, map[string]any{"id": 1000 + len(h.comments), "body": in["body"],
			"user": map[string]string{"login": "sym-e2e[bot]"}, "created_at": time.Now().UTC().Format(time.RFC3339)})
		w.WriteHeader(201)
		send(h.comments[len(h.comments)-1])
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, repo+"/issues/comments/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, repo+"/issues/comments/"))
		for _, c := range h.comments {
			if c["id"] == id {
				c["body"] = in["body"]
			}
		}
		send(map[string]any{})
	case key == "POST "+repo+"/issues/7/labels":
		for _, l := range in["labels"].([]any) {
			h.label("sym-e2e[bot]", l.(string))
		}
		send([]any{})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, repo+"/issues/7/labels/"):
		name, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), repo+"/issues/7/labels/"))
		var keep []string
		for _, l := range h.labels {
			if !strings.EqualFold(l, name) {
				keep = append(keep, l)
			}
		}
		h.labels = keep
		send([]any{})
	case key == "GET "+repo+"/pulls":
		if h.pull == nil {
			page(nil)
		} else {
			page([]map[string]any{h.pull})
		}
	case key == "POST "+repo+"/pulls":
		h.pull = map[string]any{"number": 31, "state": "open", "merged": false, "draft": in["draft"], "node_id": "PR_1",
			"title": in["title"], "user": map[string]any{"login": "sym-e2e[bot]"},
			"head": map[string]any{"ref": in["head"], "sha": h.head(), "repo": map[string]any{"full_name": "ian/app"}},
			"base": map[string]any{"ref": in["base"]}}
		w.WriteHeader(201)
		send(h.pull)
	case key == "GET "+repo+"/pulls/31" || key == "PATCH "+repo+"/pulls/31":
		send(h.pull)
	case key == "GET "+repo+"/pulls/31/reviews" || key == "GET "+repo+"/issues/31/comments" || key == "GET "+repo+"/pulls/31/comments":
		page(nil)
	case strings.HasSuffix(r.URL.Path, "/check-runs"):
		send(map[string]any{"total_count": 0, "check_runs": []any{}})
	case strings.HasSuffix(r.URL.Path, "/status"):
		send(map[string]any{"statuses": []any{}})
	case key == "POST /graphql":
		h.pull["draft"] = false
		send(map[string]any{"data": map[string]any{}})
	default:
		h.unknown = append(h.unknown, key)
		w.WriteHeader(404)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=o", "GIT_AUTHOR_EMAIL=o@example.com", "GIT_COMMITTER_NAME=o", "GIT_COMMITTER_EMAIL=o@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestServiceTakesAnIssueToAPullRequest runs the whole service, in real
// containers, against a stand-in for GitHub and a stand-in for Claude Code.
func TestServiceTakesAnIssueToAPullRequest(t *testing.T) {
	engine := "docker"
	if e := os.Getenv("MAESTRO_TEST_ENGINE"); e != "" {
		engine = e
	}
	if exec.Command(engine, "info").Run() != nil {
		t.Skip("no container engine")
	}
	root, err := os.MkdirTemp("", "maestro-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// The repository on "GitHub".
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	gitIn(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-q", "-m", "base")
	remote := filepath.Join(root, "remotes", "ian", "app.git")
	os.MkdirAll(filepath.Dir(remote), 0o755)
	gitIn(t, root, "clone", "-q", "--bare", src, remote)

	hub := &fakeHub{remote: remote}
	srv := httptest.NewServer(http.HandlerFunc(hub.serve))
	defer srv.Close()

	// What `maestro setup` leaves behind.
	dir := filepath.Join(root, "config")
	st, err := setup.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyFile := filepath.Join(dir, "app.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	worker := fmt.Sprintf("e2e%d", time.Now().UnixNano()%100000)
	st.Repo, st.AppID, st.AppSlug, st.AppKeyFile, st.InstallationID, st.Worker = "ian/app", "1234", "sym-e2e", keyFile, "42", worker
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(dir, "WORKFLOW.md")
	os.WriteFile(workflow, []byte(`---
tracker:
  kind: github
  provider:
    repo: ian/app
    state_source: labels
    bot_login: "sym-e2e[bot]"
    api_url: `+srv.URL+`
  allowed_authors: [ian]
polling:
  interval_ms: 5000
workspace:
  root: "`+filepath.Join(root, "workspaces")+`"
  repo: ian/app
sandbox:
  backend: `+engine+`
  image: maestro-e2e-sandbox:latest
  memory_max: 512M
  cpu_quota: 100%
  tasks_max: 256
verify:
  command: test "$(cat agent.txt)" = "made by the agent"
publish:
  checks_poll_interval_ms: 1000
  required_checks: none
budget:
  state_dir: "`+filepath.Join(root, "state")+`"
worker:
  name: `+worker+`
---
Work on {{ issue.identifier }}: {{ issue.title }}
`), 0o600)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	in, err := open(dir, workflow, log)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "maestro")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var out bytes.Buffer
	if err := in.box.BuildImage(ctx, dockerbox.Build{Dockerfile: e2eDockerfile, Binary: bin, Out: &out}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	defer in.box.Purge(context.Background())
	gh := endpoints{api: srv.URL, git: filepath.Join(root, "remotes"), fileRemote: true}

	// Without a login the service says what to do and starts nothing.
	err = serve(ctx, in, gh, log, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "maestro login") {
		t.Fatalf("without a login: %v", err)
	}
	if in.box.ProxyRunning(ctx, "agent") {
		t.Fatal("the proxies were left running")
	}
	if o, err := exec.Command(engine, "run", "--rm", "--user", "10001", "--entrypoint", "", "--mount",
		"type=volume,src="+in.box.ClaudeVolume()+",dst=/claude", in.cfg.Sandbox.Image, "touch", "/claude/login").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, o)
	}

	// A repository without the base branch (an empty one) is reported at
	// the start, not as a failing ticket.
	gitIn(t, root, "--git-dir", remote, "branch", "-m", "main", "other")
	err = serve(ctx, in, gh, log, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `has no branch "main"`) {
		t.Fatalf("without the base branch: %v", err)
	}
	gitIn(t, root, "--git-dir", remote, "branch", "-m", "other", "main")

	hub.label("ian", "status:todo")
	hub.label("ian", "run:"+worker)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	var said bytes.Buffer
	go func() { done <- serve(runCtx, in, gh, log, &said) }()

	deadline := time.Now().Add(3 * time.Minute)
	for !hub.has("status:human review") {
		select {
		case err := <-done:
			t.Fatalf("the service stopped: %v\n%s", err, logs.String())
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			hub.mu.Lock()
			t.Fatalf("no hand-off. labels %v, unknown requests %v, comments %v\n%s", hub.labels, hub.unknown, hub.comments, logs.String())
		}
	}
	// The commands of another terminal reach the running service.
	var errb bytes.Buffer
	var statusOut bytes.Buffer
	if code := run([]string{"status", "--dir", dir, workflow}, &statusOut, &errb); code != 0 || !strings.Contains(statusOut.String(), "Maestro is running (worker") {
		t.Errorf("status: %d %s %s", code, statusOut.String(), errb.String())
	}
	if code := run([]string{"pause", "--dir", dir, "--file", workflow, "back", "on", "monday"}, io.Discard, &errb); code != 0 {
		t.Errorf("pause: %d %s", code, errb.String())
	}
	paused := false
	for i := 0; i < 50 && !paused; i++ {
		statusOut.Reset()
		run([]string{"status", "--dir", dir, workflow}, &statusOut, &errb)
		paused = strings.Contains(statusOut.String(), "PAUSED: back on monday")
		time.Sleep(100 * time.Millisecond)
	}
	if !paused {
		t.Errorf("the pause did not show:\n%s", statusOut.String())
	}
	// A second service for the same installation is refused.
	if err := serve(ctx, in, gh, log, io.Discard); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second service: %v", err)
	}
	if code := run([]string{"resume", "--dir", dir, workflow}, io.Discard, &errb); code != 0 {
		t.Errorf("resume: %d %s", code, errb.String())
	}

	stop()
	if err := <-done; err != nil {
		t.Fatalf("stopping: %v", err)
	}
	if code := run([]string{"status", "--dir", dir, workflow}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "not running") {
		t.Errorf("status after the stop: %d %s", code, errb.String())
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.unknown) != 0 {
		t.Errorf("requests the stand-in does not know: %v", hub.unknown)
	}
	if hub.pull == nil || hub.pull["draft"] != false || hub.pull["base"].(map[string]any)["ref"] != "main" {
		t.Fatalf("pull request: %v", hub.pull)
	}
	if got := gitIn(t, root, "--git-dir", remote, "show", "refs/heads/maestro/GH-7:agent.txt"); got != "made by the agent" {
		t.Errorf("the pushed branch holds %q", got)
	}
	if got := gitIn(t, root, "--git-dir", remote, "log", "-1", "--format=%an <%ae>", "refs/heads/maestro/GH-7"); got != "sym-e2e[bot] <sym-e2e[bot]@users.noreply.github.com>" {
		t.Errorf("commit author = %q", got)
	}
	if len(hub.labels) != 2 {
		t.Errorf("labels = %v", hub.labels)
	}
	if !strings.Contains(said.String(), "Maestro is running for ian/app") || !strings.Contains(said.String(), "status:todo  run:"+worker) {
		t.Errorf("start message:\n%s", said.String())
	}
	if _, err := os.Stat(filepath.Join(root, "state", "audit", "GH-7.jsonl")); err != nil {
		t.Errorf("no audit file: %v", err)
	}
	if in.box.ProxyRunning(context.Background(), "agent") {
		t.Error("the proxies were left running after the stop")
	}
	if strings.Contains(logs.String(), "ghs_e2etoken") {
		t.Error("a token was logged")
	}
}

func TestOpenExplainsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	write := func(backend string) string {
		path := filepath.Join(dir, "WORKFLOW.md")
		os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n  allowed_authors: [ian]\nworkspace:\n  root: "+dir+"/ws\n  repo: ian/app\n"+
			"sandbox:\n  backend: "+backend+"\n  network:\n    proxy: 127.0.0.1:3128\nverify:\n  command: 'true'\nbudget:\n  state_dir: "+dir+"/state\nworker:\n  name: mac\n---\n"), 0o600)
		return path
	}
	// A file from before the Docker sandbox existed.
	_, err := open(dir, write("macos-vm"), nil)
	if err == nil || !strings.Contains(err.Error(), "maestro setup") || !strings.Contains(err.Error(), "WORKFLOW.md.old") {
		t.Errorf("old backend: %v", err)
	}
	// Setup was never run here.
	_, err = open(dir, write("docker"), nil)
	if err == nil || !strings.Contains(err.Error(), "Run `maestro setup` first") {
		t.Errorf("no setup state: %v", err)
	}
	// Setup was done for another repository.
	st, _ := setup.LoadState(dir)
	st.Repo, st.AppID, st.AppSlug, st.AppKeyFile, st.InstallationID = "ian/other", "1", "sym", filepath.Join(dir, "k.pem"), "2"
	st.Save()
	_, err = open(dir, write("docker"), nil)
	if err == nil || !strings.Contains(err.Error(), "ian/other") {
		t.Errorf("another repository: %v", err)
	}
	st.Repo = "ian/app"
	st.Save()
	in, err := open(dir, write("docker"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.box.Worker != "mac" || in.box.Image != "maestro-sandbox:latest" || in.box.Memory != "6g" || in.box.CPUs != "3.00" ||
		in.box.PIDs != 2048 || in.box.BotName != "sym[bot]" || in.box.MirrorDir != filepath.Join(dir, "state", "mirror") ||
		in.box.HandoffDir != filepath.Join(dir, "ws", ".handoff") {
		t.Errorf("box = %+v", in.box)
	}
	// No file at all.
	var errb bytes.Buffer
	if code := run([]string{"run", "--dir", dir, filepath.Join(dir, "missing.md")}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "maestro setup") {
		t.Errorf("missing file: %d %s", code, errb.String())
	}
	for quota, want := range map[string]string{"300%": "3.00", "50%": "0.50", "125%": "1.25", "x": ""} {
		if got := cpus(quota); got != want {
			t.Errorf("cpus(%s) = %q", quota, got)
		}
	}
}
