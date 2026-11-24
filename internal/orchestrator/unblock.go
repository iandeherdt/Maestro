package orchestrator

import (
	"context"
	"errors"
	"strings"
)

// UnblockResult says what Unblock did.
type UnblockResult struct {
	// Identifier is the issue's identifier as the board has it.
	Identifier string
	// Found is false when no blocked or claimed issue of this worker has
	// the identifier.
	Found bool
	// Failures is the count of failure attempts that was cleared.
	Failures int64
	// MovedTo is the state the issue was moved to, or "" when it was not
	// in the blocked state.
	MovedTo string
	// OtherWorker is set when the issue is blocked but routed to another
	// worker, which is the one to unblock it.
	OtherWorker bool
	// Err is a failed tracker request. The budget is cleared all the same.
	Err error
}

// Unblock gives an issue a fresh failure budget (SPEC 8.4, `maestro
// unblock`). An issue in the blocked state is moved to Rework, which is an
// active state the service may write. It is not moved to the first active
// state: that one is the owner's approval, which the board adapters never
// write. Without Rework among the active states only the count is cleared,
// and the owner moves the card. It is safe to call from any goroutine; the
// answer comes on the channel.
func (o *Orchestrator) Unblock(identifier string) <-chan UnblockResult {
	ch := make(chan UnblockResult, 1)
	o.post(func(ctx context.Context) { ch <- o.unblock(ctx, identifier) })
	return ch
}

func (o *Orchestrator) unblock(ctx context.Context, want string) UnblockResult {
	// An issue this service holds: it is running or waiting to be tried
	// again. Its count starts again; nothing on the board changes.
	for id, e := range o.running {
		if SameIdentifier(want, e.issue.Identifier) {
			return o.clearClaimed(id, e.issue.Identifier)
		}
	}
	for id, r := range o.retries {
		if SameIdentifier(want, r.Identifier) {
			return o.clearClaimed(id, r.Identifier)
		}
	}
	blocked, err := o.d.Tracker.FetchIssuesByStates(ctx, []string{o.cfg.Tracker.BlockedState})
	if err != nil {
		return UnblockResult{Err: err}
	}
	for _, is := range blocked {
		if !SameIdentifier(want, is.Identifier) {
			continue
		}
		res := UnblockResult{Identifier: is.Identifier, Found: true}
		if !is.HasLabels(o.cfg.Tracker.RequiredLabels) {
			res.OtherWorker = true
			return res
		}
		res.Failures = o.d.Budget.Failures(is.ID)
		o.d.Budget.ClearIssue(is.ID)
		o.persist()
		if !o.hasActiveState(reworkState) {
			res.Err = errors.New("this installation has no active state " + reworkState + " to move it to")
			return res
		}
		to := reworkState
		if err := o.d.Tracker.SetState(ctx, is, to); err != nil {
			res.Err = err
			return res
		}
		res.MovedTo = to
		is.State = to
		o.comment(ctx, is, "Unblocked by the owner with `maestro unblock`. It starts again with a fresh budget.", "")
		o.d.Log.Info("issue unblocked", "issue", is.Identifier, "cleared_failures", res.Failures, "state", to)
		o.forceTick = true
		o.publish()
		return res
	}
	return UnblockResult{}
}

func (o *Orchestrator) clearClaimed(id, identifier string) UnblockResult {
	res := UnblockResult{Identifier: identifier, Found: true, Failures: o.d.Budget.Failures(id)}
	o.d.Budget.ClearIssue(id)
	o.persist()
	o.d.Log.Info("failure count cleared", "issue", identifier, "cleared_failures", res.Failures)
	o.publish()
	return res
}

// SameIdentifier reports whether what the operator typed names the issue:
// the identifier itself in any case, or only its number ("12", "#12").
func SameIdentifier(typed, identifier string) bool {
	typed = strings.TrimSpace(typed)
	if typed == "" || identifier == "" {
		return false
	}
	if strings.EqualFold(typed, identifier) {
		return true
	}
	num := strings.TrimPrefix(typed, "#")
	if num == "" || strings.Trim(num, "0123456789") != "" {
		return false
	}
	i := strings.LastIndexAny(identifier, "-#")
	return i >= 0 && strings.TrimLeft(identifier[i+1:], "0") == strings.TrimLeft(num, "0")
}
