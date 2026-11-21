// Package ghapp mints short-lived GitHub App installation tokens (SPEC
// section 15.5).
//
// The App's private key is the one long-lived credential the service
// holds. It is read from a file only the service user can read, it is
// never logged, and it never leaves this package. Everything else works
// with installation tokens, which expire after an hour and can be limited
// to the repositories and permissions a caller needs.
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sync"
	"syscall"
	"time"
)

const (
	defaultAPI = "https://api.github.com"
	apiVersion = "2022-11-28"
	// renewBefore is how long before expiry a token is replaced. A git
	// push or a long request started with the token must be able to finish.
	renewBefore = 10 * time.Minute
	maxKeyBytes = 64 << 10
	maxResponse = 1 << 20
)

// Config identifies the App installation.
type Config struct {
	// AppID is the App's numeric ID or its client ID.
	AppID string
	// InstallationID is the numeric ID of the installation on the account.
	InstallationID string
	// KeyFile is the path of the App's private key in PEM form.
	KeyFile string
	// APIURL defaults to https://api.github.com.
	APIURL string
	HTTP   *http.Client
	Now    func() time.Time
}

// Scope narrows a token. The zero value asks for everything the
// installation has.
type Scope struct {
	// Repositories are repository names without the owner.
	Repositories []string
	// Permissions maps a permission name to "read" or "write", for
	// example {"contents": "write", "pull_requests": "write"}.
	Permissions map[string]string
}

// App mints installation tokens.
type App struct {
	cfg  Config
	key  *rsa.PrivateKey
	base *url.URL
	hc   *http.Client
	now  func() time.Time
}

var (
	idRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	numRE  = regexp.MustCompile(`^[0-9]{1,20}$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	permRE = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

// New reads the key and validates the configuration. It makes no request.
func New(cfg Config) (*App, error) {
	if !idRE.MatchString(cfg.AppID) {
		return nil, errors.New("ghapp: the App ID is missing or malformed")
	}
	if !numRE.MatchString(cfg.InstallationID) {
		return nil, errors.New("ghapp: the installation ID is missing or malformed")
	}
	api := cfg.APIURL
	if api == "" {
		api = defaultAPI
	}
	base, err := url.Parse(api)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("ghapp: the API URL is not valid")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback(base.Hostname())) {
		return nil, errors.New("ghapp: the API URL must use https")
	}
	key, err := loadKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if cfg.HTTP != nil {
		cp := *cfg.HTTP
		hc = &cp
	}
	// The App's signed assertion must not follow a redirect to another host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &App{cfg: cfg, key: key, base: base, hc: hc, now: now}, nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// loadKey reads the private key. The file must be a regular file that only
// its owner can read: a key readable by other users of the machine is a
// configuration error worth refusing to start for.
func loadKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("ghapp: no key file is configured")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ghapp: the key file cannot be opened: %w", err)
	}
	f := os.NewFile(uintptr(fd), "key")
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ghapp: the key file cannot be read: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("ghapp: the key file is not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("ghapp: the key file is accessible to other users (mode %04o), it must be 0600 or stricter", st.Mode().Perm())
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("ghapp: the key file cannot be read: %w", err)
	}
	if len(raw) > maxKeyBytes {
		return nil, errors.New("ghapp: the key file is too large to be a private key")
	}
	// Errors below never quote the file's content.
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("ghapp: the key file does not contain a PEM block")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var any interface{}
		any, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			if key, ok = any.(*rsa.PrivateKey); !ok {
				return nil, errors.New("ghapp: the key is not an RSA key")
			}
		}
	default:
		return nil, errors.New("ghapp: the key file does not contain a private key")
	}
	if err != nil {
		return nil, errors.New("ghapp: the private key cannot be parsed")
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("ghapp: the private key is shorter than 2048 bits")
	}
	return key, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// assertion is the signed statement that proves the service is the App. It
// is valid for a few minutes and is used once.
func (a *App) assertion() (string, error) {
	now := a.now()
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iss": a.cfg.AppID,
		"iat": now.Add(-time.Minute).Unix(), // allow for clock drift
		"exp": now.Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signed := header + "." + b64(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("ghapp: signing failed")
	}
	return signed + "." + b64(sig), nil
}

// Error is a failed token request. It carries no credential.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("ghapp: %s (status %d)", e.Msg, e.Status)
	}
	return "ghapp: " + e.Msg
}

func (a *App) mint(ctx context.Context, scope Scope) (string, time.Time, error) {
	jwt, err := a.assertion()
	if err != nil {
		return "", time.Time{}, err
	}
	body := map[string]any{}
	if len(scope.Repositories) > 0 {
		body["repositories"] = scope.Repositories
	}
	if len(scope.Permissions) > 0 {
		body["permissions"] = scope.Permissions
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", time.Time{}, err
	}
	u := *a.base
	u.Path = u.Path + "/app/installations/" + a.cfg.InstallationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		// A transport error can quote the URL, never the header.
		return "", time.Time{}, &Error{Msg: "the token request failed: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode != http.StatusCreated {
		msg := "the token request was refused"
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg = "GitHub did not accept the App's key or ID, or this machine's clock is wrong"
		case http.StatusNotFound:
			msg = "the installation was not found for this App"
		case http.StatusUnprocessableEntity:
			msg = "the installation does not have a requested repository or permission"
		case http.StatusForbidden, http.StatusTooManyRequests:
			msg = "the token request was refused or rate limited"
		}
		return "", time.Time{}, &Error{Status: resp.StatusCode, Msg: msg}
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" || out.ExpiresAt.IsZero() {
		return "", time.Time{}, &Error{Msg: "the token response could not be read"}
	}
	return out.Token, out.ExpiresAt, nil
}

// Source hands out one token for a scope and renews it before it expires.
// It is safe for concurrent use.
type Source struct {
	app   *App
	scope Scope

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Source returns a token source for the scope. The scope is validated here
// so that a malformed name fails at startup.
func (a *App) Source(scope Scope) (*Source, error) {
	for _, r := range scope.Repositories {
		if !repoRE.MatchString(r) {
			return nil, errors.New("ghapp: a repository name in the scope is malformed (use the name without the owner)")
		}
	}
	for k, v := range scope.Permissions {
		if !permRE.MatchString(k) || (v != "read" && v != "write") {
			return nil, errors.New("ghapp: a permission in the scope is malformed")
		}
	}
	return &Source{app: a, scope: scope}, nil
}

// Token returns a token valid for at least a few more minutes.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.app.now().Before(s.expires.Add(-renewBefore)) {
		return s.token, nil
	}
	tok, exp, err := s.app.mint(ctx, s.scope)
	if err != nil {
		// A token that is still valid beats no token during an outage.
		if s.token != "" && s.app.now().Before(s.expires.Add(-time.Minute)) {
			return s.token, nil
		}
		return "", err
	}
	s.token, s.expires = tok, exp
	return tok, nil
}

// Current returns the token last handed out, if any. It lets the secret
// scan search for the value without requesting a new one.
func (s *Source) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}
