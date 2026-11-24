package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/gitea"
)

// tea is a stand-in for Gitea with one repository, team/app, and four
// tokens: the service's account with Write, an account that administers
// the repository, a site administrator, and the reviewer's own.
func tea(t *testing.T) *httptest.Server {
	users := map[string]map[string]any{
		"bot":   {"id": 5, "login": "maestro-bot"},
		"admin": {"id": 6, "login": "helper"},
		"site":  {"id": 7, "login": "root", "is_admin": true},
		"me":    {"id": 8, "login": "ian"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := users[strings.TrimPrefix(r.Header.Get("Authorization"), "token ")]
		if u == nil {
			w.WriteHeader(401)
			return
		}
		enc := json.NewEncoder(w)
		switch r.URL.EscapedPath() {
		case "/api/v1/user":
			enc.Encode(u)
		case "/api/v1/repos/team/app":
			perm := map[string]bool{"push": true, "pull": true, "admin": u["login"] == "helper"}
			enc.Encode(map[string]any{"id": 3, "full_name": "team/app", "default_branch": "main",
				"owner": map[string]any{"login": "team"}, "permissions": perm})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGiteaSetup(t *testing.T) {
	srv := tea(t)
	st, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := SwitchHost(st, "forgejo"); err != nil {
		t.Fatal(err)
	}
	if !st.UsesGitea() || !st.UsesLinear() || st.HostName() != HostGitea {
		t.Fatalf("state %+v", st)
	}
	for _, s := range []Step{&CreateApp{}, &InstallApp{}, &GitLabProject{}} {
		if got, _ := s.Check(ctx, st); !got.Done {
			t.Errorf("%s is needed with Gitea: %+v", s.Title(), got)
		}
	}
	step := &GiteaRepo{}
	if s, _ := step.Check(ctx, st); s.Done {
		t.Fatal("done before it ran")
	}
	// No address, then the instance; the repository; the reviewer; then a
	// bad token, an administrator's, a site administrator's, the
	// reviewer's own, and the service's.
	answers := "\n" + srv.URL + "\nteam/app\n@ian\nwrong\nadmin\nsite\nme\nbot\n"
	u, out := ui(answers)
	if err := step.Run(ctx, u, st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"does not accept the token", "administers team/app", "site administrator", "is your own", "Stored."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if st.Repo != "team/app" || st.GiteaBot != "maestro-bot" || st.Reviewer != "ian" || st.HostURL != srv.URL {
		t.Errorf("state %+v", st)
	}
	if tok, _ := gitea.ReadToken(st.Dir()); tok != "bot" {
		t.Errorf("stored %q", tok)
	}
	if fi, _ := os.Stat(gitea.TokenFile(st.Dir())); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	if s, err := step.Check(ctx, st); err != nil || !s.Done {
		t.Fatalf("check: %+v %v", s, err)
	}

	st.Worker, st.LinearTeam, st.LinearAuthor, st.LinearHandoff = "linux", "ENG", "ian@example.com", "In Review"
	wf := &WorkflowFile{GOOS: "linux", LookPath: func(string) (string, error) { return "/usr/bin/x", nil }}
	u, out = ui("\ny\ny\n")
	if err := Run(ctx, u, st, []Step{wf}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(st.WorkflowFile)
	for _, want := range []string{"repo: team/app", "host: gitea", "host_url: \"" + srv.URL + "\"", `allowed_commenters: ["ian"]`, "kind: linear"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the file lacks %q:\n%s", want, raw)
		}
	}

	if err := SwitchHost(st, HostGitHub); err != nil || st.UsesGitea() || st.Repo != "" || st.GiteaBot != "" {
		t.Errorf("back to GitHub: %+v %v", st, err)
	}
}
