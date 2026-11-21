package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func ui(answers string) (*UI, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return NewUI(strings.NewReader(answers), out), out
}

func state(t *testing.T) *State {
	t.Helper()
	st, err := LoadState(filepath.Join(t.TempDir(), "symphony"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// fakeStep is done after it has run n times.
type fakeStep struct {
	name  string
	done  bool
	runs  int
	fail  error
	stays bool
}

func (f *fakeStep) Title() string { return f.name }
func (f *fakeStep) Check(context.Context, *State) (Status, error) {
	return Status{Done: f.done, Detail: f.name + " detail"}, nil
}
func (f *fakeStep) Run(_ context.Context, _ *UI, st *State) error {
	f.runs++
	if f.fail != nil {
		return f.fail
	}
	f.done = !f.stays
	st.Repo = "set/by-" + f.name
	return nil
}

func TestRunSkipsFinishedStepsAndSavesAfterEach(t *testing.T) {
	st := state(t)
	a, b, c := &fakeStep{name: "a", done: true}, &fakeStep{name: "b"}, &fakeStep{name: "c"}
	u, out := ui("")
	if err := Run(ctx, u, st, []Step{a, b, c}); err != nil {
		t.Fatal(err)
	}
	if a.runs != 0 || b.runs != 1 || c.runs != 1 {
		t.Errorf("runs %d %d %d", a.runs, b.runs, c.runs)
	}
	for _, want := range []string{"Step 1 of 3: a", "Already done. a detail", "Step 3 of 3: c", "All 3 steps are complete."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	again, err := LoadState(st.Dir())
	if err != nil || again.Repo != "set/by-c" {
		t.Errorf("state was not saved: %+v %v", again, err)
	}
}

func TestRunStopsAtAStepThatFailsOrStaysUnfinished(t *testing.T) {
	st := state(t)
	boom := errors.New("boom")
	b, c := &fakeStep{name: "b", fail: boom}, &fakeStep{name: "c"}
	u, _ := ui("")
	if err := Run(ctx, u, st, []Step{b, c}); !errors.Is(err, boom) || c.runs != 0 {
		t.Fatalf("%v, later step ran %d times", err, c.runs)
	}
	b = &fakeStep{name: "b", stays: true}
	if err := Run(ctx, u, st, []Step{b, c}); !errors.Is(err, ErrAborted) || c.runs != 0 {
		t.Fatalf("%v, later step ran %d times", err, c.runs)
	}
}

func TestPrompts(t *testing.T) {
	u, out := ui("\nnope\nian/app\n")
	got, err := u.Ask("Repo", "", validRepo)
	if err != nil || got != "ian/app" {
		t.Fatalf("%q %v", got, err)
	}
	if strings.Count(out.String(), "owner/name") != 2 {
		t.Errorf("invalid answers were not explained:\n%s", out)
	}
	u, _ = ui("\n")
	if got, _ := u.Ask("Repo", "ian/app", validRepo); got != "ian/app" {
		t.Errorf("default not used: %q", got)
	}
	u, _ = ui("maybe\n\nYES\n")
	if ok, err := u.Confirm("Go?"); err != nil || !ok {
		t.Errorf("%v %v", ok, err)
	}
	u, _ = ui("n\n")
	if ok, _ := u.Confirm("Go?"); ok {
		t.Error("no was taken as yes")
	}
	// The end of input is never consent.
	u, _ = ui("")
	if ok, err := u.Confirm("Go?"); ok || !errors.Is(err, ErrAborted) {
		t.Errorf("end of input: %v %v", ok, err)
	}
}

func TestStateDirectoryRules(t *testing.T) {
	if _, err := LoadState("relative/dir"); err == nil {
		t.Error("a relative directory was accepted")
	}
	open := filepath.Join(t.TempDir(), "open")
	os.Mkdir(open, 0o777)
	os.Chmod(open, 0o777)
	if _, err := LoadState(open); err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Errorf("world-writable directory: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "s")
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %o", fi.Mode().Perm())
	}
	st.AppID = "5"
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "setup.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %o", fi.Mode().Perm())
	}
	os.WriteFile(filepath.Join(dir, "setup.json"), []byte("{broken"), 0o600)
	if _, err := LoadState(dir); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("damaged state: %v", err)
	}
}

func machine(missing map[string]bool, gitVersion, goos string, cgroup bool) *Machine {
	return &Machine{
		LookPath: func(name string) (string, error) {
			if missing[name] {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + name, nil
		},
		Output: func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "git" {
				return gitVersion, nil
			}
			return "2.1.0 (Claude Code)", nil
		},
		GOOS:   goos,
		Exists: func(string) bool { return cgroup },
	}
}

func TestMachineChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		m    *Machine
		want []string
	}{
		"all good linux": {machine(nil, "git version 2.43.0", "linux", true), nil},
		"all good mac":   {machine(map[string]bool{"systemd-run": true}, "git version 2.39.5 (Apple Git-154)", "darwin", false), nil},
		"rhel 8 git":     {machine(nil, "git version 2.31.1", "linux", true), nil},
		"no git":         {machine(map[string]bool{"git": true}, "", "linux", true), []string{"Git is not installed"}},
		"old git":        {machine(nil, "git version 2.30.2", "linux", true), []string{"too old"}},
		"no claude":      {machine(map[string]bool{"claude": true}, "git version 2.43.0", "linux", true), []string{"Claude Code CLI is not installed"}},
		"no systemd":     {machine(map[string]bool{"systemd-run": true}, "git version 2.43.0", "linux", true), []string{"systemd-run"}},
		"cgroup v1":      {machine(nil, "git version 2.43.0", "linux", false), []string{"cgroup v2"}},
		"windows":        {machine(nil, "git version 2.43.0", "windows", true), []string{"not supported"}},
	} {
		status, err := tc.m.Check(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if status.Done != (len(tc.want) == 0) {
			t.Errorf("%s: done=%v, detail %q", name, status.Done, status.Detail)
		}
		for _, w := range tc.want {
			if !strings.Contains(status.Detail, w) {
				t.Errorf("%s: detail %q lacks %q", name, status.Detail, w)
			}
		}
	}
}

func TestMachineRunRechecksUntilFixed(t *testing.T) {
	missing := map[string]bool{"claude": true}
	m := machine(missing, "git version 2.43.0", "linux", true)
	u, out := ui("y\n")
	err := m.Run(ctx, u, nil) // one recheck, then the input ends
	if !errors.Is(err, ErrAborted) || strings.Count(out.String(), "Please fix") != 2 {
		t.Fatalf("still missing: %v\n%s", err, out)
	}
	delete(missing, "claude")
	u, _ = ui("")
	if err := m.Run(ctx, u, nil); err != nil {
		t.Fatalf("fixed: %v", err)
	}
	u, _ = ui("n\n")
	missing["git"] = true
	if err := m.Run(ctx, u, nil); !errors.Is(err, ErrAborted) {
		t.Fatalf("declined: %v", err)
	}
}

// github is a stand-in for the pages and endpoints the App steps use.
type github struct {
	t         *testing.T
	mu        sync.Mutex
	key       string
	manifest  map[string]any
	state     string
	converted int
	installed bool
	convertTo int
	srv       *httptest.Server
	labels    map[string]bool
	created   []string
	scopes    []string
	refuse    int
}

func newGitHub(t *testing.T) *github {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &github{t: t, key: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *github) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/settings/apps/new":
		r.ParseForm()
		json.Unmarshal([]byte(r.PostForm.Get("manifest")), &g.manifest)
		g.state = r.URL.Query().Get("state")
	case r.Method == "POST" && r.URL.Path == "/app-manifests/thecode/conversions":
		g.converted++
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(400)
			return
		}
		if g.convertTo != 0 {
			w.WriteHeader(g.convertTo)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"id": 4242, "slug": "symphony-ian-abc123", "pem": g.key,
			"client_secret": "CLIENTSECRET", "webhook_secret": "HOOKSECRET"})
	case r.Method == "GET" && r.URL.Path == "/repos/ian/app/installation":
		if !g.installed {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"id": 777}`)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/ian/app/labels/"):
		if r.Header.Get("Authorization") != "Bearer ghs_x" {
			w.WriteHeader(401)
			return
		}
		if !g.labels[strings.TrimPrefix(r.URL.Path, "/repos/ian/app/labels/")] {
			w.WriteHeader(404)
		}
	case r.Method == "POST" && r.URL.Path == "/repos/ian/app/labels":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if g.refuse != 0 {
			w.WriteHeader(g.refuse)
			return
		}
		if g.labels[in["name"]] {
			w.WriteHeader(422)
			return
		}
		if g.labels == nil {
			g.labels = map[string]bool{}
		}
		g.labels[in["name"]] = true
		g.created = append(g.created, in["name"])
		w.WriteHeader(201)
	case r.Method == "POST" && r.URL.Path == "/app/installations/777/access_tokens":
		var in struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		for k, v := range in.Permissions {
			g.scopes = append(g.scopes, k+":"+v)
		}
		if len(in.Repositories) != 1 || in.Repositories[0] != "app" {
			w.WriteHeader(422)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	default:
		w.WriteHeader(404)
	}
}

var (
	actionRE = regexp.MustCompile(`action="([^"]+)"`)
	valueRE  = regexp.MustCompile(`name="manifest" value="([^"]+)"`)
)

// browse does what the operator's browser does: load the local page,
// submit its form to GitHub, and follow GitHub's redirect back.
func (g *github) browse(local string, tamper func(callback *url.URL)) (int, string) {
	resp, err := http.Get(local + "/")
	if err != nil {
		g.t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	action := actionRE.FindSubmatch(page)
	value := valueRE.FindSubmatch(page)
	if action == nil || value == nil {
		g.t.Fatalf("the local page has no form:\n%s", page)
	}
	unescape := strings.NewReplacer("&#34;", `"`, "&amp;", "&", "&#39;", "'", "&lt;", "<", "&gt;", ">")
	if _, err := http.PostForm(unescape.Replace(string(action[1])), url.Values{"manifest": {unescape.Replace(string(value[1]))}}); err != nil {
		g.t.Fatal(err)
	}
	g.mu.Lock()
	redirect, _ := g.manifest["redirect_url"].(string)
	cb, _ := url.Parse(redirect)
	cb.RawQuery = url.Values{"code": {"thecode"}, "state": {g.state}}.Encode()
	g.mu.Unlock()
	if tamper != nil {
		tamper(cb)
	}
	resp, err = http.Get(cb.String())
	if err != nil {
		g.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func TestCreateAndInstallApp(t *testing.T) {
	g := newGitHub(t)
	gh := GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}
	st := state(t)
	create := &CreateApp{GitHub: gh, Port: -1, Wait: 10 * time.Second}
	create.Port = freePort(t)
	create.Ready = func(local string) {
		go func() {
			// A forged callback first: wrong state, then a bad Host.
			if code, _ := g.browse(local, func(cb *url.URL) { cb.RawQuery = "code=thecode&state=wrong" }); code != 400 {
				t.Errorf("a callback with the wrong state got %d", code)
			}
			req, _ := http.NewRequest("GET", local+"/", nil)
			req.Host = "evil.example"
			if resp, err := http.DefaultClient.Do(req); err == nil && resp.StatusCode != 404 {
				t.Errorf("a request for another host got %d", resp.StatusCode)
			}
			if code, body := g.browse(local, nil); code != 200 || !strings.Contains(body, "Go back to the terminal") {
				t.Errorf("callback: %d %s", code, body)
			}
		}()
	}
	install := &InstallApp{GitHub: gh}
	u, out := ui("ian/app\ny\n")
	// The App is already installed when the wizard looks.
	g.mu.Lock()
	g.installed = true
	g.mu.Unlock()
	if err := Run(ctx, u, st, []Step{create, install}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if st.AppID != "4242" || st.AppSlug != "symphony-ian-abc123" || st.InstallationID != "777" || st.Repo != "ian/app" {
		t.Errorf("state %+v", st)
	}
	fi, err := os.Stat(st.AppKeyFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if saved, _ := os.ReadFile(st.AppKeyFile); string(saved) != g.key {
		t.Error("the saved key differs from the one GitHub issued")
	}
	// Nothing secret in the transcript or in the state file.
	stateFile, _ := os.ReadFile(filepath.Join(st.Dir(), "setup.json"))
	for _, secret := range []string{"PRIVATE KEY", "CLIENTSECRET", "HOOKSECRET", "thecode"} {
		if strings.Contains(out.String(), secret) || strings.Contains(string(stateFile), secret) {
			t.Errorf("%q appears in the output or the state file", secret)
		}
	}
	// The manifest asks for exactly the permitted rights and no webhook.
	perms, _ := g.manifest["default_permissions"].(map[string]any)
	if len(perms) != 6 || perms["contents"] != "write" || perms["checks"] != "read" || perms["administration"] != nil ||
		perms["workflows"] != nil || g.manifest["public"] != false || g.manifest["hook_attributes"] != nil {
		t.Errorf("manifest %v", g.manifest)
	}
	if name, _ := g.manifest["name"].(string); !regexp.MustCompile(`^symphony-ian-[0-9a-f]{6}$`).MatchString(name) {
		t.Errorf("app name %q", name)
	}
	for _, want := range []string{"cannot change repository settings", "ssh -N -L"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// A second run finds everything done and changes nothing.
	u, out = ui("")
	if err := Run(ctx, u, st, []Step{create, install}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "Already done.") != 2 || g.converted != 1 {
		t.Errorf("second run:\n%s", out)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(srv.URL)
	srv.Close()
	var port int
	for _, c := range u.Port() {
		port = port*10 + int(c-'0')
	}
	return port
}

func TestCreateAppStopsWhenDeclinedOrWhenAKeyExists(t *testing.T) {
	g := newGitHub(t)
	st := state(t)
	create := &CreateApp{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, Port: freePort(t)}
	u, _ := ui("ian/app\nn\n")
	if err := create.Run(ctx, u, st); !errors.Is(err, ErrAborted) || g.converted != 0 {
		t.Fatalf("declined: %v", err)
	}
	os.WriteFile(filepath.Join(st.Dir(), keyFileName), []byte("old"), 0o600)
	u, _ = ui("ian/app\ny\n")
	if err := create.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing key: %v", err)
	}
	if saved, _ := os.ReadFile(filepath.Join(st.Dir(), keyFileName)); string(saved) != "old" {
		t.Error("an existing key file was overwritten")
	}
}

func TestCreateAppReportsARefusedExchange(t *testing.T) {
	g := newGitHub(t)
	g.convertTo = 422
	st := state(t)
	create := &CreateApp{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, Port: freePort(t), Wait: 10 * time.Second}
	create.Ready = func(local string) { go g.browse(local, nil) }
	u, _ := ui("ian/app\ny\n")
	err := create.Run(ctx, u, st)
	if err == nil || !strings.Contains(err.Error(), "status 422") || strings.Contains(err.Error(), "thecode") {
		t.Fatalf("%v", err)
	}
	if st.AppID != "" {
		t.Error("an App was recorded")
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), keyFileName)); err == nil {
		t.Error("a key file was written")
	}
}

func TestCreateAppGivesUpAfterTheWait(t *testing.T) {
	g := newGitHub(t)
	st := state(t)
	create := &CreateApp{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, Port: freePort(t), Wait: 50 * time.Millisecond}
	u, _ := ui("ian/app\ny\n")
	if err := create.Run(ctx, u, st); !errors.Is(err, ErrAborted) {
		t.Fatalf("%v", err)
	}
}

func TestInstallAppAsksUntilInstalled(t *testing.T) {
	g := newGitHub(t)
	gh := GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}
	st := state(t)
	st.Repo, st.AppID, st.AppSlug = "ian/app", "4242", "symphony-ian-abc123"
	st.AppKeyFile = filepath.Join(st.Dir(), keyFileName)
	os.WriteFile(st.AppKeyFile, []byte(g.key), 0o600)
	install := &InstallApp{GitHub: gh}
	if s, _ := install.Check(ctx, st); s.Done || !strings.Contains(s.Detail, "not installed") {
		t.Errorf("before: %+v", s)
	}
	u, out := ui("y\nn\n")
	if err := install.Run(ctx, u, st); !errors.Is(err, ErrAborted) {
		t.Fatalf("%v", err)
	}
	if strings.Count(out.String(), "/apps/symphony-ian-abc123/installations/new") != 2 || !strings.Contains(out.String(), "Only select repositories") {
		t.Errorf("instructions:\n%s", out)
	}
	g.installed = true
	u, _ = ui("")
	if err := install.Run(ctx, u, st); err != nil || st.InstallationID != "777" {
		t.Fatalf("%v %+v", err, st)
	}
	if s, _ := install.Check(ctx, st); !s.Done {
		t.Errorf("after: %+v", s)
	}
}

// installed returns a state as it is after the App steps.
func installed(t *testing.T, g *github) *State {
	t.Helper()
	st := state(t)
	st.Repo, st.AppID, st.AppSlug, st.InstallationID = "ian/app", "4242", "symphony-ian-abc123", "777"
	st.AppKeyFile = filepath.Join(st.Dir(), keyFileName)
	if err := os.WriteFile(st.AppKeyFile, []byte(g.key), 0o600); err != nil {
		t.Fatal(err)
	}
	g.installed = true
	return st
}

func TestLabelsStep(t *testing.T) {
	g := newGitHub(t)
	g.labels = map[string]bool{"status:todo": true, "bug": true}
	st := installed(t, g)
	step := &Labels{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, GOOS: "darwin"}
	if s, err := step.Check(ctx, st); err != nil || s.Done {
		t.Fatalf("%+v %v", s, err)
	}

	// Declining creates nothing.
	u, out := ui("\nn\n")
	if err := step.Run(ctx, u, st); !errors.Is(err, ErrAborted) || len(g.created) != 0 {
		t.Fatalf("%v, created %v", err, g.created)
	}
	if st.Worker != "mac" {
		t.Errorf("the default name on macOS is %q", st.Worker)
	}
	if strings.Contains(out.String(), "status:todo  ") || !strings.Contains(out.String(), "status:human review") {
		t.Errorf("the list should show only missing labels:\n%s", out)
	}

	u, _ = ui("Bad Name\nlinux\ny\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil {
		t.Fatal(err)
	}
	want := "status:in progress,status:human review,status:rework,status:blocked,status:done,run:linux"
	if strings.Join(g.created, ",") != want {
		t.Errorf("created %v", g.created)
	}
	for _, sc := range g.scopes {
		if sc != "issues:write" {
			t.Errorf("a token with %s was requested", sc)
		}
	}
	if s, _ := step.Check(ctx, st); !s.Done {
		t.Errorf("after: %+v", s)
	}
}

func TestWorkflowFileStep(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		g := newGitHub(t)
		st := installed(t, g)
		st.Worker = "linux"
		step := &WorkflowFile{GOOS: goos}
		u, out := ui("  go test ./...  \ny\n")
		if err := Run(ctx, u, st, []Step{step}); err != nil {
			t.Fatalf("%s: %v\n%s", goos, err, out)
		}
		raw, err := os.ReadFile(st.WorkflowFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"repo: ian/app", "state_source: labels", `bot_login: "symphony-ian-abc123[bot]"`,
			"allowed_authors: [ian]", "    go test ./...\n", "name: linux"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s: the file lacks %q:\n%s", goos, want, raw)
			}
		}
		if !strings.Contains(out.String(), "    tracker:") {
			t.Errorf("%s: the file was not shown before writing", goos)
		}
		if fi, _ := os.Stat(st.WorkflowFile); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", fi.Mode().Perm())
		}

		// An edited file is kept, never rewritten.
		edited := strings.Replace(string(raw), "go test ./...", "make check", 1)
		os.WriteFile(st.WorkflowFile, []byte(edited), 0o600)
		st.WorkflowFile = ""
		u, _ = ui("")
		if err := Run(ctx, u, st, []Step{step}); err != nil {
			t.Fatal(err)
		}
		if again, _ := os.ReadFile(st.WorkflowFile); string(again) != edited {
			t.Errorf("%s: an existing file was overwritten", goos)
		}

		// A broken file stops the wizard with the reason.
		os.WriteFile(st.WorkflowFile, []byte("---\ntracker: [\n---\n"), 0o600)
		if s, _ := step.Check(ctx, st); s.Done || !strings.Contains(s.Detail, "not valid") {
			t.Errorf("%s: broken file: %+v", goos, s)
		}
		st.WorkflowFile = ""
		if err := step.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "not valid") {
			t.Errorf("%s: broken file accepted: %v", goos, err)
		}
	}
}

func TestWorkflowFileNeedsAVerifyCommandAndAYes(t *testing.T) {
	g := newGitHub(t)
	st := installed(t, g)
	st.Worker = "linux"
	step := &WorkflowFile{GOOS: "linux"}
	u, out := ui("\n   \nmake check\nn\n")
	if err := step.Run(ctx, u, st); !errors.Is(err, ErrAborted) {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "a verify command is required") != 2 {
		t.Errorf("empty answers were accepted:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), workflowFileName)); err == nil {
		t.Fatal("the file was written after a no")
	}
	if err := validCommand("a\nb"); err == nil {
		t.Error("a multi-line command was accepted")
	}
}

func TestLabelsStepReportsARefusal(t *testing.T) {
	g := newGitHub(t)
	g.refuse = 403
	st := installed(t, g)
	step := &Labels{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, GOOS: "linux"}
	u, _ := ui("\ny\n")
	if err := step.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("%v", err)
	}
}
