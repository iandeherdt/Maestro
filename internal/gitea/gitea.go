// Package gitea holds the service's Gitea or Forgejo credential: the token
// of a user account of its own, kept in a file only the operator can read,
// and what it says about the account and the repository. The account is
// not the owner's, so the pull requests it opens are not the owner's and
// the owner can approve them.
//
// Forgejo keeps Gitea's API, so everything here works on both.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// repoRE is owner/name.
var repoRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}$`)

// ValidRepo reports whether path names a repository, such as owner/name.
func ValidRepo(path string) bool {
	if !repoRE.MatchString(path) {
		return false
	}
	_, name, _ := strings.Cut(path, "/")
	return !strings.HasSuffix(name, ".git") && !strings.Contains(path, "..") && name != "."
}

// TokenFile is where the token is kept.
func TokenFile(dir string) string { return filepath.Join(dir, "gitea.token") }

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
// replaced in one step.
func WriteToken(dir, token string) error {
	if err := ValidToken(token); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".gitea.token.*")
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

// CheckWeb accepts the address of an instance: https, a host, and at most
// a path for an instance below one. There is no default: Gitea and Forgejo
// are run by many.
func CheckWeb(web string) (string, error) {
	web = strings.TrimSuffix(strings.TrimSpace(web), "/")
	if web == "" {
		return "", errors.New("give the address of your Gitea or Forgejo, such as https://git.example.com")
	}
	u, err := url.Parse(web)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("that is not the address of a Gitea or Forgejo, such as https://git.example.com")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return "", errors.New("the address must start with https://")
	}
	return web, nil
}

// Client talks to one instance with one token.
type Client struct {
	// Web is the instance, such as https://git.example.com.
	Web   string
	Token string
	HTTP  *http.Client
}

// Error is a refusal by the instance. Its text never holds the answer,
// which can quote the request.
type Error struct {
	Path   string
	Status int
}

func (e *Error) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Gitea does not accept the token (it may have been deleted)"
	case http.StatusForbidden:
		return "the token may not do this: " + e.Path + " (check its scopes)"
	case http.StatusNotFound:
		return "Gitea does not show " + e.Path + " to this token"
	}
	return fmt.Sprintf("Gitea answered %d for %s", e.Status, e.Path)
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
	req, err := http.NewRequestWithContext(ctx, method, web+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/json")
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
		return fmt.Errorf("Gitea could not be reached: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &Error{Path: strings.SplitN(path, "?", 2)[0], Status: res.StatusCode}
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("Gitea's answer could not be read")
	}
	return nil
}

// User is whom the token acts as.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	// Admin is a site administrator, who could do anything anywhere.
	Admin bool `json:"is_admin"`
}

// Who returns the token's user.
func (c *Client) Who(ctx context.Context) (User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return u, err
	}
	if u.ID <= 0 || u.Login == "" {
		return u, errors.New("Gitea did not say whom the token acts as")
	}
	return u, nil
}

// Repo is what the service needs to know about the repository.
type Repo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
	Permissions   struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Repo reads a repository, with the token's own rights on it.
func (c *Client) Repo(ctx context.Context, path string) (Repo, error) {
	var r Repo
	owner, name, ok := strings.Cut(path, "/")
	if !ok || !ValidRepo(path) {
		return r, errors.New("the repository is named owner/name")
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &r); err != nil {
		return r, err
	}
	if r.ID <= 0 {
		return r, errors.New("Gitea did not describe the repository")
	}
	return r, nil
}

// Protection is what a branch allows the token's user.
type Protection struct {
	Protected bool
	// UserCanPush and UserCanMerge are for the token's own user.
	UserCanPush, UserCanMerge bool
}

// Branch reads whether a branch is protected against the token's user.
func (c *Client) Branch(ctx context.Context, path, branch string) (Protection, error) {
	var b struct {
		Protected    bool `json:"protected"`
		UserCanPush  bool `json:"user_can_push"`
		UserCanMerge bool `json:"user_can_merge"`
	}
	owner, name, _ := strings.Cut(path, "/")
	err := c.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/branches/"+url.PathEscape(branch), nil, &b)
	return Protection{Protected: b.Protected, UserCanPush: b.UserCanPush, UserCanMerge: b.UserCanMerge}, err
}

// Usable says why the token's user must not be the service's, or "" when
// it may: it must be able to push, and must be neither a site
// administrator nor an administrator of the repository, who could merge and
// change the protection.
func Usable(u User, r Repo, reviewer string) string {
	switch {
	case u.Admin:
		return "the token's user " + u.Login + " is a site administrator; give the service an ordinary account"
	case r.Permissions.Admin || strings.EqualFold(r.Owner.Login, u.Login):
		return "the token's user " + u.Login + " administers " + r.FullName + " and could merge and change the protection; " +
			"add the service's account as a collaborator with Write"
	case reviewer != "" && strings.EqualFold(reviewer, u.Login):
		return "the token is your own: what the service did would count as your approval; make an account for the service"
	case !r.Permissions.Push:
		return "the token's user " + u.Login + " cannot push to " + r.FullName + "; add it as a collaborator with Write"
	}
	return ""
}
