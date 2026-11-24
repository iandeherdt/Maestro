package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"

	"github.com/iandeherdt/Maestro/internal/ghapp"
	"github.com/iandeherdt/Maestro/internal/gitlab"
	"github.com/iandeherdt/Maestro/internal/mirror"
)

// tokenSource hands out a token and says which one is in use.
type tokenSource interface {
	Token(context.Context) (string, error)
	Current() string
}

// codeSide is the code host as the service uses it: GitHub through the
// App, or GitLab through a project access token.
type codeSide struct {
	// code is the repository token: push, change requests, checks.
	code tokenSource
	// tickets is the token for GitHub's issues; nil on GitLab, where the
	// tickets are in Linear.
	tickets *ghapp.Source
	onBoard bool
	// remote is the https address the mirror fetches and pushes.
	remote        string
	defaultBranch func(context.Context) (string, error)
	// guard says, once at the start, when the base branch lets the
	// service's credential push to it.
	guard func(ctx context.Context, base string, out io.Writer)
}

// botEmail is the commit address of the service's identity.
func (in *install) botEmail() string {
	if in.state != nil && in.state.UsesGitLab() {
		host := "gitlab.com"
		if u, err := url.Parse(in.cfg.Workspace.HostURL); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
		return in.bot + "@noreply." + host
	}
	return in.bot + "@users.noreply.github.com"
}

func (in *install) codeSide(ctx context.Context, gh endpoints, log *slog.Logger) (*codeSide, error) {
	cfg := in.cfg
	if cfg.Workspace.Host == "gitlab" {
		return in.gitLabSide(ctx, log)
	}
	app, err := ghapp.New(ghapp.Config{AppID: in.state.AppID, InstallationID: in.state.InstallationID,
		KeyFile: in.state.AppKeyFile, APIURL: gh.api})
	if err != nil {
		return nil, err
	}
	_, name, _ := strings.Cut(cfg.Workspace.Repo, "/")
	// One token for the tickets, one for the code, each limited to this
	// repository and to what it is used for.
	ticketRights := map[string]string{"issues": "write"}
	onBoard := cfg.Tracker.Provider["state_source"] == "project"
	if onBoard {
		// Moving a card needs the organization's Projects permission.
		ticketRights["organization_projects"] = "write"
	}
	tickets, err := app.Source(ghapp.Scope{Repositories: []string{name}, Permissions: ticketRights})
	if err != nil {
		return nil, err
	}
	code, err := app.Source(ghapp.Scope{Repositories: []string{name}, Permissions: map[string]string{
		"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read"}})
	if err != nil {
		return nil, err
	}
	token, err := code.Token(ctx)
	if err != nil {
		return nil, explain("GitHub did not give the App a token for %s: %v\nRun `maestro setup` to check the App and its installation.",
			cfg.Workspace.Repo, err)
	}
	return &codeSide{code: code, tickets: tickets, onBoard: onBoard,
		remote: gh.git + "/" + cfg.Workspace.Repo + ".git",
		defaultBranch: func(ctx context.Context) (string, error) {
			return defaultBranch(ctx, gh.api, cfg.Workspace.Repo, token)
		},
		// GitHub lets the App's key do more than the service ever does.
		guard: func(ctx context.Context, base string, out io.Writer) {
			t, err := code.Token(ctx)
			if err != nil {
				return
			}
			switch guarded, err := baseGuarded(ctx, gh.api, cfg.Workspace.Repo, base, t); {
			case err != nil:
				log.Warn("whether GitHub requires a pull request for the base branch could not be read", "branch", base, "error", err)
			case !guarded:
				log.Warn("GitHub does not require a pull request for the base branch; the service's key could push to it", "branch", base)
				fmt.Fprint(out, unguardedNote(gh.git, cfg.Workspace.Repo, base))
			}
		},
	}, nil
}

func (in *install) gitLabSide(ctx context.Context, log *slog.Logger) (*codeSide, error) {
	cfg := in.cfg
	src := &gitlab.Source{Dir: in.dir, Web: cfg.Workspace.HostURL, ProjectID: in.state.GitLabProject, Log: log}
	tok, err := src.Token(ctx)
	if err != nil {
		return nil, explain("%v", err)
	}
	c := &gitlab.Client{Web: cfg.Workspace.HostURL, Token: tok}
	u, err := c.Who(ctx)
	if err != nil {
		return nil, explain("%v.\nStore a new project access token with `maestro gitlab token`.", err)
	}
	if !strings.EqualFold(u.Username, in.bot) {
		return nil, explain("the stored GitLab token acts as %s, but setup recorded %s. Run `maestro setup --host gitlab` again.",
			u.Username, in.bot)
	}
	p, err := c.Project(ctx, cfg.Workspace.Repo)
	if err != nil {
		return nil, explain("the GitLab project %s cannot be reached: %v", cfg.Workspace.Repo, err)
	}
	if p.ID != in.state.GitLabProject {
		return nil, explain("%s is another GitLab project than the one setup connected. Run `maestro setup --host gitlab` again.",
			cfg.Workspace.Repo)
	}
	// The service never merges. A Maintainer's token could, and could
	// change the protection, so it is not used.
	if role, err := c.Role(ctx, p.ID, u.ID); err != nil {
		return nil, explain("the token's role in %s could not be read: %v", cfg.Workspace.Repo, err)
	} else if role >= gitlab.Maintainer {
		return nil, explain("the GitLab token is a Maintainer or Owner of %s, so it could merge and change the protection.\n"+
			"Make a project access token with the role Developer and store it with `maestro gitlab token`.", cfg.Workspace.Repo)
	}
	return &codeSide{code: src, remote: cfg.Workspace.HostURL + "/" + cfg.Workspace.Repo + ".git",
		defaultBranch: func(context.Context) (string, error) {
			if !mirror.ValidBranch(p.DefaultBranch) {
				return "", fmt.Errorf("the project's default branch has a name the service does not accept")
			}
			return p.DefaultBranch, nil
		},
		guard: func(ctx context.Context, base string, out io.Writer) {
			t, err := src.Token(ctx)
			if err != nil {
				return
			}
			pr, err := (&gitlab.Client{Web: cfg.Workspace.HostURL, Token: t}).Protected(ctx, p.ID, base)
			if err != nil {
				log.Warn("whether GitLab protects the base branch could not be read", "branch", base, "error", err)
				return
			}
			if note := gitLabUnguarded(pr, cfg.Workspace.HostURL, cfg.Workspace.Repo, base); note != "" {
				log.Warn("GitLab lets Developers push or merge to the base branch; the service's token could", "branch", base)
				fmt.Fprint(out, note)
			}
		},
	}, nil
}

// gitLabUnguarded explains how to protect the base branch when a Developer,
// the service's role, may push or merge to it. It is empty when the branch
// is protected well enough.
func gitLabUnguarded(p gitlab.Protection, web, repo, branch string) string {
	if p.Protected && (p.Push == 0 || p.Push >= gitlab.Maintainer) && p.Merge >= gitlab.Maintainer && !p.AllowForcePush && !p.UserCanPush {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nNote: GitLab lets Developers push or merge to %s.\n", branch)
	fmt.Fprintln(&b, "Maestro never pushes to that branch and never merges. But its token is a Developer, so a mistake in")
	fmt.Fprintln(&b, "Maestro or a copy of the token could. To have GitLab refuse it:")
	fmt.Fprintf(&b, "  1. Open %s/%s/-/settings/repository and expand Protected branches.\n", web, repo)
	fmt.Fprintf(&b, "  2. Protect %s with Allowed to merge: Maintainers, Allowed to push and merge: No one,\n", branch)
	fmt.Fprintln(&b, "     and Allowed to force push off.")
	fmt.Fprintln(&b, "On GitLab Premium, also require one approval under Settings, Merge requests, Merge request approvals.")
	return b.String()
}
