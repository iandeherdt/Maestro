package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/iandeherdt/Maestro/internal/gitlab"
	"github.com/iandeherdt/Maestro/internal/setup"
)

const gitlabUsage = `usage:
  maestro gitlab          whether a GitLab token is stored, whom it acts as and when it expires
  maestro gitlab token    store a new project access token, such as after revoking the old one
  maestro gitlab remove   delete the stored token

The token is a project access token of the project, with the role Developer
and the scopes api and write_repository. It is kept in the setup folder,
readable by you only, and never reaches the agent. The service renews it by
itself 30 days before it expires. ` + "`maestro setup --host gitlab`" + ` makes the first one.`

// runGitLab is `maestro gitlab`.
func runGitLab(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, _, rest, ok := parseWords("gitlab", args, stderr, true)
	if !ok {
		return 2
	}
	if dir == "" {
		found, err := configDir()
		if err != nil {
			return report(stderr, explain("no configuration directory was found for this user, pass --dir: %v", err))
		}
		dir = found
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return report(stderr, err)
	}
	st, err := setup.LoadState(dir)
	if err != nil {
		return report(stderr, err)
	}
	if !st.UsesGitLab() {
		fmt.Fprintln(stdout, "This project's code is on GitHub. For GitLab: "+mention("maestro setup --host gitlab"))
		return 0
	}
	ctx, stop := interruptible()
	defer stop()
	switch {
	case len(rest) == 0:
		tok, err := gitlab.ReadToken(dir)
		if err != nil || tok == "" {
			fmt.Fprintln(stdout, "No GitLab token is stored. Store one with: "+mention("maestro gitlab token"))
			return 0
		}
		c := &gitlab.Client{Web: st.HostURL, Token: tok}
		u, err := c.Who(ctx)
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "A GitLab token is stored in %s. It acts as %s on %s.\n", gitlab.TokenFile(dir), plain(u.Username, 100), st.Repo)
		if info, err := c.Self(ctx); err == nil && !info.Expires().IsZero() {
			fmt.Fprintf(stdout, "It expires on %s; the service renews it 30 days before.\n", info.ExpiresAt)
		}
		return 0
	case len(rest) == 1 && rest[0] == "remove":
		if err := os.Remove(gitlab.TokenFile(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "The GitLab token is removed from this machine. Revoke it in the project's settings too.")
		return 0
	case len(rest) == 1 && rest[0] == "token":
		r := bufio.NewReader(stdin)
		tok, err := askSecret(r, stdin, stdout, "Project access token (it is not shown): ")
		if err != nil {
			return report(stderr, err)
		}
		tok = strings.TrimSpace(tok)
		if gitlab.ValidToken(tok) != nil {
			fmt.Fprintln(stderr, "That is not a token. Nothing was stored.")
			return 1
		}
		c := &gitlab.Client{Web: st.HostURL, Token: tok}
		u, err := c.Who(ctx)
		if err == nil && !strings.EqualFold(u.Username, st.GitLabBot) && !u.Bot {
			err = fmt.Errorf("the token belongs to %s, a person; make a project access token", plain(u.Username, 100))
		}
		var p gitlab.Project
		if err == nil {
			p, err = c.Project(ctx, st.Repo)
		}
		if err == nil {
			var role int
			if role, err = c.Role(ctx, p.ID, u.ID); err == nil && (role >= gitlab.Maintainer || role < gitlab.Developer) {
				err = errors.New("the token's role must be Developer")
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "Nothing was stored.")
			return report(stderr, err)
		}
		if err := gitlab.WriteToken(dir, tok); err != nil {
			return report(stderr, err)
		}
		if !strings.EqualFold(u.Username, st.GitLabBot) || p.ID != st.GitLabProject {
			// A new token has a new bot user: the merge requests of the
			// old one are no longer the service's.
			st.GitLabBot, st.GitLabProject = u.Username, p.ID
			if err := st.Save(); err != nil {
				return report(stderr, err)
			}
			fmt.Fprintf(stdout, "Note: the new token acts as %s. Open merge requests by the old token's user are left alone.\n", u.Username)
		}
		fmt.Fprintf(stdout, "Stored. The token acts as %s on %s.\n", plain(u.Username, 100), st.Repo)
		fmt.Fprintln(stdout, "Restart the service to use it: maestro service install, or stop and start maestro run.")
		return 0
	}
	fmt.Fprintln(stderr, gitlabUsage)
	return 2
}
