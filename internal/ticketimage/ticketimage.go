// Package ticketimage downloads the pictures people put on a ticket, so
// that the agent can look at them (SPEC 13.5). The agent cannot fetch them
// itself: the board's attachment links need the service's credentials, and
// the sandbox has no network for them. So the service downloads them, with
// its own credentials and only from the board's own hosts, turns each into
// a plain PNG and puts it next to the repository.
package ticketimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// MaxImages is how many pictures of one ticket reach the agent.
	MaxImages = 10
	// MaxDownloadBytes caps one download.
	MaxDownloadBytes = 20 << 20
	// MaxSide is the longest side of a picture given to the agent. Larger
	// pictures are made smaller; the agent's model sees no more detail
	// than this anyway.
	MaxSide = 2000
	// maxPixels refuses a file that claims to be a huge picture before it
	// is unpacked.
	maxPixels = 64 << 20
	// MaxComments is how many of the newest comments are searched.
	MaxComments = 20
	maxRedirect = 5
	timeout     = 60 * time.Second
)

// Image is one picture on a ticket, as the agent gets it.
type Image struct {
	// Where says in the service's words where on the ticket it is:
	// "description" or "comment".
	Where string
	// Author wrote the comment it is in. Empty for the description.
	Author string
	// At is when the comment was written. Zero for the description.
	At time.Time
	// PNG was decoded and encoded again by the service, so it holds
	// pixels and nothing else.
	PNG []byte
}

// Ref is a link to a picture found in a ticket's text.
type Ref struct {
	URL    string
	Where  string
	Author string
	At     time.Time
}

// Result is what a board's pictures came to.
type Result struct {
	Images []Image
	// Note says in the service's words why some or all of them are
	// missing. Empty when every picture arrived.
	Note string
}

// Fetcher downloads pictures from a board's own hosts.
type Fetcher struct {
	HTTP *http.Client
	// Allowed tells whether a link may be downloaded at all. Only links on
	// the board's own hosts are, so that a ticket cannot make the service
	// fetch an address of its choosing.
	Allowed func(u *url.URL) bool
	// Auth returns the Authorization header for a link, or "" to send
	// none. It is only asked for links Allowed accepted, and it is never
	// sent again after a redirect to another host.
	Auth func(ctx context.Context, u *url.URL) (string, error)
}

// Fetch downloads every picture in refs, in order, up to MaxImages. It
// never fails: what went wrong is in the note.
func (f *Fetcher) Fetch(ctx context.Context, refs []Ref) Result {
	var out Result
	seen := map[string]bool{}
	failed, skipped := 0, 0
	for _, r := range refs {
		if seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		if len(out.Images) >= MaxImages {
			skipped++
			continue
		}
		raw, err := f.download(ctx, r.URL)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, errNotPicture) {
				continue // a link to a file that is not a picture
			}
			failed++
			continue
		}
		pic, err := Repaint(raw)
		if err != nil {
			if errors.Is(err, errNotPicture) {
				continue
			}
			failed++
			continue
		}
		out.Images = append(out.Images, Image{Where: r.Where, Author: r.Author, At: r.At, PNG: pic})
	}
	var notes []string
	if failed > 0 {
		notes = append(notes, fmt.Sprintf("%d %s on the ticket could not be downloaded or read.", failed, plural(failed)))
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d more %s left out: at most %d are given.", skipped, pluralWas(skipped), MaxImages))
	}
	out.Note = strings.Join(notes, " ")
	return out
}

func plural(n int) string {
	if n == 1 {
		return "picture"
	}
	return "pictures"
}

func pluralWas(n int) string {
	if n == 1 {
		return "picture was"
	}
	return "pictures were"
}

var errNotPicture = errors.New("not a picture")

func (f *Fetcher) download(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || f.Allowed == nil || !f.Allowed(u) {
		return nil, errors.New("link is not on the board's own hosts")
	}
	auth := ""
	if f.Auth != nil {
		if auth, err = f.Auth(ctx, u); err != nil {
			return nil, err
		}
	}
	hc := http.Client{Timeout: timeout}
	if f.HTTP != nil {
		hc = *f.HTTP
		if hc.Timeout == 0 {
			hc.Timeout = timeout
		}
	}
	origin := u.Host
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirect {
			return errors.New("too many redirects")
		}
		// The board's host sends the download on, usually to its storage.
		// It is trusted to do that, but the credentials stay with it.
		if req.URL.Scheme != "https" {
			return errors.New("redirect away from https")
		}
		if req.URL.Host != origin {
			req.Header.Del("Authorization")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "maestro-orchestrator")
	req.Header.Set("Accept", "image/*")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("download failed") // the error text holds the signed link
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download answered %d", res.StatusCode)
	}
	if ct := strings.ToLower(res.Header.Get("Content-Type")); ct != "" && !strings.HasPrefix(ct, "image/") &&
		!strings.HasPrefix(ct, "application/octet-stream") && !strings.HasPrefix(ct, "binary/octet-stream") {
		return nil, errNotPicture
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, errors.New("download cut off")
	}
	if len(body) > MaxDownloadBytes {
		return nil, errors.New("picture too large")
	}
	return body, nil
}

// Repaint decodes a PNG, JPEG or GIF and encodes it again as a PNG, no
// larger than MaxSide on its longest side. Text chunks, metadata, trailing
// data and anything else that is not a pixel does not survive that.
func Repaint(raw []byte) ([]byte, error) {
	c, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, errNotPicture
	}
	if c.Width < 1 || c.Height < 1 || c.Width*c.Height > maxPixels {
		return nil, errors.New("picture too large")
	}
	var img image.Image
	switch format {
	case "png":
		img, err = png.Decode(bytes.NewReader(raw))
	case "jpeg":
		img, err = jpeg.Decode(bytes.NewReader(raw))
	case "gif":
		img, err = gif.Decode(bytes.NewReader(raw)) // the first frame
	default:
		return nil, errNotPicture
	}
	if err != nil {
		return nil, errors.New("picture does not decode")
	}
	img = shrink(img, MaxSide)
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// shrink scales img down so that its longest side is at most side, by
// averaging the source pixels that fall into each target pixel.
func shrink(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	nw, nh := side, h*side/w
	if h > w {
		nw, nh = w*side/h, side
	}
	nw, nh = max(nw, 1), max(nh, 1)
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r += uint64(p[0])
					g += uint64(p[1])
					bl += uint64(p[2])
					a += uint64(p[3])
					n++
				}
			}
			d := dst.Pix[y*dst.Stride+x*4:]
			d[0], d[1], d[2], d[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}
