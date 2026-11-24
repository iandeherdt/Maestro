package publish

import (
	"context"
	"fmt"
	"regexp"
	"sort"
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

var markRE = regexp.MustCompile(`<!-- (?:maestro|symphony):feedback-through (\S+) -->`)

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
func (p *Publisher) thread(ctx context.Context, h host, number int, reviews []prReview) (thread, bool, error) {
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
	list, complete, err := h.comments(ctx, number, reviews)
	if err != nil || !complete {
		return t, false, err
	}
	for _, c := range list {
		// Only the service's own comments can carry a mark.
		if strings.EqualFold(c.Login, p.BotLogin) {
			if m := markRE.FindStringSubmatch(c.Body); m != nil && c.Path == "" {
				if at, err := time.Parse(time.RFC3339, m[1]); err == nil {
					t.marks = append(t.marks, at)
				}
			}
			continue
		}
		add(c.Login, c.At, c.Body, c.Path, c.Line)
	}
	for _, r := range reviews {
		if !r.Pending {
			add(r.Login, r.At, r.Body, "", 0)
		}
	}

	sort.SliceStable(t.notes, func(i, j int) bool { return t.notes[i].at.Before(t.notes[j].at) })
	sort.Slice(t.marks, func(i, j int) bool { return t.marks[i].After(t.marks[j]) })
	return t, true, nil
}

// mark writes on the pull request that everything up to through was taken
// up.
func (p *Publisher) markTaken(ctx context.Context, h host, number, count int, through time.Time) error {
	what := "the new comment"
	if count > 1 {
		what = fmt.Sprintf("the %d new comments", count)
	}
	body := fmt.Sprintf("Picking up %s on this pull request. The ticket is back in work, and the changes will arrive here as a new commit.\n\n"+
		"<!-- maestro:feedback-through %s -->", what, through.UTC().Format(time.RFC3339))
	return h.comment(ctx, number, body)
}

// Acknowledge records on the pull request that the comments up to
// st.FeedbackThrough were taken up.
func (p *Publisher) Acknowledge(ctx context.Context, is issue.Issue, st orchestrator.ReviewStatus) error {
	h, err := p.host()
	if err != nil {
		return err
	}
	if st.PRNumber <= 0 || st.FeedbackThrough.IsZero() {
		return nil
	}
	return p.markTaken(ctx, h, st.PRNumber, st.FeedbackCount, st.FeedbackThrough)
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
	h, err := p.host()
	if err != nil {
		return nil, err
	}
	pr, err := p.pull(ctx, h, is)
	if err != nil || pr == nil || pr.State != changeOpen {
		return nil, err
	}
	reviews, complete, err := h.reviews(ctx, pr.Number)
	if err != nil || !complete {
		return nil, err
	}
	t, complete, err := p.thread(ctx, h, pr.Number, reviews)
	if err != nil || !complete {
		return nil, err
	}
	from := t.mark(1)
	if len(t.marks) > 0 && p.committedAfter(ctx, h, pr.HeadSHA, t.mark(0)) {
		from = t.mark(0)
	}
	if fresh := t.after(t.mark(0)); len(fresh) > 0 {
		if err := p.markTaken(ctx, h, pr.Number, len(fresh), fresh[len(fresh)-1].at); err != nil {
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
func (p *Publisher) committedAfter(ctx context.Context, h host, sha string, at time.Time) bool {
	if !shaRE.MatchString(sha) {
		return false
	}
	when, err := h.commitTime(ctx, sha)
	return err == nil && when.After(at)
}
