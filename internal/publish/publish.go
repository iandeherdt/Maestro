// Package publish turns the agent's exported commits into a pull request
// (SPEC section 15): import into the mirror, diff policy and secret scan,
// push, and the pull request itself.
//
// It never approves, merges, enables auto-merge, dismisses a review or
// touches branch protection. There is no code path here that could.
package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/policy"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// Rejection categories.
const (
	ImportRejected = "import_rejected"
	PolicyDenied   = "policy_denied"
)

// Mirror is the part of the mirror the publisher uses.
type Mirror interface {
	Import(ctx context.Context, bundlePath, workspaceKey, baseSHA string) (mirror.Imported, error)
	Diff(ctx context.Context, baseSHA, head string, lim mirror.Limits) (mirror.Diff, error)
	Push(ctx context.Context, remoteURL, workspaceKey, branch, token string) error
	ForgetPush(ctx context.Context, workspaceKey string) error
}

// Publisher implements the publish gate.
type Publisher struct {
	// Config returns the configuration in force.
	Config func() *config.Config
	Mirror Mirror
	// HandoffDir is where the export step leaves <workspace_key>.bundle.
	HandoffDir string
	// Token returns a repository token (Contents and Pull requests write).
	Token func(context.Context) (string, error)
	// HeldSecrets returns the credential values the service holds, for the
	// exact-match part of the secret scan.
	HeldSecrets func() []string
	// BotName and BotEmail are the commit identity the sandbox is set up
	// with. Empty skips the identity flag.
	BotName, BotEmail string
	// BotLogin is the account the service's pull requests are opened by,
	// such as "my-app[bot]". When set, the hand-off watch ignores pull
	// requests opened by anyone else.
	BotLogin string

	// APIURL and GitURL default to github.com. HTTP defaults to a client
	// with a timeout.
	APIURL, GitURL string
	HTTP           *http.Client
	Now            func() time.Time
}

var repoRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)$`)

var (
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	nodeRE = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,200}$`)
)

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Publish runs the gate. Exactly one of the record and the rejection is
// set when the error is nil.
func (p *Publisher) Publish(ctx context.Context, is issue.Issue, ws workspace.Workspace, v worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error) {
	cfg := p.Config()
	m := repoRE.FindStringSubmatch(cfg.Workspace.Repo)
	if m == nil || strings.HasSuffix(m[2], ".git") || m[2] == "." || m[2] == ".." {
		return nil, nil, errors.New("workspace.repo must be owner/name")
	}
	owner, repo := m[1], m[2]
	prefix := cfg.Publish.BranchPrefix
	// The service only ever writes below its own prefix, so no ticket can
	// steer a push at the base branch or at someone's feature branch.
	if prefix == "" || !strings.HasPrefix(ws.Branch, prefix) || len(ws.Branch) == len(prefix) || !mirror.ValidBranch(ws.Branch) {
		return nil, nil, errors.New("the workspace branch is not under publish.branch_prefix")
	}
	if ws.Branch == cfg.Workspace.BaseBranch {
		return nil, nil, errors.New("the workspace branch is the base branch")
	}

	imp, err := p.Mirror.Import(ctx, filepath.Join(p.HandoffDir, ws.Key+".bundle"), ws.Key, ws.BaseSHA)
	var rej *mirror.Rejected
	if errors.As(err, &rej) {
		return nil, &worker.Rejection{Category: ImportRejected,
			Feedback: []issue.Feedback{{Source: "policy", Body: "The commits could not be accepted: " + rej.Reason + "."}}}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("import: %w", err)
	}

	diff, err := p.Mirror.Diff(ctx, ws.BaseSHA, imp.Head, mirror.Limits{
		MaxChangedFiles: cfg.Policy.MaxChangedFiles, MaxDiffBytes: cfg.Policy.MaxDiffBytes, MaxFileBytes: cfg.Policy.MaxFileBytes})
	if err != nil {
		return nil, nil, fmt.Errorf("diff: %w", err)
	}
	var held []string
	if p.HeldSecrets != nil {
		held = p.HeldSecrets()
	}
	res := policy.Evaluate(cfg.Policy, policy.Input{Changes: diff.Changes, Commits: imp.Commits, BasePaths: diff.BasePaths,
		BotName: p.BotName, BotEmail: p.BotEmail, HeldSecrets: held})
	if !res.OK() {
		r := &worker.Rejection{Category: PolicyDenied}
		for _, line := range res.Feedback() {
			r.Feedback = append(r.Feedback, issue.Feedback{Source: "policy", Body: line})
		}
		return nil, r, nil
	}

	api, gitURL, err := p.endpoints(owner, repo)
	if err != nil {
		return nil, nil, err
	}
	tok, err := p.Token(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("repository token: %w", err)
	}
	if err := p.Mirror.Push(ctx, gitURL, ws.Key, ws.Branch, tok); err != nil {
		return nil, nil, fmt.Errorf("push: %w", err)
	}

	var flags []flagLine
	var flagText []string
	for _, f := range res.Flags {
		flags = append(flags, flagLine{f.Rule, f.Path, f.Detail})
		flagText = append(flagText, f.String())
	}
	closes := ""
	if cfg.Tracker.Kind == "github" && strings.HasPrefix(is.Identifier, "GH-") {
		if n, err := strconv.Atoi(strings.TrimPrefix(is.Identifier, "GH-")); err == nil && n > 0 {
			trackerRepo, _ := cfg.Tracker.Provider["repo"].(string)
			if strings.EqualFold(trackerRepo, cfg.Workspace.Repo) {
				closes = "Closes #" + strconv.Itoa(n)
			} else if repoRE.MatchString(trackerRepo) {
				closes = "Issue: " + trackerRepo + "#" + strconv.Itoa(n)
			}
		}
	}
	proof := Proof{VerifyCommand: cfg.Verify.Command, VerifyStatus: v.ExitStatus, Worker: cfg.Worker.Name,
		Commits: len(imp.Commits), Files: len(diff.Changes), ScreenshotNote: v.ScreenshotNote}
	if len(v.Screenshots) > 0 {
		// Pictures are an extra. A pull request without them is better
		// than no pull request.
		if proof.Screenshots, err = p.store(ctx, api, owner, repo, ws.Key, imp.Head, v.Screenshots); err != nil {
			proof.ScreenshotNote = "The screenshots were taken but could not be stored in the repository."
		}
	}
	text := body(closes, flags, proof, "")
	number, err := api.upsert(ctx, owner, repo, ws.Branch, cfg.Workspace.BaseBranch, title(is.Identifier, is.Title), text,
		cfg.Publish.DraftUntilGreen)
	if err != nil {
		return nil, nil, fmt.Errorf("pull request: %w", err)
	}
	return &orchestrator.PublishRecord{
		IssueID: is.ID, Identifier: is.Identifier, Branch: ws.Branch, HeadSHA: imp.Head, PRNumber: number,
		ChecksDeadline: p.now().Add(time.Duration(cfg.Publish.ChecksTimeoutMS) * time.Millisecond), Flags: flagText,
	}, nil, nil
}

func (p *Publisher) endpoints(owner, repo string) (*api, string, error) {
	apiURL := p.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	base, err := url.Parse(apiURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" {
		return nil, "", errors.New("the API URL is not valid")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "127.0.0.1" || base.Hostname() == "localhost")) {
		return nil, "", errors.New("the API URL must use https")
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if p.HTTP != nil {
		cp := *p.HTTP
		hc = &cp
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	git := p.GitURL
	if git == "" {
		git = "https://github.com"
	}
	return &api{base: base, hc: hc, token: p.Token}, strings.TrimSuffix(git, "/") + "/" + owner + "/" + repo + ".git", nil
}

// Publish must keep the signature of the worker's publish gate.
var _ func(context.Context, issue.Issue, workspace.Workspace, worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error) = (*Publisher)(nil).Publish
