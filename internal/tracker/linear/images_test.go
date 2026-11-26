package linear

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/iandeherdt/Maestro/internal/issue"
)

func TestTicketImagesAreDownloadedWithTheKeyFromLinearsUploadHostOnly(t *testing.T) {
	f, a := setup(t)
	f.add(1, ian, "Todo", "run:linux")
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	var mu sync.Mutex
	asked := map[string]string{}
	uploads := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		if r.Header.Get("Authorization") != "lin_api_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(b.Bytes())
	}))
	t.Cleanup(uploads.Close)
	uu, _ := url.Parse(uploads.URL)
	a.imageHost = func(u *url.URL) bool { return u.Host == uu.Host }
	a.imageHTTP = uploads.Client()

	f.comments["id-1"] = []comment{
		{ID: "a", Body: "Like this: ![x](" + uploads.URL + "/org/owner.png)", CreatedAt: "2026-11-20T11:00:00Z", User: &ian},
		{ID: "b", Body: "![x](" + uploads.URL + "/org/stranger.png)", CreatedAt: "2026-11-20T11:00:00Z", User: &stranger},
	}
	is := issue.Issue{ID: "id-1", Identifier: "ENG-1",
		Description: "The menu: ![menu.png](" + uploads.URL + "/org/menu.png) and an outside one ![](https://example.com/x.png)"}
	res, err := a.TicketImages(context.Background(), is)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 || res.Note != "" || res.Images[0].Where != "description" || res.Images[1].Author != "ian@example.com" {
		t.Fatalf("%+v note %q", res.Images, res.Note)
	}
	if asked["/org/menu.png"] != "lin_api_test" || asked["/org/owner.png"] != "lin_api_test" {
		t.Fatalf("asked %v", asked)
	}
	for p := range asked {
		if strings.Contains(p, "stranger") {
			t.Fatal("a picture from someone not allowlisted was downloaded")
		}
	}
}
