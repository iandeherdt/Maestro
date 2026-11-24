package setup

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/iandeherdt/Maestro/internal/gitea"
)

// GiteaRepo connects the service to a repository on Gitea or Forgejo with
// the token of an account of its own (`maestro setup --host gitea`).
type GiteaRepo struct {
	HTTP *http.Client
	// AskToken reads the token without showing it. Default: an ordinary
	// question.
	AskToken func(ui *UI, prompt string) (string, error)
}

func (*GiteaRepo) Title() string { return "Connect the Gitea repository" }

func (g *GiteaRepo) client(st *State, token string) *gitea.Client {
	return &gitea.Client{Web: st.HostURL, Token: token, HTTP: g.HTTP}
}

func (g *GiteaRepo) Check(ctx context.Context, st *State) (Status, error) {
	if !st.UsesGitea() {
		return Status{Done: true, Detail: "Not needed: the code is on " + st.HostLabel() + "."}, nil
	}
	if st.Repo == "" || st.HostURL == "" || st.GiteaBot == "" || st.Reviewer == "" {
		return Status{Detail: "The service needs a Gitea account of its own, so that it never acts as you."}, nil
	}
	tok, err := gitea.ReadToken(st.Dir())
	if err != nil {
		return Status{}, err
	}
	if tok == "" {
		return Status{Detail: "No Gitea token is stored in " + gitea.TokenFile(st.Dir()) + "."}, nil
	}
	c := g.client(st, tok)
	u, err := c.Who(ctx)
	if err != nil {
		return Status{Detail: "The stored token does not work: " + err.Error()}, nil
	}
	if !strings.EqualFold(u.Login, st.GiteaBot) {
		return Status{Detail: "The stored token acts as " + oneLine(u.Login) + ", not as " + st.GiteaBot + "."}, nil
	}
	r, err := c.Repo(ctx, st.Repo)
	if err != nil {
		return Status{Detail: "The token cannot reach " + st.Repo + ": " + err.Error()}, nil
	}
	if why := gitea.Usable(u, r, st.Reviewer); why != "" {
		return Status{Detail: "The stored token cannot be used: " + why + "."}, nil
	}
	return Status{Done: true, Detail: "Maestro acts as " + st.GiteaBot + " on " + st.Repo + "; reviews by " + st.Reviewer + " count."}, nil
}

func (g *GiteaRepo) Run(ctx context.Context, ui *UI, st *State) error {
	web, err := ui.Ask("  Where is your Gitea or Forgejo (such as https://git.example.com)?", st.HostURL, func(s string) error {
		_, err := gitea.CheckWeb(s)
		return err
	})
	if err != nil {
		return err
	}
	st.HostURL, _ = gitea.CheckWeb(web)
	path, err := ui.Ask("  Which repository should the agents work on (owner/name)?", st.Repo, func(s string) error {
		if !gitea.ValidRepo(strings.Trim(strings.TrimSpace(s), "/")) {
			return errors.New("give the repository as owner/name, as it is in its address")
		}
		return nil
	})
	if err != nil {
		return err
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	reviewer, err := ui.Ask("  Your username there: whose reviews and comments on the pull requests count", st.Reviewer, func(s string) error {
		if !gitLabUserRE.MatchString(strings.TrimPrefix(strings.TrimSpace(s), "@")) {
			return errors.New("give a username, such as jdoe")
		}
		return nil
	})
	if err != nil {
		return err
	}
	reviewer = strings.TrimPrefix(strings.TrimSpace(reviewer), "@")

	ui.Say("  The service opens pull requests as an account of its own: what it does is never your")
	ui.Say("  approval, and you can approve what it opens. To make one:")
	ui.Say("    1. Make a user for the service, such as maestro-bot (an administrator can, under Site")
	ui.Say("       administration, User accounts). Not a site administrator.")
	ui.Say("    2. In %s/%s/settings/collaboration, add it with the right Write, not Administrator.", st.HostURL, path)
	ui.Say("    3. Signed in as that user, open %s/user/settings/applications and generate a token", st.HostURL)
	ui.Say("       with the permissions repository and issue on Read and write, and user on Read.")
	ui.Say("  The token is kept in %s, readable by you only. The agent never sees it.", gitea.TokenFile(st.Dir()))
	var tok string
	var user gitea.User
	var repo gitea.Repo
	for {
		if g.AskToken != nil {
			tok, err = g.AskToken(ui, "  The service account's token (it is not shown): ")
		} else {
			tok, err = ui.Ask("  The service account's token", "", nil)
		}
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(tok)
		if err := gitea.ValidToken(tok); err != nil {
			ui.Say("  %v. Paste the whole token.", err)
			continue
		}
		c := g.client(st, tok)
		if user, err = c.Who(ctx); err != nil {
			ui.Say("  %v.", err)
			continue
		}
		if repo, err = c.Repo(ctx, path); err != nil {
			ui.Say("  The token cannot reach %s: %v.", path, err)
			continue
		}
		if why := gitea.Usable(user, repo, reviewer); why != "" {
			ui.Say("  This token cannot be used: %s.", why)
			continue
		}
		break
	}
	if err := gitea.WriteToken(st.Dir(), tok); err != nil {
		return err
	}
	st.Repo, st.GiteaBot, st.Reviewer = repo.FullName, user.Login, reviewer
	if st.Repo == "" {
		st.Repo = path
	}
	st.Tracker = TrackerLinear
	ui.Say("  Stored. The service acts as %s on %s.", st.GiteaBot, st.Repo)
	return nil
}
