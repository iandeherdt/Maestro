package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

// Round is one thing the service posted on an idea: questions or a plan.
type Round struct {
	At   time.Time `json:"at"`
	Plan Plan      `json:"plan"`
	// Posted is false while the comment could not be written.
	Posted bool `json:"posted"`
}

// Progress is how far one task of an approved plan has been created.
type Progress struct {
	Ref     tracker.TaskRef `json:"ref"`
	Ordered bool            `json:"ordered"`
	Placed  bool            `json:"placed"`
}

// Record is what the service remembers about one idea.
type Record struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier"`
	// ContentHash is the text of the idea the latest round was made for.
	ContentHash string  `json:"content_hash"`
	Rounds      []Round `json:"rounds"`
	// Progress and Done belong to the latest round, once it is approved.
	Progress []Progress `json:"progress,omitempty"`
	Done     bool       `json:"done,omitempty"`
}

// Store keeps one file per idea in a directory only the service uses.
type Store struct{ dir string }

// OpenStore creates the directory if needed.
func OpenStore(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("plan store: the directory must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(issueID string) string {
	sum := sha256.Sum256([]byte(issueID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".json")
}

// Load returns the record of an idea, or nil when there is none.
func (s *Store) Load(issueID string) (*Record, error) {
	raw, err := os.ReadFile(s.path(issueID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil || r.IssueID != issueID {
		return nil, fmt.Errorf("plan store: the record of this issue is damaged; remove %s to start over", s.path(issueID))
	}
	return &r, nil
}

// Save writes the record atomically.
func (s *Store) Save(r *Record) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(r.IssueID) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(r.IssueID))
}

// Delete forgets an idea. A record that does not exist is not an error.
func (s *Store) Delete(issueID string) error {
	err := os.Remove(s.path(issueID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
