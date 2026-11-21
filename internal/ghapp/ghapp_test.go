package ghapp

import (
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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	ctx     = context.Background()
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		testKey = k
	})
	return testKey
}

func writeKey(t *testing.T, k *rsa.PrivateKey, pkcs8 bool, mode os.FileMode) string {
	t.Helper()
	var block *pem.Block
	if pkcs8 {
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	}
	p := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

// gh is a stand-in for the one endpoint the package calls.
type gh struct {
	t       *testing.T
	mu      sync.Mutex
	calls   int
	bodies  []string
	status  int
	ttl     time.Duration
	now     func() time.Time
	srv     *httptest.Server
	lastJWT string
}

func newGH(t *testing.T, now func() time.Time) *gh {
	g := &gh{t: t, ttl: time.Hour, now: now}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gh) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
		w.WriteHeader(404)
		return
	}
	jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.lastJWT = jwt
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		w.WriteHeader(401)
		return
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if rsa.VerifyPKCS1v15(&key(g.t).PublicKey, crypto.SHA256, sum[:], sig) != nil {
		w.WriteHeader(401)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(raw, &claims)
	now := g.now().Unix()
	// GitHub's rules: issued in the past, expiring within ten minutes.
	if claims.Iss != "12345" || claims.Iat > now || claims.Exp <= now || claims.Exp-now > 600 {
		w.WriteHeader(401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	g.bodies = append(g.bodies, string(body))
	if g.status != 0 {
		w.WriteHeader(g.status)
		io.WriteString(w, `{"message":"secret detail from the server"}`)
		return
	}
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]any{
		"token":      "ghs_minted_" + strings.Repeat("x", g.calls),
		"expires_at": g.now().Add(g.ttl).UTC().Format(time.RFC3339),
	})
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func setup(t *testing.T) (*App, *gh, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	g := newGH(t, c.now)
	a, err := New(Config{AppID: "12345", InstallationID: "42", KeyFile: writeKey(t, key(t), false, 0o600),
		APIURL: g.srv.URL, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return a, g, c
}

func TestTokenIsMintedCachedAndRenewed(t *testing.T) {
	a, g, c := setup(t)
	s, err := a.Source(Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Current() != "" {
		t.Error("a token before the first request")
	}
	first, err := s.Token(ctx)
	if err != nil || first != "ghs_minted_x" {
		t.Fatalf("%q %v", first, err)
	}
	c.add(49 * time.Minute)
	if again, _ := s.Token(ctx); again != first || g.calls != 1 {
		t.Fatalf("not cached: %q after %d calls", again, g.calls)
	}
	// Eleven minutes are left... ten is the margin.
	c.add(2 * time.Minute)
	second, err := s.Token(ctx)
	if err != nil || second == first || g.calls != 2 {
		t.Fatalf("not renewed: %q %v after %d calls", second, err, g.calls)
	}
	if s.Current() != second {
		t.Error("Current does not return the token in use")
	}
	if g.bodies[0] != "{}" {
		t.Errorf("an unscoped request sent %s", g.bodies[0])
	}
}

func TestScopeIsSentAndValidated(t *testing.T) {
	a, g, _ := setup(t)
	s, err := a.Source(Scope{Repositories: []string{"app"}, Permissions: map[string]string{"contents": "write", "pull_requests": "write"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	json.Unmarshal([]byte(g.bodies[0]), &sent)
	if len(sent.Repositories) != 1 || sent.Repositories[0] != "app" || sent.Permissions["contents"] != "write" || len(sent.Permissions) != 2 {
		t.Errorf("sent %s", g.bodies[0])
	}
	for name, sc := range map[string]Scope{
		"owner in name": {Repositories: []string{"ian/app"}},
		"empty name":    {Repositories: []string{""}},
		"bad level":     {Permissions: map[string]string{"contents": "admin"}},
		"bad name":      {Permissions: map[string]string{"Contents!": "read"}},
	} {
		if _, err := a.Source(sc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConcurrentCallersShareOneRequest(t *testing.T) {
	a, g, _ := setup(t)
	s, _ := a.Source(Scope{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Token(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if g.calls != 1 {
		t.Errorf("%d token requests", g.calls)
	}
}

func TestFailures(t *testing.T) {
	for status, part := range map[int]string{401: "clock", 404: "installation", 422: "repository or permission", 403: "refused", 500: "refused"} {
		a, g, _ := setup(t)
		g.status = status
		s, _ := a.Source(Scope{})
		_, err := s.Token(ctx)
		var e *Error
		if !errors.As(err, &e) || e.Status != status || !strings.Contains(e.Msg, part) {
			t.Errorf("status %d: %v", status, err)
		}
		if err != nil && (strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), g.lastJWT)) {
			t.Errorf("status %d: the error repeats server text or the assertion: %v", status, err)
		}
	}
}

func TestOutageKeepsAValidTokenAndThenFails(t *testing.T) {
	a, g, c := setup(t)
	s, _ := a.Source(Scope{})
	first, _ := s.Token(ctx)
	g.mu.Lock()
	g.status = 500
	g.mu.Unlock()
	c.add(55 * time.Minute) // inside the renewal margin, still valid
	if tok, err := s.Token(ctx); err != nil || tok != first {
		t.Fatalf("a still valid token was dropped: %q %v", tok, err)
	}
	c.add(5 * time.Minute) // expired
	if tok, err := s.Token(ctx); err == nil {
		t.Fatalf("an expired token was handed out: %q", tok)
	}
}

func TestBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":  "<html>",
		"no token":  `{"expires_at":"2026-11-21T13:00:00Z"}`,
		"no expiry": `{"token":"ghs_x"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			io.WriteString(w, body)
		}))
		a, err := New(Config{AppID: "12345", InstallationID: "42", KeyFile: writeKey(t, key(t), false, 0o600), APIURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		s, _ := a.Source(Scope{})
		if _, err := s.Token(ctx); err == nil {
			t.Errorf("%s: accepted", name)
		}
		srv.Close()
	}
}

func TestAssertionIsNotSentAcrossARedirect(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("Authorization") != ""
		w.WriteHeader(201)
		io.WriteString(w, `{"token":"ghs_evil","expires_at":"2030-01-01T00:00:00Z"}`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	a, err := New(Config{AppID: "12345", InstallationID: "42", KeyFile: writeKey(t, key(t), false, 0o600), APIURL: srv.URL,
		HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.Source(Scope{})
	if tok, err := s.Token(ctx); err == nil || leaked {
		t.Fatalf("redirect followed: token %q, leaked %v", tok, leaked)
	}
}

func TestKeyFileChecks(t *testing.T) {
	good := Config{AppID: "12345", InstallationID: "42"}
	try := func(path string) error {
		c := good
		c.KeyFile = path
		_, err := New(c)
		return err
	}
	if err := try(writeKey(t, key(t), true, 0o600)); err != nil {
		t.Errorf("PKCS#8 key refused: %v", err)
	}
	if err := try(writeKey(t, key(t), false, 0o400)); err != nil {
		t.Errorf("read-only key refused: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660} {
		if err := try(writeKey(t, key(t), false, mode)); err == nil || !strings.Contains(err.Error(), "other users") {
			t.Errorf("mode %o: %v", mode, err)
		}
	}
	dir := t.TempDir()
	real := writeKey(t, key(t), false, 0o600)
	link := filepath.Join(dir, "link.pem")
	os.Symlink(real, link)
	if err := try(link); err == nil {
		t.Error("a symbolic link was followed")
	}
	if err := try(dir); err == nil {
		t.Error("a directory was accepted")
	}
	if err := try(""); err == nil {
		t.Error("an empty path was accepted")
	}
	junk := filepath.Join(dir, "junk.pem")
	os.WriteFile(junk, []byte("-----BEGIN RSA PRIVATE KEY-----\nSECRETCONTENT\n-----END RSA PRIVATE KEY-----\n"), 0o600)
	if err := try(junk); err == nil || strings.Contains(err.Error(), "SECRETCONTENT") {
		t.Errorf("junk key: %v", err)
	}
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, []byte("SECRETCONTENT"), 0o600)
	if err := try(plain); err == nil || strings.Contains(err.Error(), "SECRETCONTENT") {
		t.Errorf("non-PEM file: %v", err)
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	if err := try(writeKey(t, small, false, 0o600)); err == nil || !strings.Contains(err.Error(), "2048") {
		t.Errorf("short key: %v", err)
	}
}

func TestConfigChecks(t *testing.T) {
	k := writeKey(t, key(t), false, 0o600)
	for name, c := range map[string]Config{
		"no app id":       {InstallationID: "42", KeyFile: k},
		"bad app id":      {AppID: "a/b", InstallationID: "42", KeyFile: k},
		"no installation": {AppID: "1", KeyFile: k},
		"path in id":      {AppID: "1", InstallationID: "42/../1", KeyFile: k},
		"plain http":      {AppID: "1", InstallationID: "42", KeyFile: k, APIURL: "http://api.example.com"},
		"user in url":     {AppID: "1", InstallationID: "42", KeyFile: k, APIURL: "https://u@api.github.com"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(Config{AppID: "Iv23liABCDEF", InstallationID: "42", KeyFile: k}); err != nil {
		t.Errorf("a client ID was refused: %v", err)
	}
}
