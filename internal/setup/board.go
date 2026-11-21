package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/iandeherdt/Symphony-beads/internal/ghapp"
)

const (
	boardTitle  = "Symphony"
	statusField = "Status"
	workerField = "Worker"
)

type column struct{ Name, Color, Description string }

// columns are the default states of SPEC 5 as the columns of a board, in
// the order a ticket passes through them.
var columns = []column{
	{"Backlog", "GRAY", "Not approved yet. Nothing happens here."},
	{"Idea", "PINK", "Something bigger. An agent asks you questions and proposes a plan."},
	{"Planned", "PURPLE", "You approved the plan. Its tasks are created in Todo."},
	{"Todo", "GREEN", "Approved. The service may start work."},
	{"In Progress", "BLUE", "An agent is working on this."},
	{"Human Review", "YELLOW", "A pull request is open and waits for you."},
	{"Rework", "ORANGE", "You asked for changes. The agent will continue."},
	{"Blocked", "RED", "The service gave up. See the status comment on the issue."},
	{"Done", "GRAY", "Finished."},
}

var authorRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func validAuthor(s string) error {
	if !authorRE.MatchString(s) {
		return errors.New("write your GitHub user name, the one in the address of your profile page")
	}
	return nil
}

// Tickets sets up where a ticket is approved: a board for a repository in
// an organization, labels for one on a personal account.
type Tickets struct {
	GitHub GitHub
	GOOS   string
}

func (*Tickets) Title() string { return "Set up where tickets are approved" }

func (t *Tickets) step(st *State) Step {
	if st.OwnerType == kindOrg {
		return &Board{GitHub: t.GitHub, GOOS: t.GOOS}
	}
	return &Labels{GitHub: t.GitHub, GOOS: t.GOOS}
}

func (t *Tickets) Check(ctx context.Context, st *State) (Status, error) {
	return t.step(st).Check(ctx, st)
}

func (t *Tickets) Run(ctx context.Context, ui *UI, st *State) error {
	return t.step(st).Run(ctx, ui, st)
}

// Board creates the board a ticket is approved on, or prepares one that
// exists: the columns, and the field that says which machine takes a card.
type Board struct {
	GitHub GitHub
	GOOS   string
}

func (*Board) Title() string { return "Create the board" }

// ---- GraphQL --------------------------------------------------------------

const boardsQuery = `
query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) { id owner { id } }
  organization(login: $owner) { projectsV2(first: 50) { nodes { number title closed } } }
}`

const boardStateQuery = `
query($owner: String!, $number: Int!, $status: String!, $worker: String!) {
  organization(login: $owner) {
    projectV2(number: $number) {
      id title closed
      items(first: 1) { totalCount }
      status: field(name: $status) { __typename ... on ProjectV2SingleSelectField { id options { name color description } } }
      worker: field(name: $worker) { __typename ... on ProjectV2SingleSelectField { id options { name color description } } }
    }
  }
}`

const createBoardMutation = `
mutation($owner: ID!, $repo: ID!, $title: String!) {
  createProjectV2(input: {ownerId: $owner, repositoryId: $repo, title: $title}) { projectV2 { number } }
}`

const setOptionsMutation = `
mutation($field: ID!, $options: [ProjectV2SingleSelectFieldOptionInput!]) {
  updateProjectV2Field(input: {fieldId: $field, singleSelectOptions: $options}) { clientMutationId }
}`

const createFieldMutation = `
mutation($project: ID!, $name: String!, $options: [ProjectV2SingleSelectFieldOptionInput!]) {
  createProjectV2Field(input: {projectId: $project, dataType: SINGLE_SELECT, name: $name, singleSelectOptions: $options}) { clientMutationId }
}`

type option struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type selectField struct {
	Typename string   `json:"__typename"`
	ID       string   `json:"id"`
	Options  []option `json:"options"`
}

func (f *selectField) usable() bool {
	return f != nil && f.Typename == "ProjectV2SingleSelectField" && f.ID != ""
}

func (f *selectField) has(name string) bool {
	if f == nil {
		return false
	}
	for _, o := range f.Options {
		if strings.EqualFold(strings.TrimSpace(o.Name), name) {
			return true
		}
	}
	return false
}

type boardState struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Closed bool   `json:"closed"`
	Items  struct {
		TotalCount int `json:"totalCount"`
	} `json:"items"`
	Status *selectField `json:"status"`
	Worker *selectField `json:"worker"`
}

// missingColumns are the columns the board still lacks.
func (b *boardState) missingColumns() []string {
	var out []string
	for _, c := range columns {
		if !b.Status.has(c.Name) {
			out = append(out, c.Name)
		}
	}
	return out
}

// graphql makes one request with a token for the boards of the
// organization and for reading the one repository.
func (b *Board) graphql(ctx context.Context, st *State, query string, vars map[string]any, out any) error {
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil {
		return errors.New("the repository recorded by setup is malformed")
	}
	app, err := b.GitHub.app(st)
	if err != nil {
		return err
	}
	src, err := app.Source(ghapp.Scope{Repositories: []string{m[2]},
		Permissions: map[string]string{boardPermission: "write", "contents": "read", "issues": "read"}})
	if err != nil {
		return err
	}
	tok, err := src.Token(ctx)
	if err != nil {
		var ge *ghapp.Error
		if errors.As(err, &ge) && ge.Status == http.StatusUnprocessableEntity {
			return fmt.Errorf("the App %s may not use the boards of %s. It was created before boards were supported, or its permissions were changed.\n"+
				"  Open %s/organizations/%s/settings/apps/%s/permissions, set Organization permissions > Projects to Read and write,\n"+
				"  save, accept the new permission in the email GitHub sends, and run setup again", st.AppSlug, m[1], b.GitHub.web(), m[1], st.AppSlug)
		}
		return err
	}
	raw, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.GitHub.api()+"/graphql", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.GitHub.client().Do(req)
	if err != nil {
		return fmt.Errorf("GitHub could not be reached: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered with status %d", resp.StatusCode)
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return errors.New("GitHub's answer could not be read")
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GitHub refused: %s", oneLine(res.Errors[0].Message))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return errors.New("GitHub's answer could not be read")
	}
	return nil
}

// oneLine makes text from GitHub safe to print on a terminal.
func oneLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 300 {
			break
		}
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (b *Board) read(ctx context.Context, st *State) (*boardState, error) {
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil {
		return nil, errors.New("the repository recorded by setup is malformed")
	}
	var out struct {
		Organization *struct {
			ProjectV2 *boardState `json:"projectV2"`
		} `json:"organization"`
	}
	err := b.graphql(ctx, st, boardStateQuery, map[string]any{"owner": m[1], "number": st.Board, "status": statusField, "worker": workerField}, &out)
	if err != nil {
		return nil, err
	}
	if out.Organization == nil || out.Organization.ProjectV2 == nil || out.Organization.ProjectV2.ID == "" {
		return nil, nil
	}
	return out.Organization.ProjectV2, nil
}

func (b *Board) url(st *State) string {
	m := repoRE.FindStringSubmatch(st.Repo)
	return b.GitHub.web() + "/orgs/" + m[1] + "/projects/" + strconv.Itoa(st.Board)
}

func (b *Board) Check(ctx context.Context, st *State) (Status, error) {
	if st.InstallationID == "" || st.Repo == "" {
		return Status{Detail: "The App has to be installed first."}, nil
	}
	if st.Worker == "" || st.Author == "" || st.Board == 0 {
		return Status{Detail: "A ticket is approved by moving its card to the Todo column, so the organization needs a board."}, nil
	}
	board, err := b.read(ctx, st)
	if err != nil {
		return Status{}, err
	}
	switch {
	case board == nil || board.Closed:
		return Status{Detail: "Board number " + strconv.Itoa(st.Board) + " no longer exists or is closed."}, nil
	case !board.Status.usable() || len(board.missingColumns()) > 0:
		return Status{Detail: "The board lacks columns: " + strings.Join(board.missingColumns(), ", ") + "."}, nil
	case !board.Worker.usable() || !board.Worker.has(st.Worker):
		return Status{Detail: "The board's Worker field does not list " + st.Worker + " yet."}, nil
	}
	return Status{Done: true, Detail: "The board " + b.url(st) + " has the columns and knows the worker " + st.Worker + "."}, nil
}

func inputs(opts []option) []map[string]string {
	out := []map[string]string{}
	for _, o := range opts {
		out = append(out, map[string]string{"name": o.Name, "color": o.Color, "description": o.Description})
	}
	return out
}

func (b *Board) Run(ctx context.Context, ui *UI, st *State) error {
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil || st.InstallationID == "" {
		return errors.New("the App has to be installed first")
	}
	goos := b.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	def := st.Worker
	if def == "" {
		def = defaultWorker(goos)
	}
	ui.Say("  Each machine that runs agents has a name. A card says which machine takes it.")
	name, err := ui.Ask("  What should this machine be called?", def, validWorker)
	if err != nil {
		return err
	}
	st.Worker = name
	ui.Say("  Only issues that you wrote yourself are ever worked on, whatever column they are in.")
	author, err := ui.Ask("  What is your GitHub user name?", st.Author, validAuthor)
	if err != nil {
		return err
	}
	if kind, err := b.GitHub.ownerKind(ctx, author); err != nil {
		return err
	} else if kind != kindUser {
		return fmt.Errorf("%s is an organization, not a person. Run setup again and give your own user name", author)
	}
	st.Author = author
	if err := st.Save(); err != nil {
		return err
	}

	if st.Board != 0 {
		// A board chosen in an earlier run that has since gone.
		if board, err := b.read(ctx, st); err != nil {
			return err
		} else if board == nil || board.Closed {
			st.Board = 0
		}
	}
	if st.Board == 0 {
		if err := b.choose(ctx, ui, st, m[1], m[2]); err != nil {
			return err
		}
		if err := st.Save(); err != nil {
			return err
		}
	}
	board, err := b.read(ctx, st)
	if err != nil {
		return err
	}
	if board == nil {
		return fmt.Errorf("board number %d was not found in %s", st.Board, m[1])
	}
	if err := b.prepare(ctx, ui, st, board); err != nil {
		return err
	}
	ui.Say("  The board is at %s", b.url(st))
	ui.Say("  GitHub opens a new board as a table. To see columns: open it, click the small arrow")
	ui.Say("  next to \"View 1\", and under Layout choose Board. That is needed once.")
	return nil
}

// choose finds a board to use or creates one, and records its number.
func (b *Board) choose(ctx context.Context, ui *UI, st *State, owner, repo string) error {
	var found struct {
		Repository *struct {
			ID    string `json:"id"`
			Owner struct {
				ID string `json:"id"`
			} `json:"owner"`
		} `json:"repository"`
		Organization *struct {
			ProjectsV2 struct {
				Nodes []*struct {
					Number int    `json:"number"`
					Title  string `json:"title"`
					Closed bool   `json:"closed"`
				} `json:"nodes"`
			} `json:"projectsV2"`
		} `json:"organization"`
	}
	if err := b.graphql(ctx, st, boardsQuery, map[string]any{"owner": owner, "repo": repo}, &found); err != nil {
		return err
	}
	if found.Repository == nil || found.Repository.ID == "" || found.Repository.Owner.ID == "" || found.Organization == nil {
		return fmt.Errorf("%s could not be read with the App. Check that the App is installed on it", st.Repo)
	}
	open := map[int]bool{}
	for _, p := range found.Organization.ProjectsV2.Nodes {
		if p != nil && !p.Closed && p.Number > 0 {
			if len(open) == 0 {
				ui.Say("  %s already has these boards:", owner)
			}
			open[p.Number] = true
			ui.Say("    %3d  %s", p.Number, oneLine(p.Title))
		}
	}
	if len(open) > 0 {
		ans, err := ui.Ask("  Type the number of the board to use, or just press Enter to create a new one", "", func(s string) error {
			if s == "" {
				return nil
			}
			if n, err := strconv.Atoi(s); err != nil || !open[n] {
				return errors.New("type one of the numbers listed above, or nothing")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if ans != "" {
			st.Board, _ = strconv.Atoi(ans)
			return nil
		}
	}
	ui.Say("  A board named %q will be created in %s and linked to %s.", boardTitle, owner, st.Repo)
	ok, err := ui.Confirm("  Create it?")
	if err != nil {
		return err
	}
	if !ok {
		return ErrAborted
	}
	var created struct {
		CreateProjectV2 struct {
			ProjectV2 *struct {
				Number int `json:"number"`
			} `json:"projectV2"`
		} `json:"createProjectV2"`
	}
	err = b.graphql(ctx, st, createBoardMutation, map[string]any{"owner": found.Repository.Owner.ID, "repo": found.Repository.ID, "title": boardTitle}, &created)
	if err != nil || created.CreateProjectV2.ProjectV2 == nil || created.CreateProjectV2.ProjectV2.Number <= 0 {
		if err == nil {
			err = errors.New("GitHub did not say which board it created")
		}
		return fmt.Errorf("the board could not be created: %w.\n"+
			"  You can create it by hand instead: open %s/orgs/%s/projects, press New project, pick Board, name it %s.\n"+
			"  Then run setup again and choose it from the list", err, b.GitHub.web(), owner, boardTitle)
	}
	st.Board = created.CreateProjectV2.ProjectV2.Number
	return nil
}

// prepare adds the columns and the worker to the board. GitHub can only
// replace the whole list of options of a field, and what that does to
// cards that already use an option is not documented. So the lists are
// only rewritten while the board is empty; after that the operator adds
// what is missing by hand, which is safe.
func (b *Board) prepare(ctx context.Context, ui *UI, st *State, board *boardState) error {
	empty := board.Items.TotalCount == 0
	if !board.Status.usable() {
		return fmt.Errorf("the board has no field named %s to hold the columns. Create a new board with setup instead", statusField)
	}
	if miss := board.missingColumns(); len(miss) > 0 {
		if !empty {
			ui.Say("  The board already has cards, so setup does not rewrite its columns.")
			ui.Say("  Open %s, click the three dots at the top right, then Settings,", b.url(st))
			ui.Say("  then %s in the list on the left. Add these options and save:", statusField)
			ui.Say("      %s", strings.Join(miss, ", "))
			ui.Say("  Then run setup again.")
			return ErrAborted
		}
		var opts []option
		for _, c := range columns {
			opts = append(opts, option{c.Name, c.Color, c.Description})
		}
		for _, o := range board.Status.Options { // columns of your own are kept
			known := false
			for _, c := range columns {
				known = known || strings.EqualFold(strings.TrimSpace(o.Name), c.Name)
			}
			if !known {
				opts = append(opts, o)
			}
		}
		if err := b.graphql(ctx, st, setOptionsMutation, map[string]any{"field": board.Status.ID, "options": inputs(opts)}, nil); err != nil {
			return fmt.Errorf("the columns could not be created: %w", err)
		}
		ui.Say("  Columns: %s.", strings.Join(columnNames(), ", "))
	}
	mine := option{st.Worker, "PURPLE", "The machine named " + st.Worker + " takes this card."}
	switch {
	case board.Worker == nil:
		if err := b.graphql(ctx, st, createFieldMutation, map[string]any{"project": board.ID, "name": workerField, "options": inputs([]option{mine})}, nil); err != nil {
			return fmt.Errorf("the field %s could not be created: %w", workerField, err)
		}
		ui.Say("  Field %s with the choice %s.", workerField, st.Worker)
	case !board.Worker.usable():
		return fmt.Errorf("the board has a field named %s that is not a single-select field. Rename it on the board and run setup again", workerField)
	case !board.Worker.has(st.Worker) && empty:
		if err := b.graphql(ctx, st, setOptionsMutation, map[string]any{"field": board.Worker.ID, "options": inputs(append(board.Worker.Options, mine))}, nil); err != nil {
			return fmt.Errorf("the worker could not be added to the board: %w", err)
		}
		ui.Say("  Field %s now also has the choice %s.", workerField, st.Worker)
	case !board.Worker.has(st.Worker):
		ui.Say("  The board already has cards, so setup does not rewrite the choices of the %s field.", workerField)
		ui.Say("  Open %s, click the three dots at the top right, then Settings,", b.url(st))
		ui.Say("  then %s in the list on the left. Add the option %s and save.", workerField, st.Worker)
		ui.Say("  Then run setup again.")
		return ErrAborted
	}
	return nil
}

func columnNames() []string {
	var out []string
	for _, c := range columns {
		out = append(out, c.Name)
	}
	return out
}
