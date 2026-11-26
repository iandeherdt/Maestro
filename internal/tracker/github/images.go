package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/ticketimage"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// Pictures on an issue (SPEC 13.5). A picture pasted into an issue is a
// github.com/user-attachments link. In a private repository that link only
// works for a signed-in person, so the service asks the API for the body as
// HTML too: there every picture has a signed download link that works for a
// few minutes without credentials. The token is never sent with a download.

type ghHTMLBody struct {
	BodyHTML  string  `json:"body_html"`
	User      *ghUser `json:"user"`
	CreatedAt string  `json:"created_at"`
}

// githubImageHost reports whether u is one of GitHub's own hosts for
// pictures on issues.
func githubImageHost(u *url.URL) bool {
	switch strings.ToLower(u.Hostname()) {
	case "private-user-images.githubusercontent.com", "user-images.githubusercontent.com",
		"camo.githubusercontent.com":
		return true
	case "github.com":
		return strings.HasPrefix(u.Path, "/user-attachments/")
	}
	return false
}

// TicketImages downloads the pictures in the issue's description and in the
// newest comments of allowlisted commenters.
func (a *Adapter) TicketImages(ctx context.Context, is issue.Issue) (ticketimage.Result, error) {
	n, err := numberOf(is)
	if err != nil {
		return ticketimage.Result{}, err
	}
	var body ghHTMLBody
	if _, err := a.c.doAs(ctx, http.MethodGet, a.c.url(a.path("/issues/%d", n), nil), nil, &body, false, mediaFull); err != nil {
		if errors.Is(err, errNotFound) {
			return ticketimage.Result{}, terr(tracker.Status, "issue %d is not readable", n)
		}
		return ticketimage.Result{}, err
	}
	var refs []ticketimage.Ref
	for _, src := range ticketimage.HTMLImages(body.BodyHTML) {
		refs = append(refs, ticketimage.Ref{URL: src, Where: "description"})
	}
	var comments []ghHTMLBody
	err = a.c.listAs(ctx, a.path("/issues/%d/comments", n), nil, mediaFull, func(page json.RawMessage) error {
		var cs []ghHTMLBody
		if err := json.Unmarshal(page, &cs); err != nil {
			return terr(tracker.Response, "comment list is not an array")
		}
		for _, c := range cs {
			if c.User != nil && a.commenters[norm(c.User.Login)] {
				comments = append(comments, c)
			}
		}
		return nil
	})
	if errors.Is(err, errNotFound) {
		err = terr(tracker.Status, "comments of issue %d are not readable", n)
	}
	if err != nil {
		return ticketimage.Result{}, err
	}
	if len(comments) > ticketimage.MaxComments {
		comments = comments[len(comments)-ticketimage.MaxComments:]
	}
	for _, c := range comments {
		at, _ := time.Parse(time.RFC3339, c.CreatedAt)
		for _, src := range ticketimage.HTMLImages(c.BodyHTML) {
			refs = append(refs, ticketimage.Ref{URL: src, Where: "comment", Author: c.User.Login, At: at})
		}
	}
	f := ticketimage.Fetcher{HTTP: a.imageHTTP, Allowed: a.imageHost}
	if f.Allowed == nil {
		f.Allowed = githubImageHost
	}
	return f.Fetch(ctx, refs), nil
}

var _ tracker.Images = (*Adapter)(nil)
