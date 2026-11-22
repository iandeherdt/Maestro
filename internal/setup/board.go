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

	"github.com/iandeherdt/Maestro/internal/ghapp"
)

const (
	boardTitle  = "Maestro"
	statusField = "Status"
	workerField = "Worker"
	modelField  = "Model"
)

type column struct{ Name, Color, Description string }

// columns are the default states of SPEC 5 as the columns of a board, in
// the order a ticket passes through them.
var columns = []column{
	{"Backlog", "GRAY", "Not approved yet. Nothing happens here."},
	{"Idea", "PINK", "Something bigger. An agent asks you questions and proposes a plan."},
	{"Design", "PINK", "Optional, after a plan: an agent makes a design for you to see before anything is built."},
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

// The fields are listed and picked by name here: asking GitHub for a field
// by a name the board does not have is answered with an error for the
// whole request, not with an empty value.
const boardStateQuery = `
query($owner: String!, $number: Int!) {
  organization(login: $owner) {
    projectV2(number: $number) {
      id title closed
      items(first: 1) { totalCount }
      fields(first: 100) { nodes { __typename ... on ProjectV2FieldCommon { id name } ... on ProjectV2SingleSelectField { options { name color description } } } }
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

// modelOptions are the choices of a new Model field: the names Claude Code
// itself keeps pointing at its newest models. Others can be added on the
// board.
var modelOptions = []option{
	{"fable", "PURPLE", "The most capable model. Uses the most of your subscription."},
	{"opus", "RED", "A very capable model, for hard tickets."},
	{"sonnet", "BLUE", "The everyday model."},
	{"haiku", "GREEN", "The fastest and lightest model, for small changes."},
}

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
	Name     string   `json:"name"`
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
	Fields struct {
		Nodes []selectField `json:"nodes"`
	} `json:"fields"`
	// Status, Worker and Model are picked from Fields by name; nil when
	// absent.
	Status *selectField `json:"-"`
	Worker *selectField `json:"-"`
	Model  *selectField `json:"-"`
}

// pick finds the fields the service uses, whatever their type.
func (b *boardState) pick() {
	for i := range b.Fields.Nodes {
		f := &b.Fields.Nodes[i]
		switch {
		case b.Status == nil && strings.EqualFold(f.Name, statusField):
			b.Status = f
		case b.Worker == nil && strings.EqualFold(f.Name, workerField):
			b.Worker = f
		case b.Model == nil && strings.EqualFold(f.Name, modelField):
			b.Model = f
		}
	}
}

// optionalColumns are columns a board works without. A board made before
// they existed is not held up for them; setup only says how to add them.
var optionalColumns = map[string]bool{"Design": true}

// missingColumns are the columns the board still lacks.
func (b *boardState) missingColumns() []string {
	var out []string
	for _, c := range columns {
		if !b.Status.has(c.Name) && !optionalColumns[c.Name] {
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
	err := b.graphql(ctx, st, boardStateQuery, map[string]any{"owner": m[1], "number": st.Board}, &out)
	if err != nil {
		return nil, err
	}
	if out.Organization == nil || out.Organization.ProjectV2 == nil || out.Organization.ProjectV2.ID == "" {
		return nil, nil
	}
	out.Organization.ProjectV2.pick()
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
	case board.Model == nil:
		return Status{Detail: "The board has no Model field yet, with which a card can ask for a model."}, nil
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
	name, err := ui.Ask("  What should this machine be called?", def, st.checkWorker)
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
	for {
		err := b.prepare(ctx, ui, st, board)
		if !errors.Is(err, errOtherBoard) {
			if err != nil {
				return err
			}
			break
		}
		// The board could not take this machine: choose again.
		st.Board = 0
		if err := b.choose(ctx, ui, st, m[1], m[2]); err != nil {
			return err
		}
		if err := st.Save(); err != nil {
			return err
		}
		if board, err = b.read(ctx, st); err != nil {
			return err
		}
		if board == nil {
			return fmt.Errorf("board number %d was not found in %s", st.Board, m[1])
		}
	}
	ui.Say("  The board is at %s", b.url(st))
	ui.Say("  GitHub opens a new board as a table. To see columns: open it, click the small arrow")
	ui.Say("  next to \"View 1\", and under Layout choose Board. That is needed once.")
	return nil
}

// errOtherBoard is returned by prepare when the operator wants another
// board than the one that was chosen.
var errOtherBoard = errors.New("another board")

// another asks whether to stop and change the board by hand or to choose
// another board. It returns ErrAborted or errOtherBoard.
func (b *Board) another(ui *UI, byHand string) error {
	i, err := ui.Choose("  Use this board, or another one?", []string{
		"Stop here: I will " + byHand,
		"Use another board, or create a new one",
	})
	if err != nil {
		return err
	}
	if i == 1 {
		return errOtherBoard
	}
	return ErrAborted
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
	if ui.Styled {
		// In a terminal the boards are a list to pick from.
		options, numbers := []string{"Create a new board named " + boardTitle}, []int{0}
		for _, p := range found.Organization.ProjectsV2.Nodes {
			if p != nil && !p.Closed && p.Number > 0 {
				options = append(options, fmt.Sprintf("Use board #%d  %s", p.Number, oneLine(p.Title)))
				numbers = append(numbers, p.Number)
			}
		}
		if len(options) > 1 {
			i, err := ui.Choose(fmt.Sprintf("%s already has boards. Which one should hold the tickets?", owner), options)
			if err != nil {
				return err
			}
			if numbers[i] != 0 {
				st.Board = numbers[i]
				return nil
			}
		}
	}
	open := map[int]bool{}
	for _, p := range found.Organization.ProjectsV2.Nodes {
		if p != nil && !p.Closed && p.Number > 0 && !ui.Styled {
			if len(open) == 0 {
				ui.Say("  %s already has these boards:", owner)
			}
			open[p.Number] = true
			ui.Show("    %3d  %s", p.Number, oneLine(p.Title))
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
		// GitHub lets a person create a board in an organization, but
		// refuses it to an App. So the operator makes an empty one and
		// setup fills it in.
		ui.Say("  GitHub did not let the App create the board (%s). That is normal: only a person can.", oneLine(err.Error()))
		ui.Say("  Please create an empty one yourself, it takes a minute:")
		ui.Say("    1. Open %s/orgs/%s/projects", b.GitHub.web(), owner)
		ui.Say("    2. Press New project, pick Board, name it %s and create it.", boardTitle)
		ui.Say("    3. Leave it empty. Setup adds the columns.")
		if _, err := ui.Ask("  Press Enter here when the board exists", "", func(string) error { return nil }); err != nil {
			return err
		}
		return b.choose(ctx, ui, st, owner, repo)
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
	miss := board.missingColumns()
	var optional []string
	for _, c := range columns {
		if optionalColumns[c.Name] && !board.Status.has(c.Name) {
			optional = append(optional, c.Name)
		}
	}
	if len(miss) == 0 && len(optional) > 0 && !empty {
		ui.Say("  The board has no column %s yet. It is optional: to use it, open %s, click the three dots", strings.Join(optional, ", "), b.url(st))
		ui.Say("  at the top right, then Settings, then %s, and add it as an option.", statusField)
	}
	if len(miss) > 0 || len(optional) > 0 && empty {
		if !empty {
			ui.Say("  The board already has cards, so setup does not rewrite its columns.")
			ui.Say("  Open %s, click the three dots at the top right, then Settings,", b.url(st))
			ui.Say("  then %s in the list on the left. Add these options and save:", statusField)
			ui.Say("      %s", strings.Join(miss, ", "))
			ui.Say("  Then run setup again.")
			return b.another(ui, "add them to this board by hand")
		}
		var opts []option
		for _, c := range columns {
			opts = append(opts, option(c))
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
		ui.Say("  To use this board, open %s, click the three dots at the top right, then Settings, "+
			"then %s in the list on the left. Add the option %s and save. Then run setup again.", b.url(st), workerField, st.Worker)
		return b.another(ui, "add "+st.Worker+" to this board by hand")
	}
	// A card can ask for a model. An empty field means the default from the
	// settings file, so the field is never required.
	switch {
	case board.Model == nil:
		if err := b.graphql(ctx, st, createFieldMutation, map[string]any{"project": board.ID, "name": modelField, "options": inputs(modelOptions)}, nil); err != nil {
			return fmt.Errorf("the field %s could not be created: %w", modelField, err)
		}
		ui.Say("  Field %s with the choices fable, opus, sonnet and haiku. Leave it empty on a card for the default.", modelField)
	case !board.Model.usable():
		ui.Say("  The board has a field named %s that is not a single-select field, so cards cannot ask for a model.", modelField)
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
