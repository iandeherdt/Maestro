package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBaseGuarded(t *testing.T) {
	for name, c := range map[string]struct {
		rulesStatus int
		rules       string
		branch      string
		want        bool
		fails       bool
	}{
		"a ruleset that requires a pull request": {200, `[{"type":"deletion"},{"type":"pull_request"}]`, `{"protected":false}`, true, false},
		"a ruleset without that rule":            {200, `[{"type":"deletion"}]`, `{"protected":false}`, false, false},
		"no rule at all":                         {200, `[]`, `{"protected":false}`, false, false},
		"the older branch protection":            {200, `[]`, `{"protected":true}`, true, false},
		"a GitHub without rulesets":              {404, ``, `{"protected":true}`, true, false},
		"a branch that cannot be read":           {200, `[]`, ``, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tok" {
					w.WriteHeader(401)
					return
				}
				switch r.URL.Path {
				case "/repos/ian/app/rules/branches/main":
					w.WriteHeader(c.rulesStatus)
					w.Write([]byte(c.rules))
				case "/repos/ian/app/branches/main":
					if c.branch == "" {
						w.WriteHeader(404)
						return
					}
					w.Write([]byte(c.branch))
				default:
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			got, err := baseGuarded(context.Background(), srv.URL, "ian/app", "main", "tok")
			if (err != nil) != c.fails || got != c.want {
				t.Fatalf("guarded %v, error %v", got, err)
			}
		})
	}
}

func TestUnguardedNoteSaysWhereAndWhat(t *testing.T) {
	note := unguardedNote("https://github.com", "ian/app", "main")
	for _, want := range []string{"https://github.com/ian/app/settings/rules", "target branch main", "Require a pull request before merging", "1 required approval"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not say %q:\n%s", want, note)
		}
	}
}
