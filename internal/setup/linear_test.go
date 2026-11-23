package setup

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeLinear is just enough of Linear's API for the setup step.
type fakeLinear struct {
	mu      sync.Mutex
	srv     *httptest.Server
	key     string
	states  []string
	labels  []string
	refuse  bool // label creation is not allowed
	noAdmin bool // state creation is not allowed
	created []string
}

func newLinear(t *testing.T) *fakeLinear {
	f := &fakeLinear{key: "lin_api_good", states: []string{"Backlog", "Todo", "In Progress", "In Review", "Done", "Canceled"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != f.key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	reply := func(data any) { json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	viewer := map[string]any{"id": "u-bot", "email": "maestro@example.com"}
	switch q := req.Query; {
	case strings.Contains(q, "workflowStateCreate"):
		if f.noAdmin {
			json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "Forbidden"}}})
			return
		}
		in := req.Variables["input"].(map[string]any)
		f.states = append(f.states, in["name"].(string))
		f.created = append(f.created, "state:"+in["name"].(string)+":"+in["type"].(string))
		reply(map[string]any{"workflowStateCreate": map[string]any{"success": true}})
	case strings.Contains(q, "issueLabelCreate"):
		if f.refuse {
			json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "Forbidden"}}})
			return
		}
		in := req.Variables["input"].(map[string]any)
		f.created = append(f.created, in["name"].(string)+"@"+in["teamId"].(string))
		f.labels = append(f.labels, in["name"].(string))
		reply(map[string]any{"issueLabelCreate": map[string]any{"success": true}})
	case strings.Contains(q, "issueLabels"):
		var nodes []any
		for _, l := range f.labels {
			if strings.EqualFold(l, req.Variables["name"].(string)) {
				nodes = append(nodes, map[string]any{"name": l, "team": map[string]any{"id": "t-eng"}})
			}
		}
		reply(map[string]any{"issueLabels": map[string]any{"nodes": nodes}})
	case strings.Contains(q, "filter: { key"):
		var teams []any
		if req.Variables["key"] == "ENG" {
			var states []any
			for _, s := range f.states {
				typ := "started"
				if s == "Canceled" {
					typ = "canceled"
				}
				states = append(states, map[string]any{"name": s, "type": typ})
			}
			teams = append(teams, map[string]any{"id": "t-eng", "key": "ENG", "name": "Engineering",
				"states":  map[string]any{"nodes": states},
				"members": map[string]any{"nodes": []any{map[string]any{"email": "ian@example.com"}, map[string]any{"email": "maestro@example.com"}}}})
		}
		reply(map[string]any{"viewer": viewer, "teams": map[string]any{"nodes": teams}})
	case strings.Contains(q, "teams(first"):
		reply(map[string]any{"teams": map[string]any{"nodes": []any{
			map[string]any{"key": "OPS", "name": "Operations"}, map[string]any{"key": "ENG", "name": "Engineering"}}}})
	case strings.Contains(q, "viewer"):
		reply(map[string]any{"viewer": viewer})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestLinearSetup(t *testing.T) {
	f := newLinear(t)
	f.noAdmin = true
	g := newGitHub(t)
	st := installed(t, g)
	step := &Tickets{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, GOOS: "linux", Linear: LinearBoard{Endpoint: f.srv.URL}}

	// Linear, a refused key then a good one, the machine's name, the
	// second team, a wrong and a right e-mail, Enter after adding the
	// states, and yes to the label.
	answers := "2\nlin_api_bad\nlin_api_good\n\n2\nbob@example.com\nian@example.com\ny\n"
	u, out := ui(answers)
	err := step.Run(ctx, u, st)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("the states are missing, so the step waits: %v\n%s", err, out)
	}
	if !st.UsesLinear() || st.LinearTeam != "ENG" || st.LinearAuthor != "ian@example.com" || st.Worker != "linux" {
		t.Fatalf("state %+v", st)
	}
	for _, want := range []string{"did not accept", "nobody on the team ENG", "Rework", "Blocked", "did not let the key create them", "Settings, Teams, ENG, Workflow"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if key, _ := ReadLinearKey(st.Dir()); key != "lin_api_good" {
		t.Errorf("stored %q", key)
	}
	if fi, _ := os.Stat(LinearKeyFile(st.Dir())); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %o", fi.Mode().Perm())
	}
	if s, _ := step.Check(ctx, st); s.Done || !strings.Contains(s.Detail, "Rework") {
		t.Errorf("check with states missing: %+v", s)
	}

	// The operator adds the states; now only the label is left.
	f.states = append(f.states, "Rework", "Blocked", "Idea", "Planned", "Design")
	u, out = ui("\n\nian@example.com\ny\n")
	if err := step.Run(ctx, u, st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(f.created, ",") != "run:linux@t-eng" {
		t.Errorf("created %v", f.created)
	}
	if st.LinearHandoff != "In Review" || strings.Join(st.LinearTerminal, ",") != "Done,Canceled" {
		t.Errorf("handoff %q terminal %v", st.LinearHandoff, st.LinearTerminal)
	}
	if s, err := step.Check(ctx, st); err != nil || !s.Done {
		t.Fatalf("after: %+v %v", s, err)
	}

	// The workflow file it leads to is valid.
	wf := &WorkflowFile{GOOS: "linux", LookPath: func(string) (string, error) { return "/usr/bin/x", nil }}
	u, out = ui("\ny\ny\n")
	if err := Run(ctx, u, st, []Step{wf}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(st.WorkflowFile)
	for _, want := range []string{"kind: linear", "team_key: ENG", `allowed_authors: ["ian@example.com"]`,
		`terminal_states: ["Done", "Canceled"]`, `handoff_state: "In Review"`, "repo: ian/app"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the file lacks %q:\n%s", want, raw)
		}
	}
}

func TestLinearSetupWithTheServiceAsTheAuthorAndARefusedLabel(t *testing.T) {
	f := newLinear(t)
	f.states = append(f.states, "Rework", "Blocked", "Idea", "Planned", "Design")
	f.refuse = true
	g := newGitHub(t)
	st := installed(t, g)
	st.Tracker = TrackerLinear
	if err := WriteLinearKey(st.Dir(), "lin_api_good"); err != nil {
		t.Fatal(err)
	}
	step := &LinearBoard{Endpoint: f.srv.URL, GOOS: "linux"}
	// The key's own user as the author: refused once, then accepted. The
	// label cannot be made by the key, so the operator makes it.
	u, out := ui("\n2\nmaestro@example.com\nn\nmaestro@example.com\ny\ny\n\n")
	if err := step.Run(ctx, u, st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.LinearAuthor != "maestro@example.com" || !strings.Contains(out.String(), "separate Linear user") ||
		!strings.Contains(out.String(), "Create the label run:linux yourself") {
		t.Errorf("author %q\n%s", st.LinearAuthor, out)
	}
}

func TestSwitchTracker(t *testing.T) {
	st := state(t)
	st.WorkflowFile = st.Dir() + "/WORKFLOW.md"
	os.WriteFile(st.WorkflowFile, []byte("x"), 0o600)
	kept, err := SwitchTracker(st, TrackerLinear)
	if err != nil || !strings.HasSuffix(kept, "WORKFLOW.github.md") || st.WorkflowFile != "" || !st.UsesLinear() {
		t.Fatalf("%q %v %+v", kept, err, st)
	}
	if kept, err := SwitchTracker(st, TrackerLinear); err != nil || kept != "" {
		t.Errorf("the same board again: %q %v", kept, err)
	}
	if _, err := SwitchTracker(st, "jira"); err == nil {
		t.Error("an unknown board was accepted")
	}
}

func TestLinearSetupCreatesTheStatesWhenAllowed(t *testing.T) {
	f := newLinear(t)
	g := newGitHub(t)
	st := installed(t, g)
	st.Tracker = TrackerLinear
	if err := WriteLinearKey(st.Dir(), "lin_api_good"); err != nil {
		t.Fatal(err)
	}
	step := &LinearBoard{Endpoint: f.srv.URL, GOOS: "linux"}
	u, out := ui("\n2\nian@example.com\ny\ny\n")
	if err := step.Run(ctx, u, st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "state:Rework:started,state:Blocked:started,state:Idea:backlog,state:Planned:backlog,state:Design:backlog,run:linux@t-eng"
	if strings.Join(f.created, ",") != want {
		t.Errorf("created %v", f.created)
	}
	if strings.Contains(out.String(), "Settings, Teams") {
		t.Errorf("asked to add states by hand although they were created:\n%s", out)
	}
	if s, err := step.Check(ctx, st); err != nil || !s.Done {
		t.Errorf("after: %+v %v", s, err)
	}
}
