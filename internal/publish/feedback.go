package publish

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

// What the owner writes on a pull request is taken up by the next run
// (SPEC 15.8). The service remembers how far it got on the pull request
// itself, in a comment of its own that carries a mark. Nothing is kept on
// disk, so a second machine or a reinstalled one sees the same thing.

const (
	// maxNotes and maxNoteRunes bound what one run is given.
	maxNotes     = 30
	maxNoteRunes = 4000
)

var markRE = regexp.MustCompile(`<!-- symphony:feedback-through (\S+) -->`)

// note is one thing a person wrote on the pull request.
type note struct {
	at time.Time
	f  issue.Feedback
}

// thread is what allowlisted people wrote on a pull request, oldest first,
// and the marks the service left, newest first.
type thread struct {
	notes []note
	marks []time.Time
}

// mark returns the i-th newest mark, or the zero time.
func (t thread) mark(i int) time.Time {
	if i < len(t.marks) {
		return t.marks[i]
	}
	return time.Time{}
}

func (t thread) after(at time.Time) []note {
	i := sort.Search(len(t.notes), func(i int) bool { return t.notes[i].at.After(at) })
	return t.notes[i:]
}

// commentsOn reports whether comments on a pull request send a ticket back
// to work. Without a bot login the service cannot tell its own mark from
// one somebody typed, so the feature is off.
func (p *Publisher) commentsOn(cfg *config.Config) bool {
	return cfg.Publish.ReworkOnComments && p.BotLogin != ""
}

func capped(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxNoteRunes {
		return string(r[:maxNoteRunes]) + "\n[cut off here]"
	}
	return s
}

// thread reads the conversation, the comments on lines and the review
// summaries of a pull request. It reports false when one of the lists was
// too long to read whole.
func (p *Publisher) thread(ctx context.Context, a *api, prefix string, number int, reviews []listedReview) (thread, bool, error) {
	var t thread
	cfg := p.Config()
	add := func(login, when, body, path string, line int) {
		at, err := time.Parse(time.RFC3339, when)
		body = capped(body)
		if err != nil || body == "" || strings.EqualFold(login, p.BotLogin) || !allowed(cfg.Tracker.AllowedCommenters, login) {
			return
		}
		t.notes = append(t.notes, note{at, issue.Feedback{Source: "review", Author: login, Body: body, Path: path, Line: line}})
	}
	n := strconv.Itoa(number)

	type user struct {
		Login string `json:"login"`
	}
	talk, complete, err := listAll[struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      user   `json:"user"`
	}](ctx, a, prefix+"/issues/"+n+"/comments")
	if err != nil || !complete {
		return t, false, err
	}
	for _, c := range talk {
		// Only the service's own comments can carry a mark.
		if strings.EqualFold(c.User.Login, p.BotLogin) {
			if m := markRE.FindStringSubmatch(c.Body); m != nil {
				if at, err := time.Parse(time.RFC3339, m[1]); err == nil {
					t.marks = append(t.marks, at)
				}
			}
			continue
		}
		add(c.User.Login, c.CreatedAt, c.Body, "", 0)
	}

	inline, complete, err := listAll[struct {
		Body         string `json:"body"`
		CreatedAt    string `json:"created_at"`
		Path         string `json:"path"`
		Line         int    `json:"line"`
		OriginalLine int    `json:"original_line"`
		User         user   `json:"user"`
	}](ctx, a, prefix+"/pulls/"+n+"/comments")
	if err != nil || !complete {
		return t, false, err
	}
	for _, c := range inline {
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		add(c.User.Login, c.CreatedAt, c.Body, c.Path, line)
	}
	for _, r := range reviews {
		if r.State != "PENDING" {
			add(r.User.Login, r.SubmittedAt, r.Body, "", 0)
		}
	}

	sort.SliceStable(t.notes, func(i, j int) bool { return t.notes[i].at.Before(t.notes[j].at) })
	sort.Slice(t.marks, func(i, j int) bool { return t.marks[i].After(t.marks[j]) })
	return t, true, nil
}

// mark writes on the pull request that everything up to through was taken
// up.
func (p *Publisher) markTaken(ctx context.Context, a *api, prefix string, number, count int, through time.Time) error {
	what := "the new comment"
	if count > 1 {
		what = fmt.Sprintf("the %d new comments", count)
	}
	body := fmt.Sprintf("Picking up %s on this pull request. The ticket is back in work, and the changes will arrive here as a new commit.\n\n"+
		"<!-- symphony:feedback-through %s -->", what, through.UTC().Format(time.RFC3339))
	return a.do(ctx, http.MethodPost, prefix+"/issues/"+strconv.Itoa(number)+"/comments", nil, map[string]string{"body": body}, nil)
}

// Acknowledge records on the pull request that the comments up to
// st.FeedbackThrough were taken up.
func (p *Publisher) Acknowledge(ctx context.Context, is issue.Issue, st orchestrator.ReviewStatus) error {
	a, prefix, err := p.repoAPI()
	if err != nil {
		return err
	}
	if st.PRNumber <= 0 || st.FeedbackThrough.IsZero() {
		return nil
	}
	return p.markTaken(ctx, a, prefix, st.PRNumber, st.FeedbackCount, st.FeedbackThrough)
}

// Feedback returns what the owner wrote on the ticket's open pull request
// that the coming run has to answer: everything since the last mark but
// one, because the last mark is the one that started this round. Once a
// commit has followed a mark, the round it started is over.
//
// Comments nobody has taken up yet are included and marked here. That is
// the case when the owner moved the card back by hand before the service
// did.
func (p *Publisher) Feedback(ctx context.Context, is issue.Issue) ([]issue.Feedback, error) {
	if !p.commentsOn(p.Config()) {
		return nil, nil
	}
	a, prefix, err := p.repoAPI()
	if err != nil {
		return nil, err
	}
	pr, err := p.pull(ctx, a, prefix, is)
	if err != nil || pr == nil || pr.MergedAt != "" || pr.State != "open" {
		return nil, err
	}
	reviews, complete, err := listAll[listedReview](ctx, a, prefix+"/pulls/"+strconv.Itoa(pr.Number)+"/reviews")
	if err != nil || !complete {
		return nil, err
	}
	t, complete, err := p.thread(ctx, a, prefix, pr.Number, reviews)
	if err != nil || !complete {
		return nil, err
	}
	from := t.mark(1)
	if len(t.marks) > 0 && p.committedAfter(ctx, a, prefix, pr.Head.SHA, t.mark(0)) {
		from = t.mark(0)
	}
	if fresh := t.after(t.mark(0)); len(fresh) > 0 {
		if err := p.markTaken(ctx, a, prefix, pr.Number, len(fresh), fresh[len(fresh)-1].at); err != nil {
			return nil, err
		}
	}
	notes := t.after(from)
	if len(notes) > maxNotes {
		notes = notes[len(notes)-maxNotes:]
	}
	out := make([]issue.Feedback, len(notes))
	for i, n := range notes {
		out[i] = n.f
	}
	return out, nil
}

// committedAfter reports whether the commit was made after at. When that
// cannot be read the answer is no, which shows the agent a comment twice
// rather than not at all.
func (p *Publisher) committedAfter(ctx context.Context, a *api, prefix, sha string, at time.Time) bool {
	if !shaRE.MatchString(sha) {
		return false
	}
	var c struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := a.do(ctx, http.MethodGet, prefix+"/commits/"+sha, nil, nil, &c); err != nil {
		return false
	}
	when, err := time.Parse(time.RFC3339, c.Commit.Committer.Date)
	return err == nil && when.After(at)
}
