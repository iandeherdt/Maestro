package linear

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/ticketimage"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// Pictures on a ticket (SPEC 13.5). A picture pasted into a Linear ticket
// or comment is stored on uploads.linear.app, which answers only a request
// with the workspace's API key. The key is sent there and nowhere else.

const uploadsHost = "uploads.linear.app"

func linearImageHost(u *url.URL) bool { return strings.EqualFold(u.Hostname(), uploadsHost) }

// TicketImages downloads the pictures in the ticket's description and in
// the newest comments of allowlisted commenters.
func (a *Adapter) TicketImages(ctx context.Context, is issue.Issue) (ticketimage.Result, error) {
	allowed := a.imageHost
	if allowed == nil {
		allowed = linearImageHost
	}
	var refs []ticketimage.Ref
	for _, l := range ticketimage.Links(is.Description) {
		if u, err := url.Parse(l); err == nil && allowed(u) {
			refs = append(refs, ticketimage.Ref{URL: l, Where: "description"})
		}
	}
	type said struct {
		who  string
		at   time.Time
		body string
	}
	var comments []said
	err := a.comments(ctx, is.ID, func(c comment) {
		if c.User == nil || c.BotActor != nil {
			return
		}
		if !(a.commenters[norm(c.User.ID)] || (c.User.Email != "" && a.commenters[norm(c.User.Email)])) {
			return
		}
		at, _ := time.Parse(time.RFC3339, c.CreatedAt)
		who := c.User.Email
		if who == "" {
			who = c.User.ID
		}
		comments = append(comments, said{who, at, c.Body})
	})
	if err != nil {
		return ticketimage.Result{}, err
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].at.Before(comments[j].at) })
	if len(comments) > ticketimage.MaxComments {
		comments = comments[len(comments)-ticketimage.MaxComments:]
	}
	for _, c := range comments {
		for _, l := range ticketimage.Links(c.body) {
			if u, err := url.Parse(l); err == nil && allowed(u) {
				refs = append(refs, ticketimage.Ref{URL: l, Where: "comment", Author: c.who, At: c.at})
			}
		}
	}
	f := ticketimage.Fetcher{
		HTTP:    a.imageHTTP,
		Allowed: allowed,
		// Same as the API: a personal key as it is, an OAuth token with Bearer.
		Auth: func(ctx context.Context, _ *url.URL) (string, error) { return a.c.key(ctx) },
	}
	return f.Fetch(ctx, refs), nil
}

var _ tracker.Images = (*Adapter)(nil)
