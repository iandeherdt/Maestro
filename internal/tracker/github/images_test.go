package github

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestTicketImagesComeFromTheRenderedBodyWithoutTheToken(t *testing.T) {
	h := newHub(t)
	fi := h.add(1, "Ian", "Ian", "status:todo")
	pic := tinyPNG(t)
	var mu sync.Mutex
	asked := map[string]string{}
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		w.Write(pic)
	}))
	t.Cleanup(storage.Close)

	// The markdown has the github.com link; the HTML the signed one.
	fi.body = "Broken: ![shot](https://github.com/user-attachments/assets/abc)"
	fi.bodyHTML = `<p>Broken: <img src="` + storage.URL + `/desc.png?jwt=x&amp;y=1" alt="shot"></p>`
	at := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
	h.comment(1, "ian", "like this", at)
	h.comment(1, "stranger", "mine", at.Add(time.Minute))
	fi.commentHTML = map[int64]string{
		fi.comments[0].ID: `<img src="` + storage.URL + `/owner.png">`,
		fi.comments[1].ID: `<img src="` + storage.URL + `/stranger.png">`,
	}

	a := adapter(t, h)
	su, _ := url.Parse(storage.URL)
	a.imageHost = func(u *url.URL) bool { return u.Host == su.Host }
	a.imageHTTP = storage.Client()
	res, err := a.TicketImages(ctx, issue.Issue{ID: "I_node1", Identifier: "GH-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 || res.Note != "" {
		t.Fatalf("%d images, note %q", len(res.Images), res.Note)
	}
	if res.Images[0].Where != "description" || res.Images[1].Where != "comment" || res.Images[1].Author != "ian" || !res.Images[1].At.Equal(at) {
		t.Fatalf("images %+v", res.Images)
	}
	if _, ok := asked["/stranger.png"]; ok {
		t.Fatal("a picture from someone not allowlisted was downloaded")
	}
	for p, auth := range asked {
		if auth != "" {
			t.Fatalf("%s got credentials %q", p, auth)
		}
	}
}

func TestGitHubImageHosts(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://github.com/user-attachments/assets/abc":                   true,
		"https://private-user-images.githubusercontent.com/1/2.png?jwt=x":  true,
		"https://user-images.githubusercontent.com/1/2.png":                true,
		"https://camo.githubusercontent.com/abc":                           true,
		"https://github.com/ian/app/raw/main/secret.png":                   false,
		"https://api.github.com/repos/ian/app":                             false,
		"https://evil.example/user-attachments/assets/abc":                 false,
		"https://private-user-images.githubusercontent.com.evil.example/a": false,
	} {
		u, _ := url.Parse(raw)
		if githubImageHost(u) != want {
			t.Errorf("%s: want %v", raw, want)
		}
	}
}
