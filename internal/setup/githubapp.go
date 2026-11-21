package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/ghapp"
)

const (
	// DefaultPort is the loopback port the wizard listens on while the
	// operator creates the App in a browser.
	DefaultPort = 8976
	keyFileName = "github-app.pem"
	homepage    = "https://github.com/iandeherdt/Symphony-beads"
)

// permissions are what SPEC 15.5 allows the App, and nothing more.
var permissions = map[string]string{
	"contents": "write", "pull_requests": "write", "issues": "write",
	"checks": "read", "statuses": "read", "metadata": "read",
}

// boardPermission is added for a repository in an organization, where
// tickets are approved on a board (SPEC 12.5).
const boardPermission = "organization_projects"

// The two kinds of account GitHub reports for a repository owner.
const (
	kindUser = "User"
	kindOrg  = "Organization"
)

var (
	repoRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})$`)
	codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
	slugRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,99}$`)
)

func validRepo(s string) error {
	if !repoRE.MatchString(s) {
		return errors.New("write it as owner/name, for example your-account/your-repo")
	}
	return nil
}

// GitHub is where the wizard reaches GitHub. The zero value is github.com.
type GitHub struct {
	WebURL string // default https://github.com
	APIURL string // default https://api.github.com
	HTTP   *http.Client
}

func (g GitHub) web() string {
	if g.WebURL == "" {
		return "https://github.com"
	}
	return strings.TrimSuffix(g.WebURL, "/")
}

func (g GitHub) api() string {
	if g.APIURL == "" {
		return "https://api.github.com"
	}
	return strings.TrimSuffix(g.APIURL, "/")
}

func (g GitHub) client() *http.Client {
	hc := &http.Client{Timeout: 30 * time.Second}
	if g.HTTP != nil {
		cp := *g.HTTP
		hc = &cp
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

// ownerKind asks GitHub whether an account is a person or an organization.
// The question needs no credential.
func (g GitHub) ownerKind(ctx context.Context, owner string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api()+"/users/"+owner, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub could not be reached: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("GitHub has no account named %s. Check the spelling and run setup again", owner)
	default:
		return "", fmt.Errorf("GitHub answered with status %d when asked about the account %s. Try again in a few minutes", resp.StatusCode, owner)
	}
	var out struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Type != kindUser && out.Type != kindOrg {
		return "", fmt.Errorf("GitHub's answer about the account %s could not be read", owner)
	}
	return out.Type, nil
}

// app opens the App recorded in the state.
func (g GitHub) app(st *State) (*ghapp.App, error) {
	return ghapp.New(ghapp.Config{AppID: st.AppID, InstallationID: st.InstallationID, KeyFile: st.AppKeyFile,
		APIURL: g.APIURL, HTTP: g.HTTP})
}

// CreateApp registers the service's own GitHub App. The operator clicks
// one button on github.com; the App's ID and key come back to this machine
// directly and the key is written to a file only this user can read.
type CreateApp struct {
	GitHub GitHub
	Port   int
	// Wait bounds how long the wizard waits for the browser. Default 15 minutes.
	Wait time.Duration
	// Ready, when set, is told the local URL once the wizard listens. Tests use it.
	Ready func(url string)
}

func (*CreateApp) Title() string { return "Create the service's GitHub App" }

func (c *CreateApp) Check(_ context.Context, st *State) (Status, error) {
	if st.AppID == "" || st.AppKeyFile == "" {
		return Status{Detail: "The service needs its own GitHub identity, so that it never uses your personal access."}, nil
	}
	if _, err := c.GitHub.app(st); err != nil {
		return Status{Detail: "An App was created before, but its key cannot be used: " + err.Error()}, nil
	}
	return Status{Done: true, Detail: "App " + st.AppSlug + " (ID " + st.AppID + "), key in " + st.AppKeyFile + "."}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (c *CreateApp) Run(ctx context.Context, ui *UI, st *State) error {
	keyPath := filepath.Join(st.Dir(), keyFileName)
	if _, err := os.Lstat(keyPath); err == nil {
		return fmt.Errorf("%s already exists. If it belongs to an App you no longer use, delete that App on GitHub, remove the file and run setup again", keyPath)
	}
	repo, err := ui.Ask("  Which repository should the agents work on (owner/name)?", st.Repo, validRepo)
	if err != nil {
		return err
	}
	owner := repoRE.FindStringSubmatch(repo)[1]
	kind, err := c.GitHub.ownerKind(ctx, owner)
	if err != nil {
		return err
	}
	st.Repo, st.OwnerType = repo, kind
	if err := st.Save(); err != nil {
		return err
	}
	suffix, err := randomHex(3)
	if err != nil {
		return err
	}
	name := "symphony-" + strings.ToLower(owner)
	if len(name) > 27 {
		name = name[:27]
	}
	name += "-" + suffix // App names are unique across GitHub

	perms, newApp := permissions, c.GitHub.web()+"/settings/apps/new"
	if kind == kindOrg {
		perms = map[string]string{boardPermission: "write"}
		for k, v := range permissions {
			perms[k] = v
		}
		newApp = c.GitHub.web() + "/organizations/" + owner + "/settings/apps/new"
		ui.Say("  %s is an organization, so tickets will be approved on a board with columns.", owner)
		ui.Say("  This creates a private GitHub App named %s in the organization %s. It may:", name, owner)
		ui.Say("    - read and write code, pull requests and issues, and read check results,")
		ui.Say("      only in repositories you install it on in the next step,")
		ui.Say("    - read and change the organization's boards (GitHub calls them Projects).")
		ui.Say("  You need to be an owner of %s to create it.", owner)
	} else {
		ui.Say("  %s is a personal account, so tickets will be approved with labels.", owner)
		ui.Say("  (For a board with columns, put the repository in a free GitHub organization instead,")
		ui.Say("  and answer with that repository. GitHub gives an App no access to a personal board.)")
		ui.Say("  This creates a private GitHub App named %s on your account. It may:", name)
		ui.Say("    - read and write code, pull requests and issues, and read check results,")
		ui.Say("      only in repositories you install it on in the next step.")
	}
	ui.Say("  It cannot change repository settings, workflows or secrets, and it cannot merge.")
	ok, err := ui.Confirm("  Continue?")
	if err != nil {
		return err
	}
	if !ok {
		return ErrAborted
	}

	port := c.Port
	if port == 0 {
		port = DefaultPort
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("port %d on this machine is in use; close what uses it or run setup with --port", port)
	}
	defer ln.Close()
	port = ln.Addr().(*net.TCPAddr).Port
	local := "http://127.0.0.1:" + strconv.Itoa(port)
	state, err := randomHex(16)
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(map[string]any{
		"name": name, "url": homepage, "redirect_url": local + "/callback", "public": false,
		"description":         "Opens pull requests for approved tickets. Created by symphony setup.",
		"default_permissions": perms, "default_events": []string{},
	})
	if err != nil {
		return err
	}

	codes := make(chan string, 1)
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Only a browser pointed at this address is served, which
			// rules out another site reaching the listener by name.
			if r.Host != "127.0.0.1:"+strconv.Itoa(port) && r.Host != "localhost:"+strconv.Itoa(port) || r.Method != http.MethodGet {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+c.GitHub.web())
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			h(w, r)
		}
	}
	mux.HandleFunc("/", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `<!doctype html><title>Symphony setup</title>
<body style="font-family:sans-serif;max-width:36em;margin:3em auto">
<h1>Create the GitHub App</h1>
<p>The next page is on github.com. Check the name and press <b>Create GitHub App</b>.</p>
<form method="post" action="%s?state=%s">
<input type="hidden" name="manifest" value="%s">
<button type="submit" style="font-size:1.1em;padding:.5em 1em">Continue to GitHub</button>
</form></body>`, html.EscapeString(newApp), state, html.EscapeString(string(manifest)))
	}))
	mux.HandleFunc("/callback", guard(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || !codeRE.MatchString(q.Get("code")) {
			http.Error(w, "This link does not belong to the setup that is running.", http.StatusBadRequest)
			return
		}
		select {
		case codes <- q.Get("code"):
		default:
		}
		io.WriteString(w, `<!doctype html><title>Symphony setup</title><body style="font-family:sans-serif;max-width:36em;margin:3em auto"><h1>Done</h1><p>Go back to the terminal. You can close this tab.</p></body>`)
	}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	ui.Say("")
	ui.Say("  Open this address in a browser where you are logged in to GitHub:")
	ui.Say("      %s/", local)
	ui.Say("  If this machine has no browser, first run this on your own computer, then open the address there:")
	ui.Say("      ssh -N -L %d:127.0.0.1:%d <this-machine>", port, port)
	ui.Say("  Waiting for you to press Create on GitHub...")
	if c.Ready != nil {
		c.Ready(local)
	}

	wait := c.Wait
	if wait == 0 {
		wait = 15 * time.Minute
	}
	var code string
	select {
	case code = <-codes:
	case <-ctx.Done():
		return ErrAborted
	case <-time.After(wait):
		ui.Say("  Nothing arrived from GitHub in time.")
		return ErrAborted
	}

	// The code is exchanged once for the App's details. Nothing but the
	// code identifies the request, so it is not logged or shown.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.GitHub.api()+"/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.GitHub.client().Do(req)
	if err != nil {
		return fmt.Errorf("GitHub could not be reached to finish creating the App: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("GitHub did not hand over the new App (status %d). If an App named %s now exists under Settings, Developer settings, delete it and run setup again", resp.StatusCode, name)
	}
	var created struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		PEM  string `json:"pem"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID <= 0 || !slugRE.MatchString(created.Slug) ||
		!strings.Contains(created.PEM, "PRIVATE KEY") {
		return errors.New("GitHub's answer for the new App could not be read")
	}
	// The response also carries a client secret and a webhook secret. The
	// service uses neither, so they are dropped here and never stored.
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("the App was created but its key could not be saved: %w. Delete the App %s on GitHub and run setup again", err, created.Slug)
	}
	_, werr := f.WriteString(created.PEM)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(keyPath)
		return fmt.Errorf("the App was created but its key could not be saved: %w. Delete the App %s on GitHub and run setup again", werr, created.Slug)
	}
	st.AppID, st.AppSlug, st.AppKeyFile = strconv.FormatInt(created.ID, 10), created.Slug, keyPath
	return nil
}

// InstallApp has the operator install the App on the one repository and
// then proves the whole chain by requesting a token for it.
type InstallApp struct {
	GitHub GitHub
}

func (*InstallApp) Title() string { return "Give the App access to the repository" }

func (i *InstallApp) Check(ctx context.Context, st *State) (Status, error) {
	if st.AppID == "" || st.Repo == "" {
		return Status{Detail: "The App has to be created first."}, nil
	}
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil {
		return Status{}, errors.New("the repository recorded by setup is malformed")
	}
	if st.InstallationID == "" {
		return Status{Detail: "The App exists but is not installed on " + st.Repo + " yet."}, nil
	}
	app, err := i.GitHub.app(st)
	if err != nil {
		return Status{}, err
	}
	src, err := app.Source(ghapp.Scope{Repositories: []string{m[2]}, Permissions: map[string]string{"contents": "write", "pull_requests": "write"}})
	if err != nil {
		return Status{}, err
	}
	if _, err := src.Token(ctx); err != nil {
		return Status{Detail: "A token for " + st.Repo + " could not be obtained: " + err.Error()}, nil
	}
	return Status{Done: true, Detail: "The App can open pull requests on " + st.Repo + "."}, nil
}

func (i *InstallApp) Run(ctx context.Context, ui *UI, st *State) error {
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil || st.AppID == "" || !slugRE.MatchString(st.AppSlug) {
		return errors.New("the App has to be created first")
	}
	st.InstallationID = "" // looked up afresh
	app, err := i.GitHub.app(st)
	if err != nil {
		return err
	}
	for {
		id, err := app.FindInstallation(ctx, m[1], m[2])
		if err != nil {
			return err
		}
		if id != "" {
			st.InstallationID = id
			return nil
		}
		ui.Say("  Open this page:")
		ui.Say("      %s/apps/%s/installations/new", i.GitHub.web(), st.AppSlug)
		ui.Say("  Choose %s, then \"Only select repositories\", pick %s and press Install.", m[1], m[2])
		ui.Say("  Selecting only this repository keeps the service away from everything else you own.")
		ok, err := ui.Confirm("  Installed it?")
		if err != nil {
			return err
		}
		if !ok {
			return ErrAborted
		}
	}
}
