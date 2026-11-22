package publish

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// DesignOutside is the rejection of a design that changes files outside
// its folder.
const DesignOutside = "design_outside_folder"

// PublishDesign is the publish gate for a design (SPEC 27). It applies the
// same import checks, diff policy and secret scan as for a ticket, and on
// top of that a design may only add or change files in its own folder.
// The pull request never closes the idea: merging it approves the design.
func (p *Publisher) PublishDesign(ctx context.Context, is issue.Issue, ws workspace.Workspace, folder string,
	shots []worker.Screenshot, note string) (*plan.Design, *worker.Rejection, error) {
	cfg := p.Config()
	owner, repo, err := target(cfg, ws)
	if err != nil {
		return nil, nil, err
	}
	folder = strings.Trim(folder, "/")
	if folder == "" || strings.Contains(folder, "..") {
		return nil, nil, fmt.Errorf("the design folder %q cannot be used", folder)
	}
	ad, rej, err := p.admit(ctx, cfg, ws)
	if err != nil || rej != nil {
		return nil, rej, err
	}
	var outside []string
	for _, c := range ad.diff.Changes {
		for _, path := range []string{c.Path, c.OldPath} {
			if path != "" && !strings.HasPrefix(path, folder+"/") {
				outside = append(outside, path)
			}
		}
	}
	if len(outside) > 0 {
		r := &worker.Rejection{Category: DesignOutside}
		for i, path := range outside {
			if i == 20 {
				r.Feedback = append(r.Feedback, issue.Feedback{Source: "policy", Body: fmt.Sprintf("... and %d more.", len(outside)-i)})
				break
			}
			r.Feedback = append(r.Feedback, issue.Feedback{Source: "policy",
				Body: "A design may only change files below " + folder + "/. This file is outside it: " + path})
		}
		return nil, r, nil
	}

	api, gitURL, err := p.endpoints(owner, repo)
	if err != nil {
		return nil, nil, err
	}
	tok, err := p.Token(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("repository token: %w", err)
	}
	if err := p.Mirror.Push(ctx, gitURL, ws.Key, ws.Branch, tok); err != nil {
		return nil, nil, fmt.Errorf("push: %w", err)
	}

	var flags []flagLine
	for _, f := range ad.res.Flags {
		flags = append(flags, flagLine{f.Rule, f.Path, f.Detail})
	}
	proof := Proof{Worker: cfg.Worker.Name, Commits: len(ad.imp.Commits), Files: len(ad.diff.Changes), ScreenshotNote: note}
	if len(shots) > 0 {
		if proof.Screenshots, err = p.store(ctx, api, owner, repo, ws.Key, ad.imp.Head, shots); err != nil {
			proof.ScreenshotNote = "The pictures were taken but could not be stored in the repository."
		}
	}
	idea := ""
	if n, err := strconv.Atoi(strings.TrimPrefix(is.Identifier, "GH-")); err == nil && n > 0 && strings.HasPrefix(is.Identifier, "GH-") {
		trackerRepo, _ := cfg.Tracker.Provider["repo"].(string)
		switch {
		case strings.EqualFold(trackerRepo, cfg.Workspace.Repo):
			idea = "#" + strconv.Itoa(n)
		case repoRE.MatchString(trackerRepo):
			idea = trackerRepo + "#" + strconv.Itoa(n)
		}
	}
	text := designBody(idea, folder, flags, proof)
	// Not a draft: there are no checks to wait for, the owner is next.
	number, err := api.upsert(ctx, owner, repo, ws.Branch, cfg.Workspace.BaseBranch,
		title("Design for "+is.Identifier, is.Title), text, false)
	if err != nil {
		return nil, nil, fmt.Errorf("pull request: %w", err)
	}
	return &plan.Design{PRNumber: number, Branch: ws.Branch, HeadSHA: ad.imp.Head, Folder: folder, Pictures: len(proof.Screenshots)}, nil, nil
}

// designBody is the description of a design pull request.
func designBody(idea, folder string, flags []flagLine, p Proof) string {
	var b strings.Builder
	if idea == "" {
		idea = "the idea"
	}
	fmt.Fprintf(&b, "A design for the plan of %s, made by an agent. It only adds files below %s and changes nothing else.\n\n", idea, code(folder+"/"))
	b.WriteString("- **To approve it**, merge this pull request and move the idea's card to Planned. The tasks of the plan are then created and told to follow this design.\n")
	b.WriteString("- **To change it**, comment here, on the lines of the files or in a review. The design is made again with your comments.\n")
	b.WriteString("- **To go on without it**, close this pull request and move the idea's card to Planned.\n")
	if len(p.Screenshots) > 0 || p.ScreenshotNote != "" {
		b.WriteString("\n## Pictures\n\n")
		if len(p.Screenshots) > 0 {
			b.WriteString("Taken by the service from the commits in this pull request, without network. " +
				"Open the files in a browser to see the rest, such as hover states and other screen sizes.\n")
		}
		for i, s := range p.Screenshots {
			fmt.Fprintf(&b, "\n%s\n\n![Picture %d](%s)\n", code(s.caption), i+1, s.url)
		}
		if p.ScreenshotNote != "" {
			if len(p.Screenshots) > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.ScreenshotNote + "\n")
		}
	}
	b.WriteString("\n## Review flags\n\n")
	if len(flags) == 0 {
		b.WriteString("None.\n")
	}
	for _, f := range flags {
		b.WriteString("- **" + f.rule + "** " + f.detail)
		if f.path != "" {
			b.WriteString(": " + code(f.path))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Checked by the service\n\n")
	fmt.Fprintf(&b, "- %d commit(s), %d file(s) changed, all below the design folder. Diff policy and secret scan passed.\n", p.Commits, p.Files)
	if p.Worker != "" {
		b.WriteString("- Worker: " + code(p.Worker) + "\n")
	}
	b.WriteString("\n---\n" + footer + "\n")
	return b.String()
}

// DesignState reports where the design pull request of an idea stands.
func (p *Publisher) DesignState(ctx context.Context, is issue.Issue) (plan.DesignState, error) {
	st, err := p.Review(ctx, plan.DesignIssue(is))
	if err != nil {
		return plan.DesignOpen, err
	}
	switch st.State {
	case orchestrator.ReviewMerged:
		return plan.DesignMerged, nil
	case orchestrator.ReviewClosed, orchestrator.ReviewNone:
		// A design whose pull request is gone is not used.
		return plan.DesignClosed, nil
	}
	return plan.DesignOpen, nil
}
