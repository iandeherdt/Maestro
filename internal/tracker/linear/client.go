package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/iandeherdt/Maestro/internal/tracker"
)

const (
	maxBodyBytes = 8 << 20
	// maxPages bounds one paged read, so a broken cursor cannot loop.
	maxPages = 50
	pageSize = 50
)

// KeySource returns the API key for a request.
type KeySource func(ctx context.Context) (string, error)

// client speaks Linear's GraphQL API. It sends the key only to the
// configured endpoint and follows no redirects.
type client struct {
	endpoint *url.URL
	http     *http.Client
	key      KeySource
}

func newClient(endpoint *url.URL, hc *http.Client, key KeySource) *client {
	c := *hc
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{endpoint: endpoint, http: &c, key: key}
}

func terr(category, format string, a ...any) *tracker.Error {
	return &tracker.Error{Category: category, Msg: fmt.Sprintf(format, a...)}
}

// RateLimitError is tracker_rate_limited with the wait the API asked for.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: retry after %s", tracker.RateLimited, e.RetryAfter)
}

// Unwrap lets tracker.CategoryOf see the category.
func (e *RateLimitError) Unwrap() error {
	return &tracker.Error{Category: tracker.RateLimited, Msg: e.Error()}
}

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// do runs one query or mutation and decodes its data into out.
func (c *client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	key, err := c.key(ctx)
	if err != nil {
		return terr(tracker.MissingSecret, "the Linear API key: %v", err)
	}
	if key == "" {
		return terr(tracker.MissingSecret, "no Linear API key")
	}
	raw, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return terr(tracker.Request, "%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A personal API key is sent as it is; an OAuth token with Bearer.
	req.Header.Set("Authorization", key)
	res, err := c.http.Do(req)
	if err != nil {
		return terr(tracker.Request, "%v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil {
		return terr(tracker.Request, "%v", err)
	}
	if len(body) > maxBodyBytes {
		return terr(tracker.Response, "the answer is larger than %d bytes", maxBodyBytes)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	jsonErr := json.Unmarshal(body, &env)
	for _, e := range env.Errors {
		if e.Extensions.Code == "RATELIMITED" {
			return &RateLimitError{RetryAfter: retryAfter(res)}
		}
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		return &RateLimitError{RetryAfter: retryAfter(res)}
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return terr(tracker.Status, "Linear refused the API key (%d)", res.StatusCode)
	case res.StatusCode/100 == 3:
		return terr(tracker.Status, "Linear answered with a redirect (%d), which is not followed", res.StatusCode)
	case res.StatusCode >= 500:
		return terr(tracker.Status, "Linear answered %d", res.StatusCode)
	case jsonErr != nil:
		return terr(tracker.Response, "the answer is not JSON (status %d)", res.StatusCode)
	case len(env.Errors) > 0:
		msg := env.Errors[0].Message
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return terr(tracker.Status, "Linear: %s", msg)
	case res.StatusCode != http.StatusOK:
		return terr(tracker.Status, "Linear answered %d", res.StatusCode)
	case len(env.Data) == 0 || string(env.Data) == "null":
		return terr(tracker.Response, "the answer has no data")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return terr(tracker.Response, "the answer has an unexpected shape: %v", err)
	}
	return nil
}

func retryAfter(res *http.Response) time.Duration {
	if s := res.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if s := res.Header.Get("X-RateLimit-Requests-Reset"); s != "" {
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			if d := time.Until(time.UnixMilli(ms)); d > 0 {
				return d
			}
		}
	}
	return time.Minute
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// pages runs a paged query: vars gets "after", and next decodes one page
// and returns its pageInfo.
func (c *client) pages(ctx context.Context, query string, vars map[string]any, next func(json.RawMessage) (pageInfo, error)) error {
	after := ""
	for i := 0; ; i++ {
		if i == maxPages {
			return terr(tracker.Pagination, "more than %d pages", maxPages)
		}
		v := map[string]any{}
		for k, x := range vars {
			v[k] = x
		}
		if after != "" {
			v["after"] = after
		} else {
			v["after"] = nil
		}
		var data json.RawMessage
		if err := c.do(ctx, query, v, &data); err != nil {
			return err
		}
		pi, err := next(data)
		if err != nil {
			return err
		}
		if !pi.HasNextPage {
			return nil
		}
		if pi.EndCursor == "" || pi.EndCursor == after {
			return terr(tracker.Pagination, "the next page has no cursor")
		}
		after = pi.EndCursor
	}
}

var errNotFound = errors.New("not found")
