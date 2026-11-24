package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/gitlab"
)

// gl is a stand-in for GitLab with one project, group/app, and three
// tokens: the project's bot as a Developer, the bot as a Maintainer, and a
// person's own token.
func gl(t *testing.T) *httptest.Server {
	users := map[string]map[string]any{
		"glpat-bot":   {"id": 5, "username": "project_7_bot_x", "bot": true},
		"glpat-maint": {"id": 6, "username": "project_7_bot_y", "bot": true},
		"glpat-me":    {"id": 8, "username": "ian", "bot": false},
	}
	roles := map[float64]int{5: 30, 6: 40, 8: 30}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if u == nil {
			w.WriteHeader(401)
			return
		}
		enc := json.NewEncoder(w)
		switch p := r.URL.EscapedPath(); {
		case p == "/api/v4/user":
			enc.Encode(u)
		case p == "/api/v4/projects/group%2Fapp":
			enc.Encode(map[string]any{"id": 7, "path_with_namespace": "group/app", "default_branch": "main"})
		case strings.HasPrefix(p, "/api/v4/projects/7/members/all/"):
			id := float64(u["id"].(int))
			enc.Encode(map[string]any{"access_level": roles[id]})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGitLabSetup(t *testing.T) {
	srv := gl(t)
	st, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := SwitchHost(st, HostGitLab); err != nil {
		t.Fatal(err)
	}
	if !st.UsesGitLab() || !st.UsesLinear() {
		t.Fatalf("state %+v", st)
	}
	for _, s := range []Step{&CreateApp{}, &InstallApp{}} {
		if got, _ := s.Check(ctx, st); !got.Done {
			t.Errorf("%s is needed with GitLab: %+v", s.Title(), got)
		}
	}
	step := &GitLabProject{}
	if s, _ := step.Check(ctx, st); s.Done {
		t.Fatal("done before it ran")
	}
	// The instance, a wrong path then the project, a bad token, a
	// Maintainer's, a person's, then the bot's, and the reviewer.
	answers := srv.URL + "\nnot a path\ngroup/app\nglpat-wrong\nglpat-maint\nglpat-me\nglpat-bot\n@ian\n"
	u, out := ui(answers)
	if err := step.Run(ctx, u, st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"does not accept the token", "Maintainer or Owner", "a person, not to a project's bot", "Stored."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if st.Repo != "group/app" || st.GitLabBot != "project_7_bot_x" || st.GitLabProject != 7 || st.Reviewer != "ian" || st.HostURL != srv.URL {
		t.Errorf("state %+v", st)
	}
	if tok, _ := gitlab.ReadToken(st.Dir()); tok != "glpat-bot" {
		t.Errorf("stored %q", tok)
	}
	if fi, _ := os.Stat(gitlab.TokenFile(st.Dir())); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	if s, err := step.Check(ctx, st); err != nil || !s.Done {
		t.Fatalf("check: %+v %v", s, err)
	}

	// The settings file names the host and the reviewer.
	st.Worker, st.LinearTeam, st.LinearAuthor, st.LinearHandoff = "linux", "ENG", "ian@example.com", "In Review"
	wf := &WorkflowFile{GOOS: "linux", LookPath: func(string) (string, error) { return "/usr/bin/x", nil }}
	u, out = ui("\ny\ny\n")
	if err := Run(ctx, u, st, []Step{wf}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(st.WorkflowFile)
	for _, want := range []string{"repo: group/app", "host: gitlab", "host_url: \"" + srv.URL + "\"", `allowed_commenters: ["ian"]`, "kind: linear"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the file lacks %q:\n%s", want, raw)
		}
	}
	if !strings.Contains(out.String(), "merge requests") {
		t.Errorf("the questions speak of pull requests:\n%s", out)
	}

	// Back to GitHub forgets the GitLab project.
	if err := SwitchHost(st, HostGitHub); err != nil || st.UsesGitLab() || st.Repo != "" || st.GitLabBot != "" {
		t.Errorf("back to GitHub: %+v %v", st, err)
	}
	if s, _ := step.Check(ctx, st); !s.Done {
		t.Error("the GitLab step is needed with GitHub")
	}
}
