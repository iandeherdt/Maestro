package pins

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeServices(t *testing.T) *Checker {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/npm/@anthropic-ai%2Fclaude-code/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"2.1.345"}`)
	})
	mux.HandleFunc("/npm/eslint/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"9.39.1"}`)
	})
	mux.HandleFunc("/node.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"version":"v25.2.0","lts":false},{"version":"v24.11.0","lts":"Krypton"},{"version":"v22.21.0","lts":"Jod"}]`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:library/node:pull" {
			http.Error(w, "scope", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"token":"t0k"}`)
	})
	mux.HandleFunc("/v2/library/node/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:library/node:pull"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/library/node/tags/list?n=1000&last=22>; rel="next"`)
			fmt.Fprint(w, `{"tags":["22-bookworm-slim","24-bookworm-slim"]}`)
			return
		}
		fmt.Fprint(w, `{"tags":["25-bookworm-slim","24-trixie-slim"]}`)
	})
	mux.HandleFunc("/blender/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<a href="Blender4.5/">x</a><a href="Blender5.0/">x</a><a href="Blender5.1/">x</a><a href="Blender5.2/">x</a>`)
	})
	mux.HandleFunc("/blender/Blender5.2/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<a href="blender-5.2.0-release-notes.txt">`)
	})
	mux.HandleFunc("/blender/Blender5.1/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<a href="blender-5.1.0-release-notes.txt">`)
	})
	mux.HandleFunc("/blender/Blender5.0/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<a href="blender-5.0.1-linux-x64.tar.xz">x</a><a href="blender-5.0.2-linux-x64.tar.xz">x</a>`)
	})
	mux.HandleFunc("/hub/v2/repositories/unityci/editor/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "-base-4.0.0" {
			http.Error(w, "filter", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("page") == "" {
			fmt.Fprintf(w, `{"next":"%s/hub/v2/repositories/unityci/editor/tags?name=-base-4.0.0&page=2","results":[{"name":"ubuntu-6000.6.9f1-base-4.0.0"},{"name":"windows-6000.7.0f1-base-4.0.0"}]}`, srv.URL)
			return
		}
		fmt.Fprintf(w, `{"next":"%s/hub/v2/repositories/unityci/editor/tags?name=-base-4.0.0&page=3","results":[{"name":"ubuntu-6000.3.31f1-base-4.0.0"},{"name":"ubuntu-6000.3.29f1-base-4.0.0"}]}`, srv.URL)
	})
	mux.HandleFunc("/hub/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Docker Hub asked past the current tag: %s", r.URL)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Checker{
		HTTP:      srv.Client(),
		NPM:       srv.URL + "/npm",
		NodeIndex: srv.URL + "/node.json",
		Blender:   srv.URL + "/blender/",
		Registry:  func(string) string { return srv.URL },
		DockerHub: srv.URL + "/hub",
		GitTags: func(context.Context, string) ([]string, error) {
			return []string{"v0.3.0", "v0.4.0", "v0.5.0", "nightly"}, nil
		},
	}
}

func TestCheck(t *testing.T) {
	c := fakeServices(t)
	results := c.Check(context.Background(), []Pin{
		{Kind: KindNPM, Name: "@anthropic-ai/claude-code", Version: "2.1.330"},
		{Kind: KindNPM, Name: "eslint", Version: "9"},
		{Kind: KindImage, Name: "registry.example/library/node", Version: "22-bookworm-slim"},
		{Kind: KindBlender, Name: "blender", Version: "5.0.1"},
		{Kind: KindGit, Name: "https://github.com/klad-app/klad", Version: "v0.4.0", Commit: strings.Repeat("a", 40)},
		{Kind: KindNPM, Name: "missing", Version: "1.0"},
		{Kind: KindImage, Name: "docker.io/unityci/editor", Version: "ubuntu-6000.3.29f1-base-4.0.0"},
	})
	want := []string{"2.1.345", "", "24-bookworm-slim", "5.0.2", "v0.5.0", "", "ubuntu-6000.6.9f1-base-4.0.0"}
	for i, r := range results {
		if r.Latest != want[i] {
			t.Errorf("%s: latest %q, want %q (err %v)", r.Pin.Name, r.Latest, want[i], r.Err)
		}
	}
	if results[5].Err == nil {
		t.Error("a lookup that fails keeps its error")
	}
	if results[6].Line != "ubuntu-6000.3.31f1-base-4.0.0" {
		t.Errorf("unity line: %q", results[6].Line)
	}
	if Outdated(results) != 5 || Failed(results) != 1 {
		t.Errorf("outdated %d, failed %d", Outdated(results), Failed(results))
	}
	text := Report(results, "https://github.com/o/r", "abc")
	for _, s := range []string{"| npm @anthropic-ai/claude-code | `2.1.330` | `2.1.345` |", "Node.js LTS lines only", "## Not checked", "commit check"} {
		if !strings.Contains(text, s) {
			t.Errorf("report lacks %q:\n%s", s, text)
		}
	}
}
