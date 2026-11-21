package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

const (
	apiVersion   = "2022-11-28"
	maxBodyBytes = 8 << 20
	maxPages     = 50
	perPage      = 100
)

// TokenSource returns the credential for a request. It is called for every
// request, so that short-lived installation tokens can be renewed.
type TokenSource func(ctx context.Context) (string, error)

// client is a minimal GitHub REST client. It sends the token only to the
// configured API origin: redirects are not followed, and pagination links
// that point anywhere else are refused.
type client struct {
	base  *url.URL
	http  *http.Client
	token TokenSource
}

func newClient(base *url.URL, hc *http.Client, ts TokenSource) *client {
	c := *hc
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{base: base, http: &c, token: ts}
}

func terr(category, format string, a ...any) *tracker.Error {
	return &tracker.Error{Category: category, Msg: fmt.Sprintf(format, a...)}
}

// RateLimitError is tracker_rate_limited with the wait the API asked for.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: retry after %s", tracker.RateLimited, e.RetryAfter)
}

// Unwrap lets tracker.CategoryOf classify the error.
func (e *RateLimitError) Unwrap() error {
	return &tracker.Error{Category: tracker.RateLimited, Msg: "rate limited"}
}

// notFound is returned for 404 so callers can tell "gone" from "broken".
var errNotFound = errors.New("not found")

func (c *client) url(path string, query url.Values) string {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query.Encode()
	return u.String()
}

// sameOrigin reports whether raw points at the configured API origin.
func (c *client) sameOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == c.base.Scheme && u.Host == c.base.Host && u.User == nil
}

// do performs one request. write selects the error category for a refusal.
func (c *client) do(ctx context.Context, method, fullURL string, body any, out any, write bool) (http.Header, error) {
	if !c.sameOrigin(fullURL) {
		return nil, terr(tracker.Pagination, "refusing a request outside the configured API origin")
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, terr(tracker.Request, "encode request: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rd)
	if err != nil {
		return nil, terr(tracker.Request, "build request: %v", err)
	}
	tok, err := c.token(ctx)
	if err != nil || tok == "" {
		return nil, terr(tracker.MissingSecret, "no token available")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "symphony-orchestrator")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		// The error text of net/http includes the URL but never headers.
		return nil, terr(tracker.Request, "%s %s: %v", method, req.URL.Path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil {
		return nil, terr(tracker.Request, "read response: %v", err)
	}
	if len(raw) > maxBodyBytes {
		return nil, terr(tracker.Response, "response larger than %d bytes", maxBodyBytes)
	}

	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
	case res.StatusCode == http.StatusNotFound:
		return res.Header, errNotFound
	case res.StatusCode == http.StatusTooManyRequests,
		res.StatusCode == http.StatusForbidden && (res.Header.Get("Retry-After") != "" || res.Header.Get("X-RateLimit-Remaining") == "0"):
		return res.Header, &RateLimitError{RetryAfter: retryAfter(res.Header)}
	case res.StatusCode >= 300 && res.StatusCode < 400:
		return res.Header, terr(tracker.Status, "%s %s: unexpected redirect (%d), not followed", method, req.URL.Path, res.StatusCode)
	case write && res.StatusCode >= 400 && res.StatusCode < 500:
		return res.Header, terr(tracker.WriteRejected, "%s %s: status %d", method, req.URL.Path, res.StatusCode)
	default:
		return res.Header, terr(tracker.Status, "%s %s: status %d", method, req.URL.Path, res.StatusCode)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.Header, terr(tracker.Response, "%s %s: malformed response", method, req.URL.Path)
		}
	}
	return res.Header, nil
}

func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if d := time.Until(time.Unix(reset, 0)); d > 0 {
			return d
		}
	}
	return time.Minute
}

var nextLinkRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// list fetches every page of a list endpoint and calls each with one page.
func (c *client) list(ctx context.Context, path string, query url.Values, each func(page json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", strconv.Itoa(perPage))
	next := c.url(path, query)
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return terr(tracker.Pagination, "%s: more than %d pages", path, maxPages)
		}
		var raw json.RawMessage
		h, err := c.do(ctx, http.MethodGet, next, nil, &raw, false)
		if err != nil {
			return err // errNotFound included: the caller decides what a 404 means
		}
		if err := each(raw); err != nil {
			return err
		}
		next = ""
		if m := nextLinkRE.FindStringSubmatch(h.Get("Link")); m != nil {
			if !c.sameOrigin(m[1]) {
				return terr(tracker.Pagination, "%s: next page points outside the API origin", path)
			}
			next = m[1]
		}
	}
	return nil
}
