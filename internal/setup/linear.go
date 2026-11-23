package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"github.com/iandeherdt/Maestro/internal/tracker/linear"
)

// Where the tickets are kept.
const (
	TrackerGitHub = "github"
	TrackerLinear = "linear"
)

// LinearKeyFile is where the Linear API key the service uses is kept. It
// is a secret, readable by the operator only.
func LinearKeyFile(dir string) string { return filepath.Join(dir, "linear.key") }

// ReadLinearKey reads the stored key; "" without an error when there is
// none.
func ReadLinearKey(dir string) (string, error) {
	raw, err := os.ReadFile(LinearKeyFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// WriteLinearKey stores key, readable by the operator only.
func WriteLinearKey(dir, key string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(LinearKeyFile(dir), []byte(key+"\n"), 0o600)
}

// ValidLinearKey refuses what cannot be a key.
func ValidLinearKey(key string) error {
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return errors.New("that is not an API key")
	}
	return nil
}

// UsesLinear reports whether the tickets are kept in Linear.
func (s *State) UsesLinear() bool { return s.Tracker == TrackerLinear }

// SwitchTracker sets where the tickets are kept. A settings file written
// for the other one is kept under another name, so that the next step
// writes a new one.
func SwitchTracker(st *State, kind string) (kept string, err error) {
	if kind != TrackerGitHub && kind != TrackerLinear {
		return "", fmt.Errorf("the board is %s or %s", TrackerGitHub, TrackerLinear)
	}
	was := st.Tracker
	if was == "" {
		was = TrackerGitHub
	}
	if was == kind {
		st.Tracker = kind
		return "", st.Save()
	}
	if st.WorkflowFile != "" {
		if _, err := os.Stat(st.WorkflowFile); err == nil {
			kept = strings.TrimSuffix(st.WorkflowFile, ".md") + "." + was + ".md"
			if _, err := os.Lstat(kept); err == nil {
				return "", fmt.Errorf("%s already exists; move it away first", kept)
			}
			if err := os.Rename(st.WorkflowFile, kept); err != nil {
				return "", err
			}
		}
	}
	st.Tracker, st.WorkflowFile = kind, ""
	return kept, st.Save()
}

// The states the service needs in the team, and the kind each should be
// when the operator adds it.
var linearStates = []struct{ name, kind string }{
	{"Todo", "Unstarted"},
	{"In Progress", "Started"},
	{"", "Started"}, // the hand-off state: In Review if the team has it, else Human Review
	{"Rework", "Started"},
	{"Blocked", "Started"},
	{"Done", "Completed"},
}

var emailRE = regexp.MustCompile(`^[^\s@,\[\]{}"'#]+@[^\s@,\[\]{}"'#]+\.[^\s@,\[\]{}"'#]+$`)

// LinearBoard sets up a Linear team as the board: the key the service
// uses, the team, who approves, the states and the worker label.
type LinearBoard struct {
	Endpoint string // default linear.DefaultEndpoint
	GOOS     string
	// AskKey reads the key without showing it. Default: an ordinary
	// question.
	AskKey func(ui *UI, prompt string) (string, error)
}

func (*LinearBoard) Title() string { return "Set up the Linear team" }

func (l *LinearBoard) endpoint() string {
	if l.Endpoint != "" {
		return l.Endpoint
	}
	return linear.DefaultEndpoint
}

type linearTeam struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	States struct {
		Nodes []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"nodes"`
	} `json:"states"`
	Members struct {
		Nodes []struct {
			Email string `json:"email"`
		} `json:"nodes"`
	} `json:"members"`
}

const linearTeamQuery = `query($key: String!) { viewer { email }
teams(filter: { key: { eq: $key } }) { nodes { id key name
  states(first: 100) { nodes { name type } }
  members(first: 100) { nodes { email } } } } }`

func (l *LinearBoard) team(ctx context.Context, key, teamKey string) (*linearTeam, string, error) {
	var out struct {
		Viewer struct {
			Email string `json:"email"`
		} `json:"viewer"`
		Teams struct {
			Nodes []linearTeam `json:"nodes"`
		} `json:"teams"`
	}
	if err := linear.Query(ctx, l.endpoint(), key, linearTeamQuery, map[string]any{"key": teamKey}, &out); err != nil {
		return nil, "", err
	}
	if len(out.Teams.Nodes) != 1 {
		return nil, out.Viewer.Email, fmt.Errorf("the key cannot see the Linear team %s", teamKey)
	}
	return &out.Teams.Nodes[0], out.Viewer.Email, nil
}

func (t *linearTeam) has(name string) bool {
	for _, s := range t.States.Nodes {
		if strings.EqualFold(strings.TrimSpace(s.Name), name) {
			return true
		}
	}
	return false
}

// handoff is the state a pull request waits for the operator in.
func (t *linearTeam) handoff() string {
	if t.has("In Review") {
		return "In Review"
	}
	return "Human Review"
}

func (t *linearTeam) missing() []string {
	var miss []string
	for _, s := range linearStates {
		name := s.name
		if name == "" {
			name = t.handoff()
		}
		if !t.has(name) {
			miss = append(miss, name)
		}
	}
	return miss
}

// terminal is Done and every canceled state, as the team spells them.
func (t *linearTeam) terminal() []string {
	out := []string{"Done"}
	for _, s := range t.States.Nodes {
		if s.Type == "canceled" && validStateName(s.Name) == nil {
			out = append(out, strings.TrimSpace(s.Name))
		}
	}
	return out
}

func validStateName(s string) error {
	if strings.TrimSpace(s) == "" || len(s) > 80 {
		return errors.New("not a state name")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("not a state name")
		}
	}
	return nil
}

func (t *linearTeam) member(email string) bool {
	for _, m := range t.Members.Nodes {
		if strings.EqualFold(m.Email, email) {
			return true
		}
	}
	return false
}

const linearLabelQuery = `query($name: String!) { issueLabels(filter: { name: { eqIgnoreCase: $name } }) { nodes { name team { id } } } }`

func (l *LinearBoard) hasLabel(ctx context.Context, key, teamID, name string) (bool, error) {
	var out struct {
		IssueLabels struct {
			Nodes []struct {
				Team *struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := linear.Query(ctx, l.endpoint(), key, linearLabelQuery, map[string]any{"name": name}, &out); err != nil {
		return false, err
	}
	for _, n := range out.IssueLabels.Nodes {
		// A workspace label, or one of this team.
		if n.Team == nil || n.Team.ID == teamID {
			return true, nil
		}
	}
	return false, nil
}

func (l *LinearBoard) Check(ctx context.Context, st *State) (Status, error) {
	key, err := ReadLinearKey(st.Dir())
	if err != nil {
		return Status{}, err
	}
	if key == "" {
		return Status{Detail: "The service needs a Linear API key to read and move the tickets."}, nil
	}
	if st.Worker == "" || st.LinearTeam == "" || st.LinearAuthor == "" {
		return Status{Detail: "The team, who approves tickets and this machine's name are not chosen yet."}, nil
	}
	team, _, err := l.team(ctx, key, st.LinearTeam)
	if err != nil {
		return Status{Detail: "Linear: " + err.Error()}, nil
	}
	if miss := team.missing(); len(miss) > 0 {
		return Status{Detail: "The team " + team.Key + " has no state " + strings.Join(miss, ", ") + "."}, nil
	}
	label := "run:" + st.Worker
	ok, err := l.hasLabel(ctx, key, team.ID, label)
	if err != nil {
		return Status{Detail: "Linear: " + err.Error()}, nil
	}
	if !ok {
		return Status{Detail: "The team " + team.Key + " has no label " + label + "."}, nil
	}
	return Status{Done: true, Detail: fmt.Sprintf("Team %s; %s approves; tickets come to this machine with the label %s.",
		team.Key, st.LinearAuthor, label)}, nil
}

func (l *LinearBoard) Run(ctx context.Context, ui *UI, st *State) error {
	key, err := l.key(ctx, ui, st)
	if err != nil {
		return err
	}
	goos := l.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	def := st.Worker
	if def == "" {
		def = defaultWorker(goos)
	}
	ui.Say("  Each machine that runs agents has a name. A ticket is sent to it with the label run:<name>.")
	name, err := ui.Ask("  What should this machine be called?", def, st.checkWorker)
	if err != nil {
		return err
	}
	st.Worker = name
	if err := st.Save(); err != nil {
		return err
	}
	team, me, err := l.chooseTeam(ctx, ui, st, key)
	if err != nil {
		return err
	}
	if err := l.chooseAuthor(ui, st, team, me); err != nil {
		return err
	}
	if team, err = l.states(ctx, ui, key, team); err != nil {
		return err
	}
	st.LinearHandoff, st.LinearTerminal = team.handoff(), team.terminal()
	if err := st.Save(); err != nil {
		return err
	}
	return l.label(ctx, ui, st, key, team)
}

// key returns a working key, asking for one when none is stored or the
// stored one no longer works.
func (l *LinearBoard) key(ctx context.Context, ui *UI, st *State) (string, error) {
	key, err := ReadLinearKey(st.Dir())
	if err != nil {
		return "", err
	}
	if key != "" {
		if _, _, err := linear.Who(ctx, l.endpoint(), key); err == nil {
			return key, nil
		}
		ui.Say("  Linear does not accept the stored API key any more. Make a new one.")
	}
	ui.Say("  The service reads and moves the tickets with a Linear API key. To make one:")
	ui.Say("    1. Best is a Linear user of its own for the service: what that user does never counts as")
	ui.Say("       your approval. Sign in to Linear as that user. Your own user works too.")
	ui.Say("    2. Open Settings, Security & access, Personal API keys, and create a key.")
	ui.Say("    3. Limit it to the team the tickets are in, with Read and Write. Not Admin.")
	ui.Say("  The key is kept in %s, readable by you only. The agent never sees it.", LinearKeyFile(st.Dir()))
	for {
		var k string
		if l.AskKey != nil {
			k, err = l.AskKey(ui, "  Linear API key (it is not shown): ")
		} else {
			k, err = ui.Ask("  Linear API key", "", nil)
		}
		if err != nil {
			return "", err
		}
		k = strings.TrimSpace(k)
		if err := ValidLinearKey(k); err != nil {
			ui.Say("  %v. Paste the whole key.", err)
			continue
		}
		_, email, err := linear.Who(ctx, l.endpoint(), k)
		if err != nil {
			ui.Say("  Linear did not accept it: %v", err)
			continue
		}
		if err := WriteLinearKey(st.Dir(), k); err != nil {
			return "", err
		}
		ui.Say("  Stored. The key acts as %s in Linear.", oneLine(email))
		return k, nil
	}
}

const linearTeamsQuery = `query { teams(first: 100) { nodes { key name } } }`

func (l *LinearBoard) chooseTeam(ctx context.Context, ui *UI, st *State, key string) (*linearTeam, string, error) {
	var out struct {
		Teams struct {
			Nodes []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := linear.Query(ctx, l.endpoint(), key, linearTeamsQuery, nil, &out); err != nil {
		return nil, "", err
	}
	var keys, names []string
	for _, t := range out.Teams.Nodes {
		if teamKeyRE.MatchString(t.Key) {
			keys = append(keys, t.Key)
			names = append(names, t.Key+"  "+oneLine(t.Name))
		}
	}
	var chosen string
	switch len(keys) {
	case 0:
		return nil, "", errors.New("the key can see no Linear team. Make a key that is allowed to read the team")
	case 1:
		chosen = keys[0]
		ui.Say("  The tickets are those of the team %s.", names[0])
	default:
		at := 0
		for i, k := range keys {
			if strings.EqualFold(k, st.LinearTeam) {
				at = i
			}
		}
		// The team chosen before comes first.
		keys[0], keys[at] = keys[at], keys[0]
		names[0], names[at] = names[at], names[0]
		i, err := ui.Choose("Which team holds the tickets for "+st.Repo+"?", names)
		if err != nil {
			return nil, "", err
		}
		chosen = keys[i]
	}
	st.LinearTeam = chosen
	if err := st.Save(); err != nil {
		return nil, "", err
	}
	return l.team(ctx, key, chosen)
}

var teamKeyRE = regexp.MustCompile(`^[A-Za-z0-9]{1,10}$`)

func (l *LinearBoard) chooseAuthor(ui *UI, st *State, team *linearTeam, me string) error {
	def := st.LinearAuthor
	if def == "" {
		var others []string
		for _, m := range team.Members.Nodes {
			if !strings.EqualFold(m.Email, me) && emailRE.MatchString(m.Email) {
				others = append(others, m.Email)
			}
		}
		if len(others) == 1 {
			def = others[0]
		}
	}
	ui.Say("  A ticket is only worked on when you approve it: you move it to Todo and add the label.")
	ui.Say("  Linear keeps who did that, and the service checks it.")
	for {
		email, err := ui.Ask("  Your e-mail address in Linear", def, func(s string) error {
			if !emailRE.MatchString(s) {
				return errors.New("write the e-mail address you sign in to Linear with")
			}
			if !team.member(s) {
				return fmt.Errorf("nobody on the team %s has that address", team.Key)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if strings.EqualFold(email, me) {
			ui.Say("  The API key acts as that user too. Then a ticket the service moves itself could look")
			ui.Say("  approved. It is safer to make the key as a separate Linear user for the service.")
			ok, err := ui.Confirm("  Use your own user for both anyway?")
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		}
		st.LinearAuthor = email
		return st.Save()
	}
}

// states waits until the team has every state the service uses. Adding a
// state takes a team admin, which the service's key is not, so the
// operator adds them.
func (l *LinearBoard) states(ctx context.Context, ui *UI, key string, team *linearTeam) (*linearTeam, error) {
	for {
		miss := team.missing()
		if len(miss) == 0 {
			return team, nil
		}
		ui.Say("  The team %s needs these states, which it does not have yet:", team.Key)
		for _, m := range miss {
			for _, s := range linearStates {
				if s.name == m || s.name == "" && m == team.handoff() {
					ui.Show("    %-14s in the group %s", m, s.kind)
				}
			}
		}
		ui.Say("  Add them in Linear: Settings, Teams, %s, Workflow. The names must match.", team.Key)
		if _, err := ui.Ask("  Press Enter here when they exist", "", func(string) error { return nil }); err != nil {
			return nil, err
		}
		var err error
		if team, _, err = l.team(ctx, key, team.Key); err != nil {
			return nil, err
		}
	}
}

const linearLabelCreate = `mutation($input: IssueLabelCreateInput!) { issueLabelCreate(input: $input) { success } }`

func (l *LinearBoard) label(ctx context.Context, ui *UI, st *State, key string, team *linearTeam) error {
	name := "run:" + st.Worker
	ok, err := l.hasLabel(ctx, key, team.ID, name)
	if err != nil || ok {
		return err
	}
	ui.Say("  The label %s will be created in the team %s. A ticket with it goes to this machine.", name, team.Key)
	yes, err := ui.Confirm("  Create it?")
	if err != nil {
		return err
	}
	if !yes {
		return ErrAborted
	}
	err = linear.Query(ctx, l.endpoint(), key, linearLabelCreate, map[string]any{"input": map[string]any{
		"name": name, "teamId": team.ID, "color": "#4EA7FC",
		"description": "Maestro: the machine " + st.Worker + " works on this ticket.",
	}}, nil)
	if err == nil {
		return nil
	}
	ui.Say("  Linear did not let the key create it (%s).", oneLine(err.Error()))
	ui.Say("  Create the label %s yourself: Settings, Teams, %s, Labels.", name, team.Key)
	_, err = ui.Ask("  Press Enter here when it exists", "", func(string) error { return nil })
	return err
}
