package publish

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
	"strings"
	"unicode"

	"github.com/iandeherdt/Maestro/internal/tracker"
)

const (
	apiVersion    = "2022-11-28"
	maxResponse   = 4 << 20
	maxTitleRunes = 120
	footer        = "Opened by Symphony. A person reviews and merges; the service never does."
)

// api is a minimal GitHub REST client for pull requests. The token goes
// only to the configured origin and redirects are not followed.
type api struct {
	base  *url.URL
	hc    *http.Client
	token func(context.Context) (string, error)
}

type apiError struct {
	Method, Path string
	Status       int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: status %d", e.Method, e.Path, e.Status)
}

func (a *api) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *a.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	tok, err := a.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The response text is not passed on: it can quote request content.
		return &apiError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: the response could not be read", method, path)
		}
	}
	return nil
}

type pull struct {
	Number int `json:"number"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// upsert creates the pull request for a branch or updates the open one.
func (a *api) upsert(ctx context.Context, owner, repo, branch, base, title, body string, draft bool) (int, error) {
	prefix := "/repos/" + owner + "/" + repo
	if base == "" {
		var r struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := a.do(ctx, http.MethodGet, prefix, nil, nil, &r); err != nil {
			return 0, err
		}
		if r.DefaultBranch == "" {
			return 0, errors.New("the repository reports no default branch")
		}
		base = r.DefaultBranch
	}
	if base == branch {
		return 0, errors.New("the head branch is the base branch")
	}
	var open []pull
	q := url.Values{"head": {owner + ":" + branch}, "state": {"open"}, "per_page": {"10"}}
	if err := a.do(ctx, http.MethodGet, prefix+"/pulls", q, nil, &open); err != nil {
		return 0, err
	}
	for _, p := range open {
		// The filter is by user and branch; confirm rather than assume.
		if p.Head.Ref != branch || p.Number <= 0 {
			continue
		}
		if p.Base.Ref != base {
			return 0, fmt.Errorf("an open pull request for this branch targets %q instead of the base branch", clean(p.Base.Ref, 100))
		}
		err := a.do(ctx, http.MethodPatch, prefix+"/pulls/"+strconv.Itoa(p.Number), nil,
			map[string]any{"title": title, "body": body}, nil)
		return p.Number, err
	}
	var created pull
	err := a.do(ctx, http.MethodPost, prefix+"/pulls", nil,
		map[string]any{"title": title, "head": branch, "base": base, "body": body, "draft": draft}, &created)
	if err != nil {
		return 0, err
	}
	if created.Number <= 0 {
		return 0, errors.New("the created pull request has no number")
	}
	return created.Number, nil
}

// clean removes control and invisible characters and caps the length.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// title builds the pull request title. The issue title is the board's
// text: mentions and references in it are defused.
func title(identifier, issueTitle string) string {
	t := clean(identifier+": "+issueTitle, maxTitleRunes)
	t = strings.NewReplacer("@", "@\u200b", "#", "#\u200b").Replace(t)
	return t
}

// code renders untrusted text as inline code that cannot break out.
func code(s string) string {
	// GitHub does not link mentions inside code, the break is a second layer.
	return "`" + strings.NewReplacer("`", "'", "@", "@\u200b").Replace(clean(s, 300)) + "`"
}

// Proof is what the service itself established about the work.
type Proof struct {
	VerifyCommand string
	VerifyStatus  int
	Worker        string
	Commits       int
	Files         int
	// Screenshots the service took, and what it has to say about them.
	Screenshots    []shotLink
	ScreenshotNote string
}

// body builds the pull request description from a fixed template. Nothing
// the agent wrote is placed outside inline code or a fenced block.
func body(closes string, flags []flagLine, p Proof, summary string) string {
	var b strings.Builder
	if closes != "" {
		b.WriteString(closes + "\n\n")
	}
	b.WriteString("## Review flags\n\n")
	if len(flags) == 0 {
		b.WriteString("None.\n")
	}
	for _, f := range flags {
		b.WriteString("- **" + f.rule + "** " + f.detail)
		if f.path != "" {
			b.WriteString(": " + code(f.path))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Checked by the service\n\n")
	if strings.TrimSpace(p.VerifyCommand) == "" {
		b.WriteString("- No verify command is configured.\n")
	} else {
		fmt.Fprintf(&b, "- Verify command exited with status %d:\n\n", p.VerifyStatus)
		b.WriteString(indent(tracker.Fence(p.VerifyCommand)) + "\n")
	}
	fmt.Fprintf(&b, "- %d commit(s), %d file(s) changed. Diff policy and secret scan passed.\n", p.Commits, p.Files)
	if p.Worker != "" {
		b.WriteString("- Worker: " + code(p.Worker) + "\n")
	}
	if len(p.Screenshots) > 0 || p.ScreenshotNote != "" {
		b.WriteString("\n## Screenshots\n\n")
		if len(p.Screenshots) > 0 {
			b.WriteString("Taken by the service after the checks passed, from the commits in this pull request. " +
				"They show what the code draws; nobody has judged them yet.\n")
		}
		for i, s := range p.Screenshots {
			fmt.Fprintf(&b, "\n%s\n\n![Screenshot %d](%s)\n", code(s.caption), i+1, s.url)
		}
		if p.ScreenshotNote != "" {
			if len(p.Screenshots) > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.ScreenshotNote + "\n")
		}
	}
	if strings.TrimSpace(summary) != "" {
		b.WriteString("\n## Agent's summary (not verified)\n\n" + tracker.Fence(summary) + "\n")
	}
	b.WriteString("\n---\n" + footer + "\n")
	return b.String()
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

type flagLine struct{ rule, path, detail string }
