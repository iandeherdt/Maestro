package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"

	"github.com/iandeherdt/Symphony-beads/internal/ghapp"
)

type label struct{ name, color, description string }

// stateLabels are the default states of SPEC 5 under the default prefix.
var stateLabels = []label{
	{"status:todo", "0e8a16", "Approved. The service may start work."},
	{"status:in progress", "1d76db", "An agent is working on this."},
	{"status:human review", "fbca04", "A pull request is open and waits for the owner."},
	{"status:rework", "d93f0b", "Changes were requested. The agent will continue."},
	{"status:blocked", "b60205", "The service gave up. See the status comment."},
	{"status:done", "c5def5", "Finished."},
}

var workerRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func validWorker(s string) error {
	if !workerRE.MatchString(s) {
		return errors.New("use a short lowercase name such as linux or mac")
	}
	return nil
}

func defaultWorker(goos string) string {
	if goos == "darwin" {
		return "mac"
	}
	return "linux"
}

// Labels creates the labels a ticket is approved and tracked with.
type Labels struct {
	GitHub GitHub
	GOOS   string
}

func (*Labels) Title() string { return "Create the labels in the repository" }

func (l *Labels) wanted(st *State) []label {
	return append(append([]label{}, stateLabels...),
		label{"run:" + st.Worker, "5319e7", "Route this ticket to the " + st.Worker + " worker."})
}

// call makes one request with a token that can only touch issues of the
// one repository.
func (l *Labels) call(ctx context.Context, st *State, method, path string, body any) (int, error) {
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil {
		return 0, errors.New("the repository recorded by setup is malformed")
	}
	app, err := l.GitHub.app(st)
	if err != nil {
		return 0, err
	}
	src, err := app.Source(ghapp.Scope{Repositories: []string{m[2]}, Permissions: map[string]string{"issues": "write"}})
	if err != nil {
		return 0, err
	}
	tok, err := src.Token(ctx)
	if err != nil {
		return 0, err
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.GitHub.api()+"/repos/"+m[1]+"/"+m[2]+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.GitHub.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitHub could not be reached: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}

func (l *Labels) missing(ctx context.Context, st *State) ([]label, error) {
	var out []label
	for _, lb := range l.wanted(st) {
		code, err := l.call(ctx, st, http.MethodGet, "/labels/"+url.PathEscape(lb.name), nil)
		if err != nil {
			return nil, err
		}
		switch code {
		case http.StatusOK:
		case http.StatusNotFound:
			out = append(out, lb)
		default:
			return nil, fmt.Errorf("GitHub answered with status %d when asked about the label %q", code, lb.name)
		}
	}
	return out, nil
}

func (l *Labels) Check(ctx context.Context, st *State) (Status, error) {
	if st.InstallationID == "" || st.Repo == "" {
		return Status{Detail: "The App has to be installed first."}, nil
	}
	if st.Worker == "" {
		return Status{Detail: "A ticket is approved by adding labels, so the repository needs them."}, nil
	}
	miss, err := l.missing(ctx, st)
	if err != nil {
		return Status{}, err
	}
	if len(miss) > 0 {
		return Status{Detail: fmt.Sprintf("%d label(s) are missing in %s.", len(miss), st.Repo)}, nil
	}
	return Status{Done: true, Detail: "The status labels and run:" + st.Worker + " exist in " + st.Repo + "."}, nil
}

func (l *Labels) Run(ctx context.Context, ui *UI, st *State) error {
	if st.InstallationID == "" {
		return errors.New("the App has to be installed first")
	}
	goos := l.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	def := st.Worker
	if def == "" {
		def = defaultWorker(goos)
	}
	ui.Say("  Each machine that runs agents has a name. Tickets are sent to it with the label run:<name>.")
	name, err := ui.Ask("  What should this machine be called?", def, validWorker)
	if err != nil {
		return err
	}
	st.Worker = name
	if err := st.Save(); err != nil {
		return err
	}
	miss, err := l.missing(ctx, st)
	if err != nil {
		return err
	}
	if len(miss) == 0 {
		return nil
	}
	ui.Say("  These labels will be created in %s:", st.Repo)
	for _, lb := range miss {
		ui.Say("    %-22s %s", lb.name, lb.description)
	}
	ok, err := ui.Confirm("  Create them?")
	if err != nil {
		return err
	}
	if !ok {
		return ErrAborted
	}
	for _, lb := range miss {
		code, err := l.call(ctx, st, http.MethodPost, "/labels",
			map[string]string{"name": lb.name, "color": lb.color, "description": lb.description})
		if err != nil {
			return err
		}
		// 422 means it exists already, for instance created a moment ago by hand.
		if code != http.StatusCreated && code != http.StatusUnprocessableEntity {
			return fmt.Errorf("the label %q could not be created (status %d)", lb.name, code)
		}
	}
	return nil
}
