package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/iandeherdt/Maestro/internal/gitlab"
)

// Code hosts.
const (
	HostGitHub = "github"
	HostGitLab = "gitlab"
	// HostGitea is Gitea, and Forgejo, which keeps Gitea's API.
	HostGitea = "gitea"
)

// UsesGitLab reports whether the code is on GitLab.
func (s *State) UsesGitLab() bool { return s.Host == HostGitLab }

// UsesGitea reports whether the code is on Gitea or Forgejo.
func (s *State) UsesGitea() bool { return s.Host == HostGitea }

// OffGitHub reports whether the code is on another host than GitHub, where
// the service has no GitHub App.
func (s *State) OffGitHub() bool { return s.UsesGitLab() || s.UsesGitea() }

// HostLabel is the code host's name for people.
func (s *State) HostLabel() string {
	switch {
	case s.UsesGitLab():
		return "GitLab"
	case s.UsesGitea():
		return "Gitea"
	}
	return "GitHub"
}

// SwitchHost sets where the code is. Off GitHub the tickets are kept in
// Linear: GitHub's issues live in a GitHub repository.
func SwitchHost(st *State, kind string) error {
	if kind == "forgejo" {
		kind = HostGitea
	}
	switch kind {
	case HostGitHub, HostGitLab, HostGitea:
	default:
		return fmt.Errorf("the code host is %s, %s or %s (also for Forgejo)", HostGitHub, HostGitLab, HostGitea)
	}
	if kind != st.HostName() {
		// Another host is another repository: the earlier answers no
		// longer apply.
		st.Repo, st.HostURL, st.GitLabBot, st.GitLabProject, st.GiteaBot, st.Reviewer = "", "", "", 0, "", ""
	}
	st.Host = kind
	if kind == HostGitHub {
		st.Host = ""
	} else {
		st.Tracker = TrackerLinear
	}
	return st.Save()
}

var gitLabUserRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)

// GitLabProject connects the service to a GitLab project with a project
// access token (`maestro setup --host gitlab`).
type GitLabProject struct {
	HTTP *http.Client
	// AskToken reads the token without showing it. Default: an ordinary
	// question.
	AskToken func(ui *UI, prompt string) (string, error)
}

func (*GitLabProject) Title() string { return "Connect the GitLab project" }

func (g *GitLabProject) client(st *State, token string) *gitlab.Client {
	return &gitlab.Client{Web: st.HostURL, Token: token, HTTP: g.HTTP}
}

func (g *GitLabProject) Check(ctx context.Context, st *State) (Status, error) {
	if !st.UsesGitLab() {
		return Status{Done: true, Detail: "Not needed: the code is on " + st.HostLabel() + "."}, nil
	}
	if st.Repo == "" || st.GitLabBot == "" || st.GitLabProject <= 0 || st.Reviewer == "" {
		return Status{Detail: "The service needs a project access token of its own, so that it never acts as you."}, nil
	}
	tok, err := gitlab.ReadToken(st.Dir())
	if err != nil {
		return Status{}, err
	}
	if tok == "" {
		return Status{Detail: "No GitLab token is stored in " + gitlab.TokenFile(st.Dir()) + "."}, nil
	}
	c := g.client(st, tok)
	u, err := c.Who(ctx)
	if err != nil {
		return Status{Detail: "The stored token does not work: " + err.Error()}, nil
	}
	if !strings.EqualFold(u.Username, st.GitLabBot) {
		return Status{Detail: "The stored token acts as " + oneLine(u.Username) + ", not as " + st.GitLabBot + "."}, nil
	}
	if _, err := c.Project(ctx, st.Repo); err != nil {
		return Status{Detail: "The token cannot reach " + st.Repo + ": " + err.Error()}, nil
	}
	return Status{Done: true, Detail: "Maestro acts as " + st.GitLabBot + " on " + st.Repo + "; reviews by " + st.Reviewer + " count."}, nil
}

func (g *GitLabProject) Run(ctx context.Context, ui *UI, st *State) error {
	web, err := ui.Ask("  Where is your GitLab?", orDefault(st.HostURL, gitlab.Default), func(s string) error {
		_, err := gitlab.CheckWeb(s)
		return err
	})
	if err != nil {
		return err
	}
	st.HostURL, _ = gitlab.CheckWeb(web)
	path, err := ui.Ask("  Which project should the agents work on (group/name, or group/subgroup/name)?", st.Repo, func(s string) error {
		if !gitlab.ValidPath(strings.Trim(strings.TrimSpace(s), "/")) {
			return errors.New("give the project's path as it is in its address, such as mygroup/myproject")
		}
		return nil
	})
	if err != nil {
		return err
	}
	path = strings.Trim(strings.TrimSpace(path), "/")

	ui.Say("  The service opens merge requests with a project access token, which has a bot user of its own:")
	ui.Say("  what it does is never your approval, and you can approve what it opens. To make one:")
	ui.Say("    1. Open %s/%s/-/settings/access_tokens and press Add new token.", st.HostURL, path)
	ui.Say("    2. Name it maestro. Give it the role Developer, not Maintainer or Owner.")
	ui.Say("    3. Tick the scopes api and write_repository. Set the expiry as far out as GitLab allows;")
	ui.Say("       the service renews the token by itself before it expires.")
	ui.Say("  The token is kept in %s, readable by you only. The agent never sees it.", gitlab.TokenFile(st.Dir()))
	var tok string
	var user gitlab.User
	var project gitlab.Project
	for {
		if g.AskToken != nil {
			tok, err = g.AskToken(ui, "  Project access token (it is not shown): ")
		} else {
			tok, err = ui.Ask("  Project access token", "", nil)
		}
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(tok)
		if err := gitlab.ValidToken(tok); err != nil {
			ui.Say("  %v. Paste the whole token.", err)
			continue
		}
		c := g.client(st, tok)
		if user, err = c.Who(ctx); err != nil {
			ui.Say("  %v.", err)
			continue
		}
		if project, err = c.Project(ctx, path); err != nil {
			ui.Say("  The token cannot reach %s: %v.", path, err)
			continue
		}
		role, err := c.Role(ctx, project.ID, user.ID)
		if err != nil {
			ui.Say("  Its role in the project could not be read: %v.", err)
			continue
		}
		if role >= gitlab.Maintainer {
			ui.Say("  This token is a Maintainer or Owner of %s: it could merge and change the protection. Make one", path)
			ui.Say("  with the role Developer.")
			continue
		}
		if role < gitlab.Developer {
			ui.Say("  This token's role in %s cannot push. Make one with the role Developer.", path)
			continue
		}
		if !user.Bot {
			ui.Say("  This token belongs to %s, a person, not to a project's bot. The merge requests would be", oneLine(user.Username))
			ui.Say("  that person's, who could then not approve them. Make a project access token as above.")
			continue
		}
		break
	}
	reviewer, err := ui.Ask("  Your GitLab username: whose reviews and comments on the merge requests count", st.Reviewer, func(s string) error {
		if !gitLabUserRE.MatchString(strings.TrimPrefix(strings.TrimSpace(s), "@")) {
			return errors.New("give a GitLab username, such as jdoe")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := gitlab.WriteToken(st.Dir(), tok); err != nil {
		return err
	}
	st.Repo, st.GitLabBot, st.GitLabProject = project.Path, user.Username, project.ID
	if st.Repo == "" {
		st.Repo = path
	}
	st.Reviewer = strings.TrimPrefix(strings.TrimSpace(reviewer), "@")
	st.Tracker = TrackerLinear
	ui.Say("  Stored. The service acts as %s on %s.", st.GitLabBot, st.Repo)
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// HostName is the code host as the settings file names it.
func (s *State) HostName() string {
	if s.OffGitHub() {
		return s.Host
	}
	return HostGitHub
}
