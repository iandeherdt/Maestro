package publish

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// host is the code host's side of publishing: where the branch is pushed,
// and the change request (a pull request on GitHub and Gitea, a merge
// request on GitLab) with its checks, reviews and comments. Everything
// that decides something stays in this package's shared code: what counts
// as passed, whose review or comment counts, the feedback mark. A host
// only reads and writes.
//
// No method approves, merges, enables auto-merge, dismisses a review or
// changes protection, on any host.
type host interface {
	// gitURL is the https remote of the repository, without credentials.
	gitURL() string
	// upsert opens the change request of branch, or updates the open one,
	// and returns its number. An empty base is the default branch. An open
	// one that targets another base is an error.
	upsert(ctx context.Context, branch, base, title, body string, draft bool) (int, error)
	// change reads one change request.
	change(ctx context.Context, number int) (change, error)
	// changes lists the change requests from branch in the repository
	// itself, newest first. One from a fork that reuses the name is left
	// out.
	changes(ctx context.Context, branch string) ([]change, error)
	// checks reads the results reported for a commit, by name, the worse
	// one when a name repeats. It reports false when there were more than
	// it reads.
	checks(ctx context.Context, sha string) (map[string]check, bool, error)
	// markReady takes a draft out of draft.
	markReady(ctx context.Context, c change) error
	// reviews lists the reviews of a change request. It reports false when
	// there were more than it reads.
	reviews(ctx context.Context, number int) ([]prReview, bool, error)
	// comments lists what was written on a change request: the
	// conversation and the comments on lines, with the time each became
	// visible. Review summaries come from reviews. It reports false when a
	// list was too long to read whole.
	comments(ctx context.Context, number int, reviews []prReview) ([]comment, bool, error)
	// comment writes a comment on a change request, as the bot.
	comment(ctx context.Context, number int, body string) error
	// commitTime is when a commit was committed.
	commitTime(ctx context.Context, sha string) (time.Time, error)
	// deleteBranch removes a branch. One that is already gone is not an
	// error.
	deleteBranch(ctx context.Context, branch string) error
	// storeShots keeps the pictures where the change request can show
	// them, and returns a link to each.
	storeShots(ctx context.Context, key, head string, shots []worker.Screenshot) ([]shotLink, error)
}

// Change request states.
const (
	changeOpen   = "open"
	changeClosed = "closed"
	changeMerged = "merged"
)

// change is one change request.
type change struct {
	Number  int
	State   string
	Draft   bool
	HeadSHA string
	// Author is the login of whoever opened it.
	Author string
	// id is the host's own handle, such as GitHub's node id.
	id string
}

// Review verdicts.
const (
	verdictNone    = ""
	verdictApprove = "approve"
	verdictChanges = "changes"
)

// prReview is one review of a change request.
type prReview struct {
	ID        int64
	Verdict   string
	CommitSHA string
	Body      string
	// At is when it was submitted, in RFC 3339; empty while it is pending.
	At      string
	Login   string
	Pending bool
}

// comment is one comment on a change request.
type comment struct {
	Login string
	// At is when it became visible, in RFC 3339.
	At   string
	Body string
	Path string
	Line int
}

// check is one result reported for a commit.
type check struct {
	res    result
	detail string
}

type result int

const (
	resPending result = iota
	resPassed
	resFailed
)

func worse(a, b result) bool {
	rank := map[result]int{resPassed: 0, resPending: 1, resFailed: 2}
	return rank[a] > rank[b]
}

// host returns the code host of the repository in the settings.
func (p *Publisher) host() (host, error) {
	if p.testHost != nil {
		return p.testHost, nil
	}
	w := p.Config().Workspace
	switch w.Host {
	case "", "github":
	case "gitlab":
		g, err := p.gitLab(w.Repo)
		if err != nil {
			return nil, err
		}
		return g, nil
	default:
		return nil, errors.New("workspace.host " + w.Host + " is not supported")
	}
	m := repoRE.FindStringSubmatch(w.Repo)
	if m == nil {
		return nil, errors.New("workspace.repo must be owner/name")
	}
	g, err := p.gitHub(m[1], m[2])
	if err != nil {
		return nil, err
	}
	return g, nil
}

// validRepo reports whether the repository in the settings can be named
// on its host.
func validRepo(w config.Workspace) bool {
	if w.Host == "gitlab" {
		return ValidGitLabPath(w.Repo)
	}
	m := repoRE.FindStringSubmatch(w.Repo)
	return m != nil && !strings.HasSuffix(m[2], ".git") && m[2] != "." && m[2] != ".."
}
