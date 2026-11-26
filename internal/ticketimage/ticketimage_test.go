package ticketimage

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestRepaintKeepsOnlyPixelsAndShrinksLargePictures(t *testing.T) {
	// A PNG with something after its end, and a JPEG: both come out as
	// plain PNGs.
	withTail := append(pngOf(t, 30, 20), []byte("<script>trailing</script>")...)
	for name, raw := range map[string][]byte{"png": withTail, "jpeg": jpegOf(t, 30, 20)} {
		out, err := Repaint(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, err := png.DecodeConfig(bytes.NewReader(out))
		if err != nil || c.Width != 30 || c.Height != 20 || bytes.Contains(out, []byte("trailing")) {
			t.Fatalf("%s: %+v %v", name, c, err)
		}
	}
	out, err := Repaint(pngOf(t, MaxSide*2, MaxSide/2))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := png.DecodeConfig(bytes.NewReader(out)); c.Width != MaxSide || c.Height != MaxSide/4 {
		t.Fatalf("shrunk to %dx%d", c.Width, c.Height)
	}
	if _, err := Repaint([]byte("%PDF-1.7 not a picture")); err == nil {
		t.Fatal("a PDF was taken for a picture")
	}
}

func TestLinksInTextAndHTML(t *testing.T) {
	md := "See ![shot](https://uploads.linear.app/a/b/c) and [log](https://uploads.linear.app/d/e).\nAlso https://example.com/x."
	got := Links(md)
	want := []string{"https://uploads.linear.app/a/b/c", "https://uploads.linear.app/d/e", "https://example.com/x"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("links %q", got)
	}
	html := `<p><a href="x"><img src="https://private-user-images.githubusercontent.com/1/a.png?jwt=e&amp;x=1" alt="s" style="max-width: 100%;"></a>` +
		`<img alt='y' src='https://camo.githubusercontent.com/z'></p>`
	got = HTMLImages(html)
	if len(got) != 2 || got[0] != "https://private-user-images.githubusercontent.com/1/a.png?jwt=e&x=1" || got[1] != "https://camo.githubusercontent.com/z" {
		t.Fatalf("images %q", got)
	}
}

// board serves pictures over https and records what it was sent.
type board struct {
	mu   sync.Mutex
	auth map[string]string // path -> Authorization header
	srv  *httptest.Server
}

func newBoard(t *testing.T, h func(b *board, w http.ResponseWriter, r *http.Request)) *board {
	b := &board{auth: map[string]string{}}
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.auth[r.URL.Path] = r.Header.Get("Authorization")
		b.mu.Unlock()
		h(b, w, r)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func TestFetchSendsTheKeyOnlyToTheBoardAndNeverAfterARedirect(t *testing.T) {
	pic := pngOf(t, 8, 8)
	storage := newBoard(t, func(_ *board, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pic)
	})
	home := newBoard(t, func(_ *board, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moved":
			http.Redirect(w, r, storage.srv.URL+"/signed", http.StatusFound)
		case "/doc":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF"))
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "image/png")
			w.Write(pic)
		}
	})
	hu, _ := url.Parse(home.srv.URL)
	f := Fetcher{
		HTTP:    home.srv.Client(),
		Allowed: func(u *url.URL) bool { return u.Host == hu.Host },
		Auth:    func(context.Context, *url.URL) (string, error) { return "lin_api_secret", nil },
	}
	// The storage server shares the test CA, so the board's client can
	// reach it.
	refs := []Ref{
		{URL: home.srv.URL + "/a.png", Where: "description"},
		{URL: home.srv.URL + "/a.png", Where: "description"}, // the same link twice
		{URL: home.srv.URL + "/moved", Where: "comment", Author: "ian"},
		{URL: storage.srv.URL + "/elsewhere", Where: "description"}, // not the board's host
		{URL: "http://" + hu.Host + "/plain", Where: "description"}, // not https
		{URL: home.srv.URL + "/doc", Where: "description"},          // not a picture
		{URL: home.srv.URL + "/gone", Where: "description"},
	}
	res := f.Fetch(context.Background(), refs)
	if len(res.Images) != 2 || res.Images[1].Where != "comment" || res.Images[1].Author != "ian" {
		t.Fatalf("images %+v", res.Images)
	}
	if res.Note != "3 pictures on the ticket could not be downloaded or read." {
		t.Fatalf("note %q", res.Note)
	}
	if home.auth["/a.png"] != "lin_api_secret" || home.auth["/moved"] != "lin_api_secret" {
		t.Fatalf("the board did not get the key: %v", home.auth)
	}
	if got, ok := storage.auth["/signed"]; !ok || got != "" {
		t.Fatalf("after the redirect the storage got %q (asked: %v)", got, ok)
	}
	if _, ok := storage.auth["/elsewhere"]; ok {
		t.Fatal("a link outside the board's hosts was downloaded")
	}
}

func TestFetchGivesAtMostMaxImages(t *testing.T) {
	pic := pngOf(t, 4, 4)
	home := newBoard(t, func(_ *board, w http.ResponseWriter, _ *http.Request) { w.Write(pic) })
	f := Fetcher{HTTP: home.srv.Client(), Allowed: func(*url.URL) bool { return true }}
	var refs []Ref
	for i := 0; i < MaxImages+3; i++ {
		refs = append(refs, Ref{URL: home.srv.URL + "/" + strings.Repeat("p", i+1)})
	}
	res := f.Fetch(context.Background(), refs)
	if len(res.Images) != MaxImages || res.Note != "3 more pictures were left out: at most 10 are given." {
		t.Fatalf("%d images, note %q", len(res.Images), res.Note)
	}
}
