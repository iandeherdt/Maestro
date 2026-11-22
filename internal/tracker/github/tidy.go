package github

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/iandeherdt/Maestro/internal/tracker"
)

// A board belongs to one repository. A card whose issue is in another
// repository got there by GitHub's own automation: an "Auto-add to
// project" workflow, often one copied along with a board, that watches
// that other repository. The service never reads such a card, but the
// owner sees every new issue land on several boards at once.

const workflowsQuery = `
query($owner: String!, $number: Int!) {
  repositoryOwner(login: $owner) {
    ... on ProjectV2Owner {
      projectV2(number: $number) {
        workflows(first: 30) { nodes { id name enabled } }
      }
    }
  }
}`

const deleteWorkflowMutation = `
mutation($workflow: ID!) {
  deleteProjectV2Workflow(input: {workflowId: $workflow}) { deletedWorkflowId }
}`

const deleteCardMutation = `
mutation($project: ID!, $item: ID!) {
  deleteProjectV2Item(input: {projectId: $project, itemId: $item}) { deletedItemId }
}`

// Tidied is what Tidy changed on the board.
type Tidied struct {
	// Workflows are the names of the automations that were deleted.
	Workflows []string
	// Cards is the number of cards taken off the board, and Repos the
	// repositories their issues are in. The issues themselves stay.
	Cards int
	Repos []string
}

// Tidy keeps the board to the configured repository. When the board holds
// cards of issues from other repositories, it deletes the board's enabled
// "Auto-add to project" workflows, which put them there, and then takes
// those cards off the board. A board without such cards is left alone,
// including an Auto-add workflow that only watches this repository.
func (a *Adapter) Tidy(ctx context.Context) (Tidied, error) {
	var out Tidied
	if a.board == nil {
		return out, nil
	}
	m, err := a.meta(ctx, false)
	if err != nil {
		return out, err
	}
	foreign, repos, err := a.foreignCards(ctx)
	if err != nil || len(foreign) == 0 {
		return out, err
	}
	// The automation goes first, so that no new card arrives while the
	// old ones are taken off.
	var res struct {
		RepositoryOwner *struct {
			ProjectV2 *struct {
				Workflows struct {
					Nodes []struct {
						ID      string `json:"id"`
						Name    string `json:"name"`
						Enabled bool   `json:"enabled"`
					} `json:"nodes"`
				} `json:"workflows"`
			} `json:"projectV2"`
		} `json:"repositoryOwner"`
	}
	if err := a.c.graphql(ctx, workflowsQuery, map[string]any{"owner": a.board.owner, "number": a.board.number}, &res, false); err != nil {
		return out, err
	}
	if res.RepositoryOwner != nil && res.RepositoryOwner.ProjectV2 != nil {
		for _, w := range res.RepositoryOwner.ProjectV2.Workflows.Nodes {
			if !w.Enabled || w.ID == "" || !strings.HasPrefix(norm(w.Name), "auto-add") {
				continue
			}
			if err := a.c.graphql(ctx, deleteWorkflowMutation, map[string]any{"workflow": w.ID}, nil, true); err != nil {
				return out, explainTidy(err, "its automation "+w.Name)
			}
			out.Workflows = append(out.Workflows, w.Name)
		}
	}
	for _, item := range foreign {
		if err := a.c.graphql(ctx, deleteCardMutation, map[string]any{"project": m.id, "item": item}, nil, true); err != nil {
			return out, explainTidy(err, "a card of another repository")
		}
		out.Cards++
	}
	out.Repos = repos
	return out, nil
}

// foreignCards lists the board's cards whose issue is in another
// repository, and those repositories.
func (a *Adapter) foreignCards(ctx context.Context) ([]string, []string, error) {
	var items []string
	seen := map[string]bool{}
	var repos []string
	after := any(nil)
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, nil, terr(tracker.Pagination, "the board has more than %d pages of cards", maxPages)
		}
		var res struct {
			RepositoryOwner *struct {
				ProjectV2 *struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []*gqlCard `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"repositoryOwner"`
		}
		err := a.c.graphql(ctx, cardsQuery, a.vars(map[string]any{"owner": a.board.owner, "number": a.board.number, "after": after}), &res, false)
		if err != nil {
			return nil, nil, err
		}
		if res.RepositoryOwner == nil || res.RepositoryOwner.ProjectV2 == nil {
			return nil, nil, terr(tracker.Status, "%s was not found, or the service's App may not read it", a.boardName())
		}
		p := res.RepositoryOwner.ProjectV2.Items
		for _, c := range p.Nodes {
			if c == nil || c.ID == "" || c.Content == nil || c.Content.Typename != "Issue" || a.mine(c.Content.gqlIssue) {
				continue
			}
			items = append(items, c.ID)
			if r := c.Content.Repository.NameWithOwner; !seen[norm(r)] {
				seen[norm(r)] = true
				repos = append(repos, r)
			}
		}
		if !p.PageInfo.HasNextPage {
			break
		}
		after = p.PageInfo.EndCursor
	}
	sort.Strings(repos)
	return items, repos, nil
}

func explainTidy(err error, what string) error {
	var te *tracker.Error
	if errors.As(err, &te) && te.Category == tracker.WriteRejected {
		return terr(tracker.WriteRejected, "%s could not be removed from the board: %s. The App needs the organization permission Projects, read and write", what, te.Msg)
	}
	return err
}
