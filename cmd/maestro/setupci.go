package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/ghapp"
	"github.com/iandeherdt/Maestro/internal/setup"
)

// findCI lists the GitHub Actions workflow files of the repository, with
// the App's read-only token. It answers for GitHub only; elsewhere, or when
// it cannot look, known is false and setup asks without a hint.
func findCI(ctx context.Context, st *setup.State) (found []string, known bool) {
	if st.OffGitHub() || st.AppID == "" || st.InstallationID == "" || st.AppKeyFile == "" {
		return nil, false
	}
	owner, name, ok := strings.Cut(st.Repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, false
	}
	app, err := ghapp.New(ghapp.Config{AppID: st.AppID, InstallationID: st.InstallationID,
		KeyFile: st.AppKeyFile, APIURL: gitHub.api})
	if err != nil {
		return nil, false
	}
	src, err := app.Source(ghapp.Scope{Repositories: []string{name}, Permissions: map[string]string{"contents": "read"}})
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := src.Token(ctx)
	if err != nil {
		return nil, false
	}
	return workflowFiles(ctx, gitHub.api, st.Repo, token)
}

// workflowNameRE is a workflow file name that is safe to show.
var workflowNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}\.ya?ml$`)

// workflowFiles reads .github/workflows. A folder that is not there is a
// known "none".
func workflowFiles(ctx context.Context, apiURL, repo, token string) ([]string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/repos/"+repo+"/contents/.github/workflows", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(req)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound:
		return nil, true
	case http.StatusOK:
	default:
		return nil, false
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&entries); err != nil {
		return nil, false
	}
	var found []string
	for _, e := range entries {
		if e.Type == "file" && workflowNameRE.MatchString(e.Name) {
			found = append(found, e.Name)
		}
	}
	sort.Strings(found)
	if len(found) > 5 {
		found = append(found[:5], "…")
	}
	return found, true
}
