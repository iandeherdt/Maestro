// Package budget persists the one piece of scheduling state that must
// survive a restart: failure counts, block markers, daily usage, the pause
// flag and the quota window (SPEC sections 4.1.9 and 8.7).
package budget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	fileName = "budget.json"
	dayFmt   = "2006-01-02"
	keepDays = 35
)

// Day is the usage of one local day.
type Day struct {
	Runs         int64 `json:"runs"`
	TotalTokens  int64 `json:"total_tokens"`
	AgentSeconds int64 `json:"agent_seconds"`
}

type record struct {
	FailureAttempts map[string]int64     `json:"failure_attempts"`
	BlockedAt       map[string]time.Time `json:"blocked_at"`
	// TicketTokens are the tokens a ticket's finished attempts used since
	// its budget was last cleared, for budget.max_tokens_per_ticket.
	TicketTokens map[string]int64 `json:"ticket_tokens,omitempty"`
	// LastFailure is what a ticket's last failed attempt failed on, so
	// that a second failure the same way can block it at once.
	LastFailure       map[string]string `json:"last_failure,omitempty"`
	Days              map[string]*Day   `json:"days"`
	Paused            bool              `json:"paused"`
	PauseReason       string            `json:"pause_reason,omitempty"`
	QuotaBlockedUntil *time.Time        `json:"quota_blocked_until,omitempty"`
}

// Store is the Budget Record and the file behind it. It is not safe for
// concurrent use: the orchestrator is its only writer.
type Store struct {
	path string
	rec  record
}

// Open loads the record from dir. A missing file is an empty record. A file
// that cannot be parsed is an error, so that counts are never silently reset.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, fileName)}
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("budget record: %w", err)
	default:
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s.rec); err != nil {
			return nil, fmt.Errorf("budget record %s is corrupt: %w", s.path, err)
		}
	}
	if s.rec.FailureAttempts == nil {
		s.rec.FailureAttempts = map[string]int64{}
	}
	if s.rec.BlockedAt == nil {
		s.rec.BlockedAt = map[string]time.Time{}
	}
	if s.rec.Days == nil {
		s.rec.Days = map[string]*Day{}
	}
	if s.rec.TicketTokens == nil {
		s.rec.TicketTokens = map[string]int64{}
	}
	if s.rec.LastFailure == nil {
		s.rec.LastFailure = map[string]string{}
	}
	return s, nil
}

// Save writes the record atomically: temporary file, fsync, rename.
func (s *Store) Save() error {
	data, err := json.MarshalIndent(&s.rec, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func (s *Store) Failures(id string) int64 { return s.rec.FailureAttempts[id] }

// IncFailure counts one failure attempt and returns the new total.
func (s *Store) IncFailure(id string) int64 {
	s.rec.FailureAttempts[id]++
	return s.rec.FailureAttempts[id]
}

// ClearIssue removes the failure count, the block marker, the token count
// and the last failure of an issue.
func (s *Store) ClearIssue(id string) {
	delete(s.rec.FailureAttempts, id)
	delete(s.rec.BlockedAt, id)
	delete(s.rec.TicketTokens, id)
	delete(s.rec.LastFailure, id)
}

// TicketTokens are the tokens the issue's finished attempts used since its
// budget was last cleared.
func (s *Store) TicketTokens(id string) int64 { return s.rec.TicketTokens[id] }

func (s *Store) AddTicketTokens(id string, n int64) {
	if n > 0 {
		s.rec.TicketTokens[id] += n
	}
}

// LastFailure is the signature of the issue's last failed attempt, or "".
func (s *Store) LastFailure(id string) string { return s.rec.LastFailure[id] }

func (s *Store) SetLastFailure(id, sig string) { s.rec.LastFailure[id] = sig }

func (s *Store) MarkBlocked(id string, at time.Time) { s.rec.BlockedAt[id] = at.UTC() }

func (s *Store) BlockMarked(id string) bool {
	_, ok := s.rec.BlockedAt[id]
	return ok
}

func (s *Store) day(now time.Time) *Day {
	key := now.Format(dayFmt)
	d := s.rec.Days[key]
	if d == nil {
		d = &Day{}
		s.rec.Days[key] = d
		cutoff := now.AddDate(0, 0, -keepDays).Format(dayFmt)
		for k := range s.rec.Days {
			if k < cutoff {
				delete(s.rec.Days, k)
			}
		}
	}
	return d
}

// Today returns the usage of the local day that now falls in.
func (s *Store) Today(now time.Time) Day { return *s.day(now) }

func (s *Store) AddRun(now time.Time) { s.day(now).Runs++ }

func (s *Store) AddUsage(now time.Time, tokens, seconds int64) {
	d := s.day(now)
	d.TotalTokens += tokens
	d.AgentSeconds += seconds
}

func (s *Store) Pause(reason string) { s.rec.Paused, s.rec.PauseReason = true, reason }
func (s *Store) Resume()             { s.rec.Paused, s.rec.PauseReason = false, "" }

func (s *Store) Paused() (bool, string) { return s.rec.Paused, s.rec.PauseReason }

func (s *Store) SetQuotaBlockedUntil(t time.Time) {
	u := t.UTC()
	s.rec.QuotaBlockedUntil = &u
}

// QuotaBlockedUntil returns the zero time when no window is set.
func (s *Store) QuotaBlockedUntil() time.Time {
	if s.rec.QuotaBlockedUntil == nil {
		return time.Time{}
	}
	return *s.rec.QuotaBlockedUntil
}
