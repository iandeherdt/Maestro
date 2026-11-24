package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func TestTokenFile(t *testing.T) {
	dir := t.TempDir()
	if tok, err := ReadToken(dir); tok != "" || err != nil {
		t.Fatalf("none: %q %v", tok, err)
	}
	if err := WriteToken(dir, "glpat-one two"); err == nil {
		t.Error("a token with a space was stored")
	}
	if err := WriteToken(dir, "glpat-abc"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(TokenFile(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi.Mode(), err)
	}
	if tok, _ := ReadToken(dir); tok != "glpat-abc" {
		t.Errorf("read %q", tok)
	}
}

func TestCheckWeb(t *testing.T) {
	for in, want := range map[string]string{"": Default, "https://git.example.org/": "https://git.example.org",
		"https://example.org/gitlab": "https://example.org/gitlab"} {
		if got, err := CheckWeb(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://git.example.org", "https://u:p@example.org", "https://example.org/?a=b", "git.example.org"} {
		if _, err := CheckWeb(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

type fake struct {
	token, expires string
	rotated        int
	projectRotate  bool
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(401)
		return
	}
	enc := json.NewEncoder(w)
	switch r.Method + " " + r.URL.EscapedPath() {
	case "GET /api/v4/user":
		enc.Encode(map[string]any{"id": 5, "username": "project_7_bot_x", "bot": true})
	case "GET /api/v4/projects/group%2Fapp":
		enc.Encode(map[string]any{"id": 7, "path_with_namespace": "group/app", "default_branch": "main"})
	case "GET /api/v4/projects/7/members/all/5":
		enc.Encode(map[string]any{"access_level": 30})
	case "GET /api/v4/projects/7/protected_branches/main":
		enc.Encode(map[string]any{"allow_force_push": false,
			"push_access_levels":  []map[string]any{{"access_level": 0}},
			"merge_access_levels": []map[string]any{{"access_level": 40}}})
	case "GET /api/v4/personal_access_tokens/self":
		enc.Encode(map[string]any{"name": "maestro", "scopes": []string{"api", "write_repository"}, "expires_at": f.expires, "active": true})
	case "POST /api/v4/projects/7/access_tokens/self/rotate":
		if !f.projectRotate {
			w.WriteHeader(404)
			return
		}
		fallthrough
	case "POST /api/v4/personal_access_tokens/self/rotate":
		f.rotated++
		f.token = "glpat-new"
		enc.Encode(map[string]any{"token": "glpat-new", "expires_at": "2027-11-24"})
	default:
		w.WriteHeader(404)
	}
}

func TestClient(t *testing.T) {
	f := &fake{token: "glpat-abc", expires: "2027-01-01"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	c := &Client{Web: srv.URL, Token: "glpat-abc"}
	u, err := c.Who(ctx)
	if err != nil || u.Username != "project_7_bot_x" || !u.Bot {
		t.Fatalf("who: %+v %v", u, err)
	}
	p, err := c.Project(ctx, "group/app")
	if err != nil || p.ID != 7 || p.DefaultBranch != "main" {
		t.Fatalf("project: %+v %v", p, err)
	}
	if r, err := c.Role(ctx, 7, 5); err != nil || r != Developer {
		t.Errorf("role: %d %v", r, err)
	}
	pr, err := c.Protected(ctx, 7, "main")
	if err != nil || !pr.Protected || pr.Push != 0 || pr.Merge != Maintainer || pr.AllowForcePush {
		t.Errorf("protection: %+v %v", pr, err)
	}
	if pr, err := c.Protected(ctx, 7, "dev"); err != nil || pr.Protected {
		t.Errorf("unprotected: %+v %v", pr, err)
	}
	bad := &Client{Web: srv.URL, Token: "wrong"}
	if _, err := bad.Who(ctx); err == nil || !strings.Contains(err.Error(), "does not accept the token") {
		t.Errorf("refused: %v", err)
	}
}

func TestSourceRenewsBeforeExpiry(t *testing.T) {
	for _, projectRotate := range []bool{true, false} {
		dir := t.TempDir()
		WriteToken(dir, "glpat-abc")
		now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
		f := &fake{token: "glpat-abc", expires: "2027-06-01", projectRotate: projectRotate}
		srv := httptest.NewServer(http.HandlerFunc(f.serve))
		s := &Source{Dir: dir, Web: srv.URL, ProjectID: 7, Now: func() time.Time { return now }}
		if tok, err := s.Token(ctx); err != nil || tok != "glpat-abc" || f.rotated != 0 {
			t.Fatalf("far from expiry: %q %v %d", tok, err, f.rotated)
		}
		// Close to expiry, a day later: renewed, and stored.
		f.expires = "2026-12-10"
		now = now.Add(25 * time.Hour)
		if tok, err := s.Token(ctx); err != nil || tok != "glpat-new" || f.rotated != 1 {
			t.Fatalf("renewal: %q %v %d", tok, err, f.rotated)
		}
		if stored, _ := ReadToken(dir); stored != "glpat-new" {
			t.Errorf("stored %q", stored)
		}
		// Within the day, it is not asked again.
		if tok, _ := s.Token(ctx); tok != "glpat-new" || f.rotated != 1 {
			t.Errorf("again: %q %d", tok, f.rotated)
		}
		srv.Close()
	}
}
