package claude

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// LastSession finds the id of the last session in an agent's raw stream,
// as the audit file keeps it. Only the session_id field of a whole JSON
// line counts. Empty means there is none.
func LastSession(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	last := ""
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var l struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &l) == nil && sessionRE.MatchString(l.SessionID) {
			last = l.SessionID
		}
	}
	return last
}

// Conversations remembers the sessions the owner had with the agent on a
// terminal (`maestro talk`), one per workspace, so that the next run on
// the ticket continues that conversation instead of starting afresh. The
// files are in the service's own state directory; nothing in the sandbox
// can write them.
type Conversations struct{ Dir string }

func (c Conversations) path(key string) (string, bool) {
	if c.Dir == "" || !sandbox.ValidKey(key) {
		return "", false
	}
	return filepath.Join(c.Dir, key+".talk"), true
}

// Save remembers the session of a conversation on a workspace.
func (c Conversations) Save(key, sessionID string) error {
	p, ok := c.path(key)
	if !ok || !sessionRE.MatchString(sessionID) {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(sessionID+"\n"), 0o600)
}

// Pending is the session of a conversation on a workspace that no run has
// continued yet, or "".
func (c Conversations) Pending(key string) string {
	p, ok := c.path(key)
	if !ok {
		return ""
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(raw)); sessionRE.MatchString(id) {
		return id
	}
	return ""
}

// Done forgets the conversation once a run has continued it.
func (c Conversations) Done(key string) {
	if p, ok := c.path(key); ok {
		os.Remove(p)
	}
}

// Last is the session to continue on a workspace: a pending conversation,
// or else the agent's last session in the audit file.
func (c Conversations) Last(key, auditFile string) string {
	if id := c.Pending(key); id != "" {
		return id
	}
	f, err := os.Open(auditFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	return LastSession(f)
}
