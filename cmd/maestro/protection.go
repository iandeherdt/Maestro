package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The service never pushes to the base branch and never merges: it has no
// code that could. GitHub would let it, though. Opening a pull request
// takes the permission to write code, and with that permission the App's
// key can also push to any branch and merge a pull request. So "nothing
// reaches the base branch without you" holds only as long as the service
// has no mistake in it and nobody else has its key, unless GitHub itself
// refuses: a rule on the base branch that requires a pull request and an
// approval. The service looks for that rule when it starts and says so
// when there is none.

// baseGuarded reports whether GitHub requires a pull request for the
// branch. A branch ruleset says so by name. The older branch protection
// only shows an App that the branch is protected, not how, and counts.
func baseGuarded(ctx context.Context, apiURL, repo, branch, token string) (bool, error) {
	get := func(path string, into any) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/repos/"+repo+path, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := hc.Do(req)
		if err != nil {
			return 0, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return res.StatusCode, nil
		}
		return res.StatusCode, json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(into)
	}
	var rules []struct {
		Type string `json:"type"`
	}
	status, err := get("/rules/branches/"+branch, &rules)
	if err != nil {
		return false, err
	}
	// A GitHub without rulesets answers 404 here; the protection below is
	// then all there is.
	if status == http.StatusOK {
		for _, r := range rules {
			if r.Type == "pull_request" {
				return true, nil
			}
		}
	}
	var b struct {
		Protected bool `json:"protected"`
	}
	status, err = get("/branches/"+branch, &b)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("GitHub answered %d for the branch", status)
	}
	return b.Protected, nil
}

// unguardedNote is said when GitHub would accept a direct push.
func unguardedNote(web, repo, branch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nNote: GitHub does not require a pull request for %s.\n", branch)
	fmt.Fprintln(&b, "Maestro never pushes to that branch and never merges. But its key is allowed to, so a mistake in")
	fmt.Fprintln(&b, "Maestro or a copy of the key file could. To have GitHub refuse it, add a rule, once:")
	fmt.Fprintf(&b, "  1. Open %s/%s/settings/rules and press New ruleset, New branch ruleset.\n", web, repo)
	fmt.Fprintf(&b, "  2. Set Enforcement status to Active and add the target branch %s.\n", branch)
	fmt.Fprintln(&b, "  3. Tick \"Require a pull request before merging\" with 1 required approval. Leave the bypass list empty.")
	fmt.Fprintln(&b, "Maestro works the same with the rule: it only opens pull requests, and you approve and merge them.")
	fmt.Fprintln(&b, "On a private repository GitHub offers rules on its paid plans only.")
	return b.String()
}
