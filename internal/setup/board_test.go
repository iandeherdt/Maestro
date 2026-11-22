package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	tracker "github.com/iandeherdt/Maestro/internal/tracker/github"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

type fakeBoard struct {
	number     int
	title      string
	closed     bool
	items      int
	status     []option
	worker     []option
	hasWorker  bool
	workerKind string
	model      []option
	hasModel   bool
	modelKind  string
}

func names(opts []option) string {
	var out []string
	for _, o := range opts {
		out = append(out, o.Name)
	}
	return strings.Join(out, ",")
}

func (g *github) graphql(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	answer := func(data any) { json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	fail := func(msg string) {
		json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": msg}}})
	}
	if r.Header.Get("Authorization") != "Bearer ghs_x" {
		w.WriteHeader(401)
		return
	}
	str := func(k string) string { s, _ := in.Variables[k].(string); return s }
	options := func() []option {
		raw, _ := json.Marshal(in.Variables["options"])
		var opts []option
		json.Unmarshal(raw, &opts)
		for _, o := range opts {
			if o.Name == "" || o.Color == "" || o.Description == "" {
				g.t.Errorf("an option without name, color or description: %+v", o)
			}
		}
		return opts
	}
	field := func(name string, opts []option, id, kind string) any {
		if kind == "" {
			kind = "ProjectV2SingleSelectField"
		}
		return map[string]any{"__typename": kind, "id": id, "name": name, "options": opts}
	}
	if g.gqlRefuse != "" && strings.Contains(in.Query, g.gqlRefuse) {
		fail("Resource not accessible\x1b[31m by integration")
		return
	}
	switch {
	case strings.Contains(in.Query, "createProjectV2("):
		g.mutations = append(g.mutations, "board")
		if str("owner") != "O_org" || str("repo") != "R_repo" || str("title") != "Maestro" {
			fail("bad input")
			return
		}
		b := &fakeBoard{number: len(g.boards) + 1, title: str("title"),
			status: []option{{"Todo", "GREEN", ""}, {"In Progress", "YELLOW", ""}, {"Done", "PURPLE", ""}}}
		g.boards = append(g.boards, b)
		answer(map[string]any{"createProjectV2": map[string]any{"projectV2": map[string]int{"number": b.number}}})
	case strings.Contains(in.Query, "updateProjectV2Field("):
		for _, b := range g.boards {
			switch str("field") {
			case "F_status_" + b.title:
				g.mutations = append(g.mutations, "columns")
				b.status = options()
			case "F_worker_" + b.title:
				g.mutations = append(g.mutations, "workers")
				b.worker = options()
			}
		}
		answer(map[string]any{"updateProjectV2Field": map[string]any{"clientMutationId": nil}})
	case strings.Contains(in.Query, "createProjectV2Field("):
		g.mutations = append(g.mutations, "field")
		for _, b := range g.boards {
			if str("project") == "PVT_"+b.title && str("name") == "Worker" && !b.hasWorker {
				b.hasWorker, b.worker = true, options()
			}
			if str("project") == "PVT_"+b.title && str("name") == "Model" && !b.hasModel {
				b.hasModel, b.model = true, options()
			}
		}
		answer(map[string]any{"createProjectV2Field": map[string]any{"clientMutationId": nil}})
	case strings.Contains(in.Query, "projectsV2("):
		var nodes []any
		for _, b := range g.boards {
			nodes = append(nodes, map[string]any{"number": b.number, "title": b.title, "closed": b.closed})
		}
		answer(map[string]any{"repository": map[string]any{"id": "R_repo", "owner": map[string]string{"id": "O_org"}},
			"organization": map[string]any{"projectsV2": map[string]any{"nodes": nodes}}})
	case strings.Contains(in.Query, "projectV2(number:"):
		n, _ := in.Variables["number"].(float64)
		var project any
		for _, b := range g.boards {
			if b.number == int(n) {
				// As on github.com: the fields are listed, with others between them.
				fields := []any{map[string]any{"__typename": "ProjectV2Field", "id": "F_title", "name": "Title"},
					field("Status", b.status, "F_status_"+b.title, "")}
				if b.hasWorker {
					fields = append(fields, field("Worker", b.worker, "F_worker_"+b.title, b.workerKind))
				}
				if b.hasModel {
					fields = append(fields, field("Model", b.model, "F_model_"+b.title, b.modelKind))
				}
				project = map[string]any{"id": "PVT_" + b.title, "title": b.title, "closed": b.closed,
					"items": map[string]int{"totalCount": b.items}, "fields": map[string]any{"nodes": fields}}
			}
		}
		answer(map[string]any{"organization": map[string]any{"projectV2": project}})
	default:
		w.WriteHeader(400)
	}
}

func onOrg(t *testing.T, g *github) (*State, *Board) {
	t.Helper()
	st := installed(t, g)
	st.OwnerType = kindOrg
	return st, &Board{GitHub: GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}, GOOS: "linux"}
}

var allColumns = "Backlog,Idea,Design,Planned,Todo,In Progress,Human Review,Rework,Blocked,Done"

func TestBoardStepCreatesTheBoard(t *testing.T) {
	g := newGitHub(t)
	st, step := onOrg(t, g)
	if s, err := step.Check(ctx, st); err != nil || s.Done {
		t.Fatalf("%+v %v", s, err)
	}
	// Declining creates nothing, but remembers the answers.
	u, _ := ui("\nian\nn\n")
	if err := step.Run(ctx, u, st); !errors.Is(err, ErrAborted) || len(g.mutations) != 0 || st.Worker != "linux" || st.Author != "ian" {
		t.Fatalf("%v, mutations %v, state %+v", err, g.mutations, st)
	}

	u, out := ui("\n\ny\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b := g.boards[0]
	if st.Board != 1 || names(b.status) != allColumns || names(b.worker) != "linux" {
		t.Fatalf("board %d, columns %q, workers %q", st.Board, names(b.status), names(b.worker))
	}
	if strings.Join(g.mutations, ",") != "board,columns,field,field" || names(b.model) != "fable,opus,sonnet,haiku" {
		t.Errorf("mutations %v", g.mutations)
	}
	for _, want := range []string{"/orgs/ian/projects/1", "Layout choose Board", "Only issues that you wrote"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	seen := map[string]bool{}
	for _, sc := range g.scopes {
		seen[sc] = true
	}
	if !seen["organization_projects:write"] || seen["issues:write"] || seen["contents:write"] || seen["pull_requests:write"] {
		t.Errorf("token scopes %v", g.scopes)
	}

	// A second run changes nothing.
	u, out = ui("")
	if err := Run(ctx, u, st, []Step{step}); err != nil || !strings.Contains(out.String(), "Already done.") || len(g.mutations) != 4 {
		t.Fatalf("second run: %v %v\n%s", err, g.mutations, out)
	}
	// A board that was closed in the meantime is replaced.
	b.closed = true
	if s, _ := step.Check(ctx, st); s.Done {
		t.Fatal("a closed board counts as done")
	}
	u, _ = ui("\n\ny\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil || st.Board != 2 {
		t.Fatalf("after closing: board %d, %v", st.Board, err)
	}
}

func TestBoardStepUsesAnEmptyExistingBoard(t *testing.T) {
	g := newGitHub(t)
	g.boards = []*fakeBoard{
		{number: 3, title: "Old", closed: true},
		{number: 4, title: "Mine", hasWorker: true, worker: []option{{"mac", "BLUE", "The Mac."}},
			status: []option{{"todo", "GREEN", "x"}, {"Ideas", "PINK", "Loose thoughts."}}},
	}
	st, step := onOrg(t, g)
	u, out := ui("server\nian\n3\n9\n4\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b := g.boards[1]
	if st.Board != 4 || names(b.status) != allColumns+",Ideas" || names(b.worker) != "mac,server" {
		t.Fatalf("board %d, columns %q, workers %q", st.Board, names(b.status), names(b.worker))
	}
	if strings.Contains(out.String(), "  3  Old") || !strings.Contains(out.String(), "  4  Mine") || strings.Contains(strings.Join(g.mutations, ","), "board") {
		t.Errorf("mutations %v\n%s", g.mutations, out)
	}
}

func TestBoardStepNeverRewritesABoardWithCards(t *testing.T) {
	g := newGitHub(t)
	full := []option{}
	for _, c := range columns {
		full = append(full, option(c))
	}
	g.boards = []*fakeBoard{
		{number: 1, title: "Partial", items: 2, status: full[:3], hasModel: true},
		{number: 2, title: "Shared", items: 5, status: full, hasWorker: true, worker: []option{{"mac", "BLUE", "x"}}, hasModel: true},
		{number: 3, title: "Odd", status: full, hasWorker: true, workerKind: "ProjectV2Field"},
	}
	for pick, want := range map[string]string{
		"1": "Planned, Todo, In Progress, Human Review, Rework, Blocked, Done",
		"2": "Add the option linux",
	} {
		st, step := onOrg(t, g)
		u, out := ui("\nian\n" + pick + "\n")
		if err := Run(ctx, u, st, []Step{step}); !errors.Is(err, ErrAborted) || len(g.mutations) != 0 {
			t.Fatalf("board %s: %v, mutations %v", pick, err, g.mutations)
		}
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "run setup again") {
			t.Errorf("board %s:\n%s", pick, out)
		}
		// Once the operator has added what was missing, setup finishes.
		g.boards[0].status, g.boards[0].hasWorker, g.boards[0].worker = full, true, []option{{"Linux", "BLUE", "x"}}
		g.boards[1].worker = append(g.boards[1].worker, option{"linux", "BLUE", "x"})
		u, out = ui("\n\n")
		if err := Run(ctx, u, st, []Step{step}); err != nil || len(g.mutations) != 0 {
			t.Fatalf("board %s after the fix: %v\n%s", pick, err, out)
		}
		g.boards[0].status, g.boards[0].hasWorker = full[:3], false
		g.boards[1].worker = g.boards[1].worker[:1]
	}
	// A board from before cards could ask for a model gets the field. That
	// adds a field and rewrites nothing, so cards do not stand in the way.
	g.boards[1].hasModel = false
	g.boards[1].worker = append(g.boards[1].worker, option{"linux", "BLUE", "x"})
	st2, step2 := onOrg(t, g)
	u2, out2 := ui("\nian\n2\n")
	if err := Run(ctx, u2, st2, []Step{step2}); err != nil || strings.Join(g.mutations, ",") != "field" || names(g.boards[1].model) != "fable,opus,sonnet,haiku" {
		t.Fatalf("adding the Model field: %v, mutations %v\n%s", err, g.mutations, out2)
	}
	if s, _ := step2.Check(ctx, st2); !s.Done {
		t.Errorf("not done after the Model field was added: %s", s.Detail)
	}
	g.mutations = nil
	st, step := onOrg(t, g)
	u, _ := ui("\nian\n3\n")
	if err := step.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "not a single-select field") {
		t.Fatalf("a Worker field of another kind: %v", err)
	}
}

func TestBoardStepExplainsRefusals(t *testing.T) {
	g := newGitHub(t)
	st, step := onOrg(t, g)

	u, _ := ui("\nacme\n")
	if err := step.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "is an organization, not a person") {
		t.Fatalf("an organization as the author: %v", err)
	}
	u, _ = ui("\nnobody\n")
	if err := step.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "no account named nobody") {
		t.Fatalf("an unknown author: %v", err)
	}

	g.gqlRefuse = "createProjectV2("
	// GitHub refuses an App that creates a board. Setup says how to make
	// one by hand, waits, and then uses it.
	u, out := ui("\nian\ny\n")
	err := step.Run(ctx, u, st)
	for _, want := range []string{"only a person can", "/orgs/ian/projects", "Press Enter here"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("a refused creation lacks %q:\n%s", want, out)
		}
	}
	if !errors.Is(err, ErrAborted) || strings.Contains(out.String(), "\x1b") || st.Board != 0 {
		t.Fatalf("a refused creation: %v", err)
	}
	g.boards = []*fakeBoard{{number: 6, title: "Maestro", status: []option{{"Todo", "GREEN", "x"}}}}
	u, out = ui("\nian\n\ny\n\n6\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil || st.Board != 6 || names(g.boards[0].status) != allColumns {
		t.Fatalf("after creating it by hand: board %d, %v\n%s", st.Board, err, out)
	}
	g.boards = nil

	// An App from before boards has no right to them.
	g.gqlRefuse, g.noBoards = "", true
	st.Worker, st.Author, st.Board = "linux", "ian", 1
	if _, err := step.Check(ctx, st); err == nil || !strings.Contains(err.Error(), "Projects to Read and write") {
		t.Fatalf("an App without the permission: %v", err)
	}
}

func TestTicketsPicksBoardOrLabels(t *testing.T) {
	g := newGitHub(t)
	gh := GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}
	st := installed(t, g)
	step := &Tickets{GitHub: gh, GOOS: "linux"}
	// A state from before boards, and a personal account, use labels.
	for _, kind := range []string{"", kindUser} {
		st.OwnerType = kind
		if s, err := step.Check(ctx, st); err != nil || !strings.Contains(s.Detail, "labels") {
			t.Errorf("%q: %+v %v", kind, s, err)
		}
	}
	st.OwnerType = kindOrg
	if s, err := step.Check(ctx, st); err != nil || !strings.Contains(s.Detail, "board") {
		t.Errorf("organization: %+v %v", s, err)
	}
	u, _ := ui("\nian\ny\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil || st.Board != 1 || len(g.created) != 0 {
		t.Fatalf("board %d, labels %v, %v", st.Board, g.created, err)
	}
}

func TestCreateAppInAnOrganization(t *testing.T) {
	g := newGitHub(t)
	gh := GitHub{WebURL: g.srv.URL, APIURL: g.srv.URL}
	st := state(t)
	create := &CreateApp{GitHub: gh, Port: freePort(t), Wait: 10 * time.Second}
	create.Ready = func(local string) { go g.browse(local, nil) }
	u, out := ui("acme/app\ny\n")
	if err := Run(ctx, u, st, []Step{create}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	g.mu.Lock()
	perms, _ := g.manifest["default_permissions"].(map[string]any)
	path := g.formPath
	g.mu.Unlock()
	if st.OwnerType != kindOrg || path != "/organizations/acme/settings/apps/new" || len(perms) != 7 ||
		perms["organization_projects"] != "write" || perms["administration"] != nil || perms["members"] != nil {
		t.Errorf("owner type %q, form %q, permissions %v", st.OwnerType, path, perms)
	}
	for _, want := range []string{"acme is an organization", "boards", "owner of acme"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	install := &InstallApp{GitHub: gh}
	st.InstallationID = ""
	u, out = ui("n\n")
	if err := install.Run(ctx, u, st); !errors.Is(err, ErrAborted) || !strings.Contains(out.String(), "Choose acme,") {
		t.Errorf("%v\n%s", err, out)
	}

	st = state(t)
	u, _ = ui("nobody/app\n")
	if err := create.Run(ctx, u, st); err == nil || !strings.Contains(err.Error(), "no account named nobody") || st.Repo != "" {
		t.Fatalf("an unknown account: %v", err)
	}
}

func TestWorkflowFileForABoard(t *testing.T) {
	g := newGitHub(t)
	st, _ := onOrg(t, g)
	step := &WorkflowFile{GOOS: "linux", LookPath: func(string) (string, error) { return "/usr/bin/x", nil }}
	st.Worker = "linux"
	if err := step.Run(ctx, nil, st); err == nil {
		t.Fatal("the file was written without a board")
	}
	st.Author, st.Board = "ian-himself", 12
	u, out := ui("\ny\ny\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(st.WorkflowFile)
	for _, want := range []string{"state_source: project\n    project:\n      number: 12\n", "allowed_authors: [ian-himself]", "repo: ian/app"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the file lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "labels") {
		t.Errorf("labels in a board file:\n%s", raw)
	}
	// The board adapter accepts the file as written.
	def, err := workflow.Load(st.WorkflowFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Build(def, config.Options{Env: os.Getenv, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.New(cfg, tracker.Options{Token: func(context.Context) (string, error) { return "x", nil }}); err != nil {
		t.Fatalf("the adapter refuses the file: %v", err)
	}
}

func TestABoardWithCardsWorksWithoutTheDesignColumn(t *testing.T) {
	g := newGitHub(t)
	var old []option
	for _, c := range columns {
		if c.Name != "Design" {
			old = append(old, option(c))
		}
	}
	g.boards = []*fakeBoard{{number: 1, title: "Old", items: 3, status: old, hasWorker: true, worker: []option{{"linux", "BLUE", "x"}}, hasModel: true}}
	st, step := onOrg(t, g)
	u, out := ui("\nian\n1\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil || len(g.mutations) != 0 {
		t.Fatalf("%v, mutations %v\n%s", err, g.mutations, out)
	}
	if !strings.Contains(out.String(), "no column Design yet. It is optional") {
		t.Errorf("no hint about the Design column:\n%s", out)
	}
	if s, _ := step.Check(ctx, st); !s.Done {
		t.Errorf("not done without the optional column: %s", s.Detail)
	}
}

// A board that cannot take this machine can be swapped for a new one in
// the same run.
func TestBoardStepOffersAnotherBoard(t *testing.T) {
	g := newGitHub(t)
	full := []option{}
	for _, c := range columns {
		full = append(full, option(c))
	}
	g.boards = []*fakeBoard{{number: 1, title: "Shared", items: 5, status: full, hasWorker: true, worker: []option{{"mac", "BLUE", "x"}}, hasModel: true}}
	st, step := onOrg(t, g)
	// Pick board 1, then "another board", then press Enter to create one.
	u, out := ui("\nian\n1\n2\n\ny\n")
	if err := Run(ctx, u, st, []Step{step}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.Board == 1 || st.Board == 0 || len(g.mutations) == 0 {
		t.Fatalf("board %d, mutations %v\n%s", st.Board, g.mutations, out)
	}
}
