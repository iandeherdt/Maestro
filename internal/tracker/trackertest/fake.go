// Package trackertest provides an in-memory tracker for deterministic tests.
package trackertest

import (
	"context"
	"sync"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

// Write records one write the orchestrator made.
type Write struct {
	Op    string // "set_state" or "comment"
	ID    string
	Value string
}

// Fake is an in-memory board.
type Fake struct {
	mu     sync.Mutex
	issues map[string]issue.Issue
	writes []Write

	// FailReads and FailWrites make the next n calls of that kind fail.
	FailReads  int
	FailWrites int
}

func New(issues ...issue.Issue) *Fake {
	f := &Fake{issues: map[string]issue.Issue{}}
	for _, i := range issues {
		f.Put(i)
	}
	return f
}

// Put adds or replaces an issue, as a human editing the board would.
func (f *Fake) Put(i issue.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i.ContentHash == "" {
		i.ContentHash = issue.ContentHash(i.Title, i.Description)
	}
	f.issues[i.ID] = i
}

// Move changes an issue's state, as a human moving a card would.
func (f *Fake) Move(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.issues[id]
	i.State = state
	f.issues[id] = i
}

func (f *Fake) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.issues, id)
}

func (f *Fake) Get(id string) issue.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[id]
}

// Writes returns a copy of the writes made so far.
func (f *Fake) Writes() []Write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Write(nil), f.writes...)
}

// StateWrites returns the states written for one issue, in order.
func (f *Fake) StateWrites(id string) []string {
	var out []string
	for _, w := range f.Writes() {
		if w.Op == "set_state" && w.ID == id {
			out = append(out, w.Value)
		}
	}
	return out
}

func (f *Fake) readErr() error {
	if f.FailReads > 0 {
		f.FailReads--
		return &tracker.Error{Category: tracker.Request, Msg: "injected read failure"}
	}
	return nil
}

func (f *Fake) writeErr() error {
	if f.FailWrites > 0 {
		f.FailWrites--
		return &tracker.Error{Category: tracker.WriteRejected, Msg: "injected write failure"}
	}
	return nil
}

func (f *Fake) FetchIssuesByStates(_ context.Context, states []string) ([]issue.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(states) == 0 {
		return nil, nil
	}
	if err := f.readErr(); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, s := range states {
		want[issue.NormalizeState(s)] = true
	}
	var out []issue.Issue
	for _, i := range f.issues {
		if want[issue.NormalizeState(i.State)] {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *Fake) FetchIssuesByIDs(_ context.Context, ids []string) ([]issue.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ids) == 0 {
		return nil, nil
	}
	if err := f.readErr(); err != nil {
		return nil, err
	}
	var out []issue.Issue
	for _, id := range ids {
		if i, ok := f.issues[id]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *Fake) SetState(_ context.Context, is issue.Issue, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writeErr(); err != nil {
		return err
	}
	if i, ok := f.issues[is.ID]; ok {
		i.State = state
		f.issues[is.ID] = i
	}
	f.writes = append(f.writes, Write{"set_state", is.ID, state})
	return nil
}

func (f *Fake) UpsertStatusComment(_ context.Context, is issue.Issue, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, Write{"comment", is.ID, body})
	return nil
}

func (f *Fake) FetchFeedback(context.Context, issue.Issue, time.Time) ([]tracker.Feedback, error) {
	return nil, nil
}
