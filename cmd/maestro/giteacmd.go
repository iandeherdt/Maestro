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

	"github.com/iandeherdt/Maestro/internal/gitea"
	"github.com/iandeherdt/Maestro/internal/setup"
)

const giteaUsage = `usage:
  maestro gitea          whether a Gitea token is stored and whom it acts as
  maestro gitea token    store a new token, such as after deleting the old one
  maestro gitea remove   delete the stored token

The token belongs to the service's own account on your Gitea or Forgejo,
which has Write on the repository and administers nothing. It has the
permissions repository and issue on Read and write, and user on Read. It is
kept in the setup folder, readable by you only, and never reaches the agent.
` + "`maestro setup --host gitea`" + ` makes the first one.`

// runGitea is `maestro gitea`.
func runGitea(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, _, rest, ok := parseWords("gitea", args, stderr, true)
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
	if !st.UsesGitea() {
		fmt.Fprintln(stdout, "This project's code is on "+st.HostLabel()+". For Gitea or Forgejo: "+mention("maestro setup --host gitea"))
		return 0
	}
	ctx, stop := interruptible()
	defer stop()
	switch {
	case len(rest) == 0:
		tok, err := gitea.ReadToken(dir)
		if err != nil || tok == "" {
			fmt.Fprintln(stdout, "No Gitea token is stored. Store one with: "+mention("maestro gitea token"))
			return 0
		}
		u, err := (&gitea.Client{Web: st.HostURL, Token: tok}).Who(ctx)
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "A Gitea token is stored in %s. It acts as %s on %s at %s.\n", gitea.TokenFile(dir), plain(u.Login, 100), st.Repo, st.HostURL)
		return 0
	case len(rest) == 1 && rest[0] == "remove":
		if err := os.Remove(gitea.TokenFile(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "The Gitea token is removed from this machine. Delete it in the account's settings too.")
		return 0
	case len(rest) == 1 && rest[0] == "token":
		r := bufio.NewReader(stdin)
		tok, err := askSecret(r, stdin, stdout, "The service account's token (it is not shown): ")
		if err != nil {
			return report(stderr, err)
		}
		tok = strings.TrimSpace(tok)
		if gitea.ValidToken(tok) != nil {
			fmt.Fprintln(stderr, "That is not a token. Nothing was stored.")
			return 1
		}
		c := &gitea.Client{Web: st.HostURL, Token: tok}
		u, err := c.Who(ctx)
		var repo gitea.Repo
		if err == nil {
			repo, err = c.Repo(ctx, st.Repo)
		}
		if err == nil {
			if why := gitea.Usable(u, repo, st.Reviewer); why != "" {
				err = errors.New(why)
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "Nothing was stored.")
			return report(stderr, err)
		}
		if err := gitea.WriteToken(dir, tok); err != nil {
			return report(stderr, err)
		}
		if !strings.EqualFold(u.Login, st.GiteaBot) {
			// Another account: the pull requests of the old one are no
			// longer the service's.
			st.GiteaBot = u.Login
			if err := st.Save(); err != nil {
				return report(stderr, err)
			}
			fmt.Fprintf(stdout, "Note: the new token acts as %s. Open pull requests by the old account are left alone.\n", u.Login)
		}
		fmt.Fprintf(stdout, "Stored. The token acts as %s on %s.\n", plain(u.Login, 100), st.Repo)
		fmt.Fprintln(stdout, "Restart the service to use it: maestro service install, or stop and start maestro run.")
		return 0
	}
	fmt.Fprintln(stderr, giteaUsage)
	return 2
}
