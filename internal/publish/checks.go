package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

// Check reads the pull request and the checks of the pushed commit (SPEC
// 15.6). With publish.required_checks set, exactly those must pass. With
// an empty list, every check reported for the commit must pass and there
// must be at least one, so that "no checks yet" is never read as green.
func (p *Publisher) Check(ctx context.Context, rec orchestrator.PublishRecord) (orchestrator.CheckStatus, error) {
	var none orchestrator.CheckStatus
	h, err := p.host()
	if err != nil {
		return none, err
	}
	if rec.PRNumber <= 0 || !shaRE.MatchString(rec.HeadSHA) {
		return none, errors.New("the publish record is incomplete")
	}
	pr, err := h.change(ctx, rec.PRNumber)
	if err != nil {
		return none, err
	}
	if pr.State != changeOpen {
		return orchestrator.CheckStatus{State: orchestrator.ChecksClosed}, nil
	}
	if pr.HeadSHA != rec.HeadSHA {
		return orchestrator.CheckStatus{State: orchestrator.ChecksForeignPush}, nil
	}
	if p.Config().Publish.NoChecks {
		// The operator said this repository has no checks to wait for.
		return orchestrator.CheckStatus{State: orchestrator.ChecksPassed}, nil
	}
	results, complete, err := h.checks(ctx, rec.HeadSHA)
	if err != nil {
		return none, err
	}

	// A copy: the list is sorted below and the configuration is shared.
	names := append([]string(nil), p.Config().Publish.RequiredChecks...)
	if len(names) == 0 {
		for n := range results {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var failedNames []string
	waiting := 0
	for _, n := range names {
		switch c, ok := results[n]; {
		case !ok || c.res == resPending:
			waiting++
		case c.res == resFailed:
			failedNames = append(failedNames, n)
		}
	}
	switch {
	case len(failedNames) > 0:
		st := orchestrator.CheckStatus{State: orchestrator.ChecksFailed,
			Detail: fmt.Sprintf("%d required check(s) failed", len(failedNames))}
		for _, n := range failedNames {
			body := "The check " + clean(n, 100) + " failed."
			if d := clean(strings.ReplaceAll(results[n].detail, "\n", " "), 1500); d != "" {
				body += " Its report: " + d
			}
			st.Feedback = append(st.Feedback, issue.Feedback{Source: "ci", Body: body})
		}
		return st, nil
	case waiting > 0 || len(names) == 0 || !complete:
		return orchestrator.CheckStatus{State: orchestrator.ChecksPending,
			Detail:       fmt.Sprintf("%d of %d check(s) not finished", waiting, len(names)),
			NoneReported: len(results) == 0}, nil
	}
	return orchestrator.CheckStatus{State: orchestrator.ChecksPassed}, nil
}

// MarkReady takes the pull request out of draft. A pull request that is
// already ready is left alone.
func (p *Publisher) MarkReady(ctx context.Context, rec orchestrator.PublishRecord) error {
	h, err := p.host()
	if err != nil {
		return err
	}
	pr, err := h.change(ctx, rec.PRNumber)
	if err != nil {
		return err
	}
	if pr.HeadSHA != rec.HeadSHA {
		return errors.New("the pull request no longer points at the pushed commit")
	}
	if !pr.Draft {
		return nil
	}
	return h.markReady(ctx, pr)
}

var _ orchestrator.Checker = (*Publisher)(nil)
