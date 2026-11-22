package pins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Result is what a lookup found for one pin. Latest is empty when the pin
// is current.
type Result struct {
	Pin    Pin
	Latest string
	// Line is the newest release of the pinned line (6000.3.x for
	// 6000.3.29f1), when it differs from Latest.
	Line string
	Note string
	Err  error
}

// Checker looks up the newest releases. The fields are the places it asks;
// tests point them at a fake server.
type Checker struct {
	HTTP      *http.Client
	NPM       string // https://registry.npmjs.org
	NodeIndex string // https://nodejs.org/dist/index.json
	Blender   string // https://download.blender.org/release/
	// Registry maps an image's registry to the URL of its API.
	Registry func(host string) string
	// DockerHub is the API of Docker Hub, which can filter tags by name;
	// the registry API of docker.io lists tens of thousands for some images.
	DockerHub string
	// GitTags lists the tags of a repository.
	GitTags func(ctx context.Context, url string) ([]string, error)
	// Log, when set, hears of each lookup as it starts.
	Log func(format string, args ...any)
}

// NewChecker returns a Checker that asks the real services.
func NewChecker() *Checker {
	return &Checker{
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		NPM:       "https://registry.npmjs.org",
		NodeIndex: "https://nodejs.org/dist/index.json",
		Blender:   "https://download.blender.org/release/",
		Registry: func(host string) string {
			if host == "docker.io" {
				return "https://registry-1.docker.io"
			}
			return "https://" + host
		},
		DockerHub: "https://hub.docker.com",
		GitTags:   lsRemoteTags,
	}
}

// Check looks up every pin. A failed lookup is kept in its result, so one
// unreachable service does not hide the others.
func (c *Checker) Check(ctx context.Context, pins []Pin) []Result {
	out := make([]Result, 0, len(pins))
	for _, p := range pins {
		if c.Log != nil {
			c.Log("checking %s %s %s", p.Kind, p.Name, p.Version)
		}
		out = append(out, c.check(ctx, p))
	}
	return out
}

func (c *Checker) check(ctx context.Context, p Pin) Result {
	r := Result{Pin: p}
	var (
		candidates []string
		keep       func([]int) bool
	)
	switch p.Kind {
	case KindImage:
		candidates, keep, r.Note, r.Err = c.image(ctx, p)
	case KindNPM:
		candidates, r.Err = c.npm(ctx, p)
	case KindGit:
		// Tags of the same shape only: v0.4.0 is followed by v0.5.0, and
		// 4.5.1-stable by 4.5.2-stable, never by 4.6-rc1.
		candidates, r.Err = c.GitTags(ctx, p.Name)
	case KindBlender:
		candidates, r.Err = c.blender(ctx, p)
	default:
		r.Err = fmt.Errorf("no lookup for kind %q", p.Kind)
	}
	if r.Err != nil {
		return r
	}
	r.Latest = newest(p.Version, candidates, keep)
	if line := sameLine(p.Version); line != nil && r.Latest != "" {
		both := line
		if keep != nil {
			both = func(n []int) bool { return keep(n) && line(n) }
		}
		if l := newest(p.Version, candidates, both); l != r.Latest {
			r.Line = l
		}
	}
	if r.Latest == "" {
		r.Note = ""
	} else if p.Kind == KindGit && p.Commit != "" {
		r.Note = "the commit check next to the tag needs the new commit too"
	}
	return r
}

func (c *Checker) get(ctx context.Context, u string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "maestro-pincheck")
	return c.HTTP.Do(req)
}

func (c *Checker) getBody(ctx context.Context, u string) ([]byte, error) {
	resp, err := c.get(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func (c *Checker) npm(ctx context.Context, p Pin) ([]string, error) {
	body, err := c.getBody(ctx, c.NPM+"/"+strings.Replace(p.Name, "/", "%2F", 1)+"/latest")
	if err != nil {
		return nil, err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Version == "" {
		return nil, fmt.Errorf("npm %s: no version in the answer", p.Name)
	}
	return []string{truncate(v.Version, p.Version)}, nil
}

func lsRemoteTags(ctx context.Context, url string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--tags", "--refs", url).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %s: %w", url, err)
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	return tags, nil
}

var (
	reBlenderDir  = regexp.MustCompile(`href="Blender(\d+\.\d+)/"`)
	reBlenderFile = regexp.MustCompile(`blender-(\d+\.\d+\.\d+)-linux-x64\.tar\.xz"`)
)

func (c *Checker) blender(ctx context.Context, p Pin) ([]string, error) {
	index, err := c.getBody(ctx, c.Blender)
	if err != nil {
		return nil, err
	}
	var series []string
	for _, m := range reBlenderDir.FindAllStringSubmatch(string(index), -1) {
		series = append(series, m[1])
	}
	// The pinned series and the newest one; a series without a Linux
	// build yet lists no candidate.
	cur := p.Version[:strings.LastIndex(p.Version, ".")]
	look := []string{cur}
	if top := newest(cur, series, nil); top != "" {
		look = append(look, top)
	}
	var files []string
	for _, dir := range look {
		page, err := c.getBody(ctx, c.Blender+"Blender"+dir+"/")
		if err != nil {
			return nil, err
		}
		for _, m := range reBlenderFile.FindAllStringSubmatch(string(page), -1) {
			files = append(files, m[1])
		}
	}
	return files, nil
}

func (c *Checker) image(ctx context.Context, p Pin) ([]string, func([]int) bool, string, error) {
	tags, err := c.tags(ctx, p.Name, p.Version)
	if err != nil || !strings.HasSuffix(p.Name, "/library/node") {
		return tags, nil, "", err
	}
	lts, err := c.nodeLTS(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	keep := func(nums []int) bool { return len(nums) > 0 && lts[nums[0]] }
	return tags, keep, "Node.js LTS lines only", nil
}

// sameLine keeps the candidates whose first two numbers are those of the
// pin, for a pin with a patch number; nil otherwise.
func sameLine(pin string) func([]int) bool {
	_, cur := shape(pin)
	if len(cur) < 3 {
		return nil
	}
	return func(nums []int) bool { return len(nums) >= 2 && nums[0] == cur[0] && nums[1] == cur[1] }
}

func (c *Checker) nodeLTS(ctx context.Context) (map[int]bool, error) {
	body, err := c.getBody(ctx, c.NodeIndex)
	if err != nil {
		return nil, err
	}
	var rel []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("node index: %w", err)
	}
	lts := map[int]bool{}
	for _, r := range rel {
		if s, ok := r.LTS.(string); ok && s != "" {
			_, nums := shape(r.Version)
			if len(nums) > 0 {
				lts[nums[0]] = true
			}
		}
	}
	return lts, nil
}

// tags lists an image's tags through the registry API, with the anonymous
// token the registry asks for.
func (c *Checker) tags(ctx context.Context, image, current string) ([]string, error) {
	host, repo, _ := strings.Cut(image, "/")
	if host == "docker.io" && c.DockerHub != "" {
		return c.hubTags(ctx, repo, current)
	}
	next := c.Registry(host) + "/v2/" + repo + "/tags/list?n=1000"
	var all []string
	token := ""
	for page := 0; next != "" && page < 200; page++ {
		h := http.Header{}
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.get(ctx, next, h)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && token == "" {
			challenge := resp.Header.Get("Www-Authenticate")
			resp.Body.Close()
			if token, err = c.token(ctx, challenge, repo); err != nil {
				return nil, fmt.Errorf("%s: %w", image, err)
			}
			page--
			continue
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("tags of %s: %s", image, resp.Status)
			}
			return json.NewDecoder(resp.Body).Decode(&body)
		}()
		if err != nil {
			return nil, err
		}
		all = append(all, body.Tags...)
		next = nextLink(resp.Header.Get("Link"), next)
	}
	return all, nil
}

var reChallenge = regexp.MustCompile(`(\w+)="([^"]*)"`)

func (c *Checker) token(ctx context.Context, challenge, repo string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", errors.New("the registry asks for a login this check does not have")
	}
	params := map[string]string{}
	for _, m := range reChallenge.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	if params["realm"] == "" {
		return "", errors.New("the registry's login challenge names no realm")
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	body, err := c.getBody(ctx, params["realm"]+"?"+q.Encode())
	if err != nil {
		return "", err
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", err
	}
	if t.Token != "" {
		return t.Token, nil
	}
	return t.AccessToken, nil
}

// hubTags lists the tags of a Docker Hub repository that contain the text
// of the current tag around its release, newest push first, up to the
// current tag itself: a newer release is pushed after it.
func (c *Checker) hubTags(ctx context.Context, repo, current string) ([]string, error) {
	q := url.Values{}
	q.Set("page_size", "100")
	q.Set("ordering", "-last_updated")
	if f := hubFilter(current); f != "" {
		q.Set("name", f)
	}
	next := c.DockerHub + "/v2/repositories/" + repo + "/tags?" + q.Encode()
	var all []string
	for page := 0; next != "" && page < 50; page++ {
		body, err := c.getBody(ctx, next)
		if err != nil {
			return nil, err
		}
		var res struct {
			Next    string `json:"next"`
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("tags of %s: %w", repo, err)
		}
		for _, r := range res.Results {
			all = append(all, r.Name)
			if r.Name == current {
				return all, nil
			}
		}
		next = res.Next
	}
	return all, nil
}

// hubFilter is the text Docker Hub filters the tags by: what follows the
// release in the tag (-base-4.0.0 of ubuntu-6000.3.29f1-base-4.0.0), or
// else its longest text between numbers (-bookworm-slim).
func hubFilter(tag string) string {
	if v := tagVersion(Pin{Kind: KindImage, Version: tag}); v != "" {
		if _, after, ok := strings.Cut(tag, v); ok && len(after) > 1 {
			return after
		}
	}
	sk, _ := shape(tag)
	best := ""
	for _, part := range strings.Split(sk, "#") {
		if len(part) > len(best) {
			best = part
		}
	}
	if len(best) < 3 {
		return ""
	}
	return best
}

// nextLink reads the next page from a Link header, which may be relative.
func nextLink(link, current string) string {
	if link == "" || !strings.Contains(link, `rel="next"`) {
		return ""
	}
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end < start {
		return ""
	}
	base, err := url.Parse(current)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(link[start+1 : end])
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}
