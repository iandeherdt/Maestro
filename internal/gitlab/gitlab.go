// Package gitlab holds the service's GitLab credential: a project access
// token, kept in a file only the operator can read, and what it says about
// itself and the project. The token comes with a bot user of its own, so
// the merge requests it opens are not the owner's and the owner can
// approve them.
//
// The token is renewed by the service before it expires (Rotate). GitLab
// then stops accepting the old one at once, so the new one is written to
// the file before anything else.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Default is gitlab.com.
const Default = "https://gitlab.com"

// Roles, as GitLab numbers them.
const (
	Guest      = 10
	Reporter   = 20
	Developer  = 30
	Maintainer = 40
	Owner      = 50
)

// pathRE is a project path: a group, any subgroups, and the project.
var pathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*){1,19}$`)

// ValidPath reports whether path names a project, such as group/sub/name.
func ValidPath(path string) bool {
	if !pathRE.MatchString(path) {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if strings.HasSuffix(seg, ".git") || strings.HasSuffix(seg, ".atom") || strings.Contains(seg, "..") {
			return false
		}
	}
	return true
}

// TokenFile is where the token is kept.
func TokenFile(dir string) string { return filepath.Join(dir, "gitlab.token") }

// ReadToken reads the stored token; "" without an error when there is none.
func ReadToken(dir string) (string, error) {
	raw, err := os.ReadFile(TokenFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// WriteToken stores the token, readable by the operator only. The file is
// replaced in one step, so that a reader sees the old token or the new one.
func WriteToken(dir, token string) error {
	if err := ValidToken(token); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".gitlab.token.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.WriteString(token + "\n")
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, TokenFile(dir))
}

// ValidToken refuses what cannot be a token.
func ValidToken(token string) error {
	if token == "" || len(token) > 1000 || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("that is not a token")
	}
	return nil
}

// Client talks to one GitLab instance with one token.
type Client struct {
	// Web is the instance, such as https://gitlab.com.
	Web   string
	Token string
	HTTP  *http.Client
}

// Error is a refusal by GitLab. Its text never holds GitLab's answer, which
// can quote the request.
type Error struct {
	Path   string
	Status int
}

func (e *Error) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "GitLab does not accept the token (it may have expired or been revoked)"
	case http.StatusForbidden:
		return "the token may not do this: " + e.Path
	case http.StatusNotFound:
		return "GitLab does not show " + e.Path + " to this token"
	}
	return fmt.Sprintf("GitLab answered %d for %s", e.Status, e.Path)
}

// CheckWeb accepts the address of an instance: https, a host, and at most
// a path for an instance below one.
func CheckWeb(web string) (string, error) {
	web = strings.TrimSuffix(strings.TrimSpace(web), "/")
	if web == "" {
		return Default, nil
	}
	u, err := url.Parse(web)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("that is not the address of a GitLab, such as https://gitlab.example.com")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return "", errors.New("the address must start with https://")
	}
	return web, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	web, err := CheckWeb(c.Web)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, web+"/api/v4"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if c.HTTP != nil {
		cp := *c.HTTP
		hc = &cp
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GitLab could not be reached: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// Only the first part of the path: the rest can be the project.
		return &Error{Path: strings.SplitN(path, "?", 2)[0], Status: res.StatusCode}
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("GitLab's answer could not be read")
	}
	return nil
}

// User is whom the token acts as.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// Who returns the token's user.
func (c *Client) Who(ctx context.Context) (User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return u, err
	}
	if u.ID <= 0 || u.Username == "" {
		return u, errors.New("GitLab did not say whom the token acts as")
	}
	return u, nil
}

// Info is what a token says about itself.
type Info struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"`
	Active    bool     `json:"active"`
}

// Expires is when the token stops working; the zero time when it does not.
func (i Info) Expires() time.Time {
	t, err := time.Parse("2006-01-02", i.ExpiresAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Self reads the token's own description.
func (c *Client) Self(ctx context.Context) (Info, error) {
	var i Info
	err := c.do(ctx, http.MethodGet, "/personal_access_tokens/self", nil, &i)
	return i, err
}

// Project is what the service needs to know about the project.
type Project struct {
	ID            int64  `json:"id"`
	Path          string `json:"path_with_namespace"`
	DefaultBranch string `json:"default_branch"`
	Visibility    string `json:"visibility"`
}

// Project reads a project by its path.
func (c *Client) Project(ctx context.Context, path string) (Project, error) {
	var p Project
	if err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(path), nil, &p); err != nil {
		return p, err
	}
	if p.ID <= 0 {
		return p, errors.New("GitLab did not describe the project")
	}
	return p, nil
}

// Role returns the access level of a user in a project, inherited ones
// included.
func (c *Client) Role(ctx context.Context, projectID, userID int64) (int, error) {
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%d/members/all/%d", projectID, userID), nil, &m)
	return m.AccessLevel, err
}

// Protection is how a branch is protected.
type Protection struct {
	Protected bool
	// Push and Merge are the lowest roles that may push to it and merge
	// into it; 0 means nobody.
	Push, Merge    int
	AllowForcePush bool
	// UserCanPush is true when a single user or a deploy key may push
	// besides the roles.
	UserCanPush bool
}

// Protected reads a branch's protection.
func (c *Client) Protected(ctx context.Context, projectID int64, branch string) (Protection, error) {
	var b struct {
		AllowForcePush bool `json:"allow_force_push"`
		Push           []struct {
			AccessLevel int    `json:"access_level"`
			UserID      *int64 `json:"user_id"`
			DeployKeyID *int64 `json:"deploy_key_id"`
		} `json:"push_access_levels"`
		Merge []struct {
			AccessLevel int    `json:"access_level"`
			UserID      *int64 `json:"user_id"`
		} `json:"merge_access_levels"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%d/protected_branches/%s", projectID, url.PathEscape(branch)), nil, &b)
	var ge *Error
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return Protection{}, nil
	}
	if err != nil {
		return Protection{}, err
	}
	p := Protection{Protected: true, AllowForcePush: b.AllowForcePush}
	lowest := func(levels []int) int {
		low := 0
		for _, l := range levels {
			if l > 0 && (low == 0 || l < low) {
				low = l
			}
		}
		return low
	}
	var push, merge []int
	for _, l := range b.Push {
		push = append(push, l.AccessLevel)
		p.UserCanPush = p.UserCanPush || l.UserID != nil || l.DeployKeyID != nil
	}
	for _, l := range b.Merge {
		merge = append(merge, l.AccessLevel)
		p.UserCanPush = p.UserCanPush || l.UserID != nil
	}
	p.Push, p.Merge = lowest(push), lowest(merge)
	return p, nil
}

// Rotate replaces the token with a new one that expires on until, and
// returns it. GitLab stops accepting the old token at once.
func (c *Client) Rotate(ctx context.Context, projectID int64, until time.Time) (string, Info, error) {
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	body := map[string]string{"expires_at": until.Format("2006-01-02")}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%d/access_tokens/self/rotate", projectID), body, &out)
	var ge *Error
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		// An older GitLab knows only the general endpoint.
		err = c.do(ctx, http.MethodPost, "/personal_access_tokens/self/rotate", body, &out)
	}
	if err != nil {
		return "", Info{}, err
	}
	if ValidToken(out.Token) != nil {
		return "", Info{}, errors.New("GitLab gave no usable new token")
	}
	return out.Token, Info{ExpiresAt: out.ExpiresAt, Active: true}, nil
}

// Source hands out the stored token and renews it before it expires.
type Source struct {
	Dir string
	Web string
	// ProjectID is the project the token belongs to.
	ProjectID int64
	HTTP      *http.Client
	Now       func() time.Time
	// Log hears about renewals.
	Log *slog.Logger

	mu      sync.Mutex
	current string
	checked time.Time
}

// RenewBefore is how long before it expires the token is renewed, and
// Lifetime how long a new one lasts.
const (
	RenewBefore = 30 * 24 * time.Hour
	Lifetime    = 365 * 24 * time.Hour
)

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token returns the token. Once a day it looks at when the token expires
// and renews it when that is close. A renewal that fails leaves the token
// as it was: it still works until it expires, and doctor says so.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == "" {
		t, err := ReadToken(s.Dir)
		if err != nil {
			return "", err
		}
		if t == "" {
			return "", errors.New("no GitLab token is stored; run `maestro gitlab token`")
		}
		s.current = t
	}
	if s.ProjectID > 0 && s.now().Sub(s.checked) > 24*time.Hour {
		s.checked = s.now()
		s.renew(ctx)
	}
	return s.current, nil
}

func (s *Source) renew(ctx context.Context) {
	c := &Client{Web: s.Web, Token: s.current, HTTP: s.HTTP}
	info, err := c.Self(ctx)
	if err != nil || info.Expires().IsZero() || info.Expires().Sub(s.now()) > RenewBefore {
		return
	}
	next, _, err := c.Rotate(ctx, s.ProjectID, s.now().Add(Lifetime))
	if err != nil {
		s.log().Warn("the GitLab token expires soon and could not be renewed", "expires", info.ExpiresAt, "error", err)
		return
	}
	// The old token no longer works: the new one is kept before anything
	// else, in memory when the file cannot be written.
	s.current = next
	if err := WriteToken(s.Dir, next); err != nil {
		s.log().Error("the GitLab token was renewed but could not be stored; it works until this service stops. "+
			"Store a new one with `maestro gitlab token`", "file", TokenFile(s.Dir), "error", err)
		return
	}
	s.log().Info("renewed the GitLab token", "until", s.now().Add(Lifetime).Format("2006-01-02"))
}

func (s *Source) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Current is the token in use, for the secret scan; "" before the first.
func (s *Source) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}
