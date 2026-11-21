package publish

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// Screenshots are kept in the repository itself, on one branch that holds
// nothing else (SPEC 24). GitHub has no other place an App can put a
// picture where a pull request of a private repository can show it. Every
// publish adds one commit with its pictures below <ticket>/<commit>/, and
// the pull request points at that commit, so later pictures never change
// what an earlier pull request shows.

// shotLink is one picture in the pull request description.
type shotLink struct{ caption, url string }

// shotBranch is the branch the pictures are kept on.
func shotBranch(prefix string) string { return prefix + "screenshots" }

type gitObject struct {
	SHA string `json:"sha"`
}

// store uploads the pictures and returns where each can be seen.
func (p *Publisher) store(ctx context.Context, a *api, owner, repo, key, head string, shots []worker.Screenshot) ([]shotLink, error) {
	branch := shotBranch(p.Config().Publish.BranchPrefix)
	if !mirror.ValidBranch(branch) || !shaRE.MatchString(head) {
		return nil, errors.New("the branch for screenshots cannot be named")
	}
	prefix := "/repos/" + owner + "/" + repo
	dir := key + "/" + head[:12]
	type entry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var entries []entry
	for i, s := range shots {
		var blob gitObject
		in := map[string]string{"content": base64.StdEncoding.EncodeToString(s.PNG), "encoding": "base64"}
		if err := a.do(ctx, http.MethodPost, prefix+"/git/blobs", nil, in, &blob); err != nil {
			return nil, err
		}
		entries = append(entries, entry{dir + "/" + strconv.Itoa(i+1) + ".png", "100644", "blob", blob.SHA})
	}

	// Two machines can publish at the same moment. The branch only moves
	// forward, so the slower one finds that out and builds on the other.
	var commit gitObject
	for try := 0; ; try++ {
		var ref struct {
			Object gitObject `json:"object"`
		}
		err := a.do(ctx, http.MethodGet, prefix+"/git/ref/heads/"+branch, nil, nil, &ref)
		var ae *apiError
		exists := err == nil
		if err != nil && !(errors.As(err, &ae) && ae.Status == http.StatusNotFound) {
			return nil, err
		}
		tree := map[string]any{"tree": entries}
		parents := []string{}
		if exists {
			var tip struct {
				Tree gitObject `json:"tree"`
			}
			if err := a.do(ctx, http.MethodGet, prefix+"/git/commits/"+ref.Object.SHA, nil, nil, &tip); err != nil {
				return nil, err
			}
			tree["base_tree"], parents = tip.Tree.SHA, []string{ref.Object.SHA}
		}
		var made gitObject
		if err := a.do(ctx, http.MethodPost, prefix+"/git/trees", nil, tree, &made); err != nil {
			return nil, err
		}
		in := map[string]any{"message": "Screenshots for " + key, "tree": made.SHA, "parents": parents}
		if err := a.do(ctx, http.MethodPost, prefix+"/git/commits", nil, in, &commit); err != nil {
			return nil, err
		}
		if exists {
			err = a.do(ctx, http.MethodPatch, prefix+"/git/refs/heads/"+branch, nil, map[string]any{"sha": commit.SHA, "force": false}, nil)
		} else {
			err = a.do(ctx, http.MethodPost, prefix+"/git/refs", nil, map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA}, nil)
		}
		if err == nil {
			break
		}
		if !(errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity) || try >= 2 {
			return nil, err
		}
	}
	if !shaRE.MatchString(commit.SHA) {
		return nil, errors.New("the commit with the screenshots has no usable name")
	}

	web := strings.TrimSuffix(p.GitURL, "/")
	if web == "" {
		web = "https://github.com"
	}
	links := make([]shotLink, len(shots))
	for i, s := range shots {
		// A picture of a private repository is shown to whoever may read
		// the repository, through their own session with GitHub.
		links[i] = shotLink{s.Caption, fmt.Sprintf("%s/%s/%s/blob/%s/%s/%s/%d.png?raw=true", web, owner, repo,
			commit.SHA, url.PathEscape(key), head[:12], i+1)}
	}
	return links, nil
}

// storeFor is store with the repository taken from the settings.
func (p *Publisher) storeFor(ctx context.Context, key, head string, shots []worker.Screenshot) ([]shotLink, error) {
	m := repoRE.FindStringSubmatch(p.Config().Workspace.Repo)
	if m == nil {
		return nil, errors.New("workspace.repo must be owner/name")
	}
	a, _, err := p.endpoints(m[1], m[2])
	if err != nil {
		return nil, err
	}
	return p.store(ctx, a, m[1], m[2], key, head, shots)
}
