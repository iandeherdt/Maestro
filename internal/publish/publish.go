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
	OnBranch(ctx context.Context, sha, branch, notUnder string) (bool, error)
	HistoryAdded(ctx context.Context, baseSHA, head string, maxBytes int64) (lines []string, complete bool, err error)
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
	// ExportedHead returns the commit the export step reported for a
	// workspace, which is the one the verify step ran on. When set, a
	// bundle that holds another commit is refused.
	ExportedHead func(workspaceKey string) string
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

	// testHost replaces the code host in tests.
	testHost host
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
	if err := target(cfg, ws); err != nil {
		return nil, nil, err
	}
	ad, rej, err := p.admit(ctx, cfg, ws)
	if err != nil || rej != nil {
		return nil, rej, err
	}
	imp, diff, res := ad.imp, ad.diff, ad.res
	h, err := p.host()
	if err != nil {
		return nil, nil, err
	}
	tok, err := p.Token(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("repository token: %w", err)
	}
	if err := p.Mirror.Push(ctx, h.gitURL(), ws.Key, ws.Branch, tok); err != nil {
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
	if cfg.Tracker.Kind == "linear" && strings.HasPrefix(is.URL, "https://linear.app/") && !strings.ContainsAny(is.URL, " ()[]<>\n") {
		// Linear links the pull request by the identifier in its title
		// and branch; the link is for the reader.
		closes = "Ticket: [" + is.Identifier + "](" + is.URL + ")"
	}
	proof := Proof{VerifyCommand: cfg.Verify.Command, VerifyStatus: v.ExitStatus, Worker: cfg.Worker.Name,
		Commits: len(imp.Commits), Files: len(diff.Changes), ScreenshotNote: v.ScreenshotNote}
	if len(v.Screenshots) > 0 {
		// Pictures are an extra. A pull request without them is better
		// than no pull request.
		if proof.Screenshots, err = h.storeShots(ctx, ws.Key, imp.Head, v.Screenshots); err != nil {
			proof.ScreenshotNote = "The screenshots were taken but could not be stored in the repository."
		}
	}
	text := body(closes, flags, proof, "")
	number, err := h.upsert(ctx, ws.Branch, cfg.Workspace.BaseBranch, title(is.Identifier, is.Title), text,
		cfg.Publish.DraftUntilGreen)
	if err != nil {
		return nil, nil, fmt.Errorf("pull request: %w", err)
	}
	return &orchestrator.PublishRecord{
		IssueID: is.ID, Identifier: is.Identifier, Branch: ws.Branch, HeadSHA: imp.Head, PRNumber: number,
		ChecksDeadline: p.now().Add(time.Duration(cfg.Publish.ChecksTimeoutMS) * time.Millisecond), Flags: flagText,
	}, nil, nil
}

// target names the repository and checks the branch a workspace is
// published on.
func target(cfg *config.Config, ws workspace.Workspace) error {
	m := repoRE.FindStringSubmatch(cfg.Workspace.Repo)
	if m == nil || strings.HasSuffix(m[2], ".git") || m[2] == "." || m[2] == ".." {
		return errors.New("workspace.repo must be owner/name")
	}
	prefix := cfg.Publish.BranchPrefix
	// The service only ever writes below its own prefix, so no ticket can
	// steer a push at the base branch or at someone's feature branch.
	if prefix == "" || !strings.HasPrefix(ws.Branch, prefix) || len(ws.Branch) == len(prefix) || !mirror.ValidBranch(ws.Branch) {
		return errors.New("the workspace branch is not under publish.branch_prefix")
	}
	if ws.Branch == cfg.Workspace.BaseBranch {
		return errors.New("the workspace branch is the base branch")
	}
	return nil
}

// historyFactor is how many times policy.max_diff_bytes the commits may
// change when they are added up one by one. Work that goes back and forth
// over the same lines is larger that way than its final diff.
const historyFactor = 16

// admitted is work that passed import, diff policy and secret scan.
type admitted struct {
	imp  mirror.Imported
	diff mirror.Diff
	res  policy.Result
}

// admit imports the exported commits and applies the diff policy and the
// secret scan. Exactly one of the result and the rejection is set when the
// error is nil.
func (p *Publisher) admit(ctx context.Context, cfg *config.Config, ws workspace.Workspace) (*admitted, *worker.Rejection, error) {
	refuse := func(reason string) (*admitted, *worker.Rejection, error) {
		return nil, &worker.Rejection{Category: ImportRejected,
			Feedback: []issue.Feedback{{Source: "policy", Body: "The commits could not be accepted: " + reason + "."}}}, nil
	}
	// The sandbox says which commit the work started from, and the agent
	// can change what it says. Only a commit of the base branch is taken:
	// the diff below is then a diff against the owner's code, never against
	// a commit of the agent's own.
	onBase, err := p.Mirror.OnBranch(ctx, ws.BaseSHA, cfg.Workspace.BaseBranch, cfg.Publish.BranchPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("base commit: %w", err)
	}
	if !onBase {
		name := "the base branch"
		if cfg.Workspace.BaseBranch != "" {
			name = "the branch " + cfg.Workspace.BaseBranch
		}
		return refuse("the workspace does not start from a commit of " + name +
			". If that branch was rewritten, remove the workspace so that the work starts again")
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
	if p.ExportedHead != nil {
		if want := p.ExportedHead(ws.Key); want != "" && want != imp.Head {
			return refuse("the bundle does not hold the commit that was exported and checked")
		}
	}

	diff, err := p.Mirror.Diff(ctx, ws.BaseSHA, imp.Head, mirror.Limits{
		MaxChangedFiles: cfg.Policy.MaxChangedFiles, MaxDiffBytes: cfg.Policy.MaxDiffBytes, MaxFileBytes: cfg.Policy.MaxFileBytes,
		BinaryPaths: cfg.Policy.BinaryPaths, BinaryMaxFileBytes: cfg.Policy.BinaryMaxFileBytes})
	if err != nil {
		return nil, nil, fmt.Errorf("diff: %w", err)
	}
	var held []string
	if p.HeldSecrets != nil {
		held = p.HeldSecrets()
	}
	// The diff is the change from base to head. Every commit is pushed, so
	// what one commit adds and a later one removes is looked at as well.
	var history []string
	if cfg.Policy.SecretScan && len(imp.Commits) > 1 {
		limit := max(historyFactor*cfg.Policy.MaxDiffBytes, 1<<20)
		lines, complete, err := p.Mirror.HistoryAdded(ctx, ws.BaseSHA, imp.Head, limit)
		if err != nil {
			return nil, nil, fmt.Errorf("history: %w", err)
		}
		if !complete {
			return nil, &worker.Rejection{Category: PolicyDenied, Feedback: []issue.Feedback{{Source: "policy",
				Body: fmt.Sprintf("D5 the commits together change more than %d bytes, too much to check one by one. "+
					"Make the change again from the base commit in fewer, smaller commits", limit)}}}, nil
		}
		history = lines
	}
	res := policy.Evaluate(cfg.Policy, policy.Input{Changes: diff.Changes, Commits: imp.Commits, BasePaths: diff.BasePaths,
		BotName: p.BotName, BotEmail: p.BotEmail, HeldSecrets: held, HistoryAdded: history})
	if !res.OK() {
		r := &worker.Rejection{Category: PolicyDenied}
		for _, line := range res.Feedback() {
			r.Feedback = append(r.Feedback, issue.Feedback{Source: "policy", Body: line})
		}
		return nil, r, nil
	}

	return &admitted{imp, diff, res}, nil, nil
}

// Publish must keep the signature of the worker's publish gate.
var _ func(context.Context, issue.Issue, workspace.Workspace, worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error) = (*Publisher)(nil).Publish
