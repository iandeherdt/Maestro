package gitea

import (
	"os"
	"strings"
	"testing"
)

func TestUsable(t *testing.T) {
	repo := func(admin, push bool) Repo {
		r := Repo{FullName: "team/app"}
		r.Owner.Login = "team"
		r.Permissions.Admin, r.Permissions.Push = admin, push
		return r
	}
	bot := User{ID: 5, Login: "maestro-bot"}
	if why := Usable(bot, repo(false, true), "ian"); why != "" {
		t.Errorf("the service's account was refused: %s", why)
	}
	for name, c := range map[string]struct {
		u    User
		r    Repo
		want string
	}{
		"site admin":  {User{Login: "root", Admin: true}, repo(false, true), "site administrator"},
		"repo admin":  {bot, repo(true, true), "administers"},
		"owner":       {User{Login: "team"}, repo(false, true), "administers"},
		"own token":   {User{Login: "Ian"}, repo(false, true), "your own"},
		"cannot push": {bot, repo(false, false), "cannot push"},
	} {
		if why := Usable(c.u, c.r, "ian"); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q", name, why)
		}
	}
}

func TestWebAndToken(t *testing.T) {
	for web, ok := range map[string]bool{"https://git.example.com": true, "https://example.com/git/": true,
		"": false, "http://git.example.com": false, "http://127.0.0.1:3000": true, "https://u:p@git.example.com": false} {
		if _, err := CheckWeb(web); (err == nil) != ok {
			t.Errorf("%q: %v", web, err)
		}
	}
	dir := t.TempDir()
	if tok, err := ReadToken(dir); tok != "" || err != nil {
		t.Fatalf("no token: %q %v", tok, err)
	}
	if err := WriteToken(dir, "abc 123"); err == nil {
		t.Error("a token with a space was stored")
	}
	if err := WriteToken(dir, "abc123"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := ReadToken(dir); tok != "abc123" {
		t.Errorf("read %q", tok)
	}
	if fi, _ := os.Stat(TokenFile(dir)); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
}
