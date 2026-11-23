package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeLinearViewer answers the viewer query for the key "lin_api_good".
func fakeLinearViewer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "lin_api_good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"viewer": map[string]any{"id": "u-bot", "email": "bot@example.com", "name": "Bot"},
		}})
	}))
	t.Cleanup(srv.Close)
	old := linearEndpoint
	linearEndpoint = srv.URL
	t.Cleanup(func() { linearEndpoint = old })
	return srv
}

func TestLinearKeyIsCheckedStoredAndRemoved(t *testing.T) {
	fakeLinearViewer(t)
	dir := t.TempDir()
	run := func(stdin string, words ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runLinear(append([]string{"--dir", dir}, words...), strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}

	if code, out, _ := run(""); code != 0 || !strings.Contains(out, "No Linear API key is stored") {
		t.Fatalf("status without a key: %d %q", code, out)
	}
	if code, _, errs := run("lin_api_bad\n", "key"); code == 0 || !strings.Contains(errs, "Nothing was stored") {
		t.Fatalf("a refused key: %d %q", code, errs)
	}
	if _, err := os.Stat(linearKeyFile(dir)); !os.IsNotExist(err) {
		t.Fatal("a refused key was stored")
	}
	if code, _, errs := run("two words\n", "key"); code != 1 || !strings.Contains(errs, "not an API key") {
		t.Fatalf("a key with a space: %d %q", code, errs)
	}
	code, out, errs := run("  lin_api_good \n", "key")
	if code != 0 || !strings.Contains(out, "bot@example.com") {
		t.Fatalf("a good key: %d %q %q", code, out, errs)
	}
	fi, err := os.Stat(linearKeyFile(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v %v", fi, err)
	}
	if key, _ := readLinearKey(dir); key != "lin_api_good" {
		t.Fatalf("stored %q", key)
	}
	if code, out, _ := run(""); code != 0 || !strings.Contains(out, "acts as bot@example.com") {
		t.Fatalf("status with a key: %d %q", code, out)
	}
	if code, _, _ := run("", "remove"); code != 0 {
		t.Fatal("remove failed")
	}
	if _, err := os.Stat(linearKeyFile(dir)); !os.IsNotExist(err) {
		t.Fatal("the key is still there")
	}
	if code, _, _ := run("", "bogus"); code != 2 {
		t.Fatal("an unknown word is not a usage error")
	}
}
