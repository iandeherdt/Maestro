// Package claude drives Claude Code in print mode for one turn (SPEC
// section 10). It builds the argument vector, hands the prompt to the
// sandbox launcher on standard input, reads the JSON stream, and maps what
// it sees to runtime events and to one of the normalized outcomes.
//
// Nothing here holds or passes a credential. The CLI uses the login stored
// in the sandbox user's profile.
package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// Failure categories decided by the runner (SPEC 10.7).
const (
	AgentNotFound   = "agent_not_found"
	ResponseTimeout = "response_timeout"
	TurnTimeout     = "turn_timeout"
	PortExit        = "port_exit"
	MaxSteps        = "max_steps"
)

const (
	// MaxLineBytes is the longest stream line that is interpreted.
	MaxLineBytes = 10 << 20
	// unitGraceMS is added to the turn's wall-clock cap for the sandbox
	// unit's own limit, so a hung runner cannot leave an agent running.
	unitGraceMS  = 60_000
	summaryRunes = 200
	// maxSessions bounds how many sessions' token totals are remembered.
	maxSessions = 256
)

// Runner implements worker.Runner.
type Runner struct {
	Config   func() *config.Config
	Launcher sandbox.Streamer
	// Audit, when set, returns where the raw stream of a turn is appended
	// before it is interpreted (SPEC 16.2). A failing writer is ignored.
	Audit func(workspaceKey string) io.Writer
	// Trace, when set, starts the trace of a turn: it is given each stream
	// line next to the audit, and how the turn ended. It returns nil when
	// traces are off.
	Trace func(t worker.Turn) TurnTrace
	// Now, NewID and Location are for tests. Location is where "resets
	// 3:45pm" is read; it defaults to the service's local time zone.
	Now      func() time.Time
	NewID    func() (string, error)
	Location *time.Location

	mu       sync.Mutex
	sessions map[string]orchestrator.Usage
	order    []string
}

var _ worker.Runner = (*Runner)(nil)

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) location() *time.Location {
	if r.Location != nil {
		return r.Location
	}
	return time.Local
}

// newUUID returns a random version 4 UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

var sessionRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Args builds the argument vector of one turn (SPEC 10.1). Every flag is
// passed on every turn: the CLI does not restore them on resume.
func Args(c config.Claude, sessionID string, resume bool) []string {
	argv := []string{c.Command, "-p",
		"--output-format", "stream-json", "--verbose",
		"--max-turns", strconv.FormatInt(c.MaxStepsPerTurn, 10),
		"--permission-mode", c.PermissionMode,
		"--permission-prompts", "none",
		// Without these two, print mode runs the repository's own hooks
		// and MCP servers with no trust prompt.
		"--setting-sources", "user",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
	}
	if len(c.DisallowedTools) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(c.DisallowedTools, ","))
	}
	if c.Model != "" {
		argv = append(argv, "--model", c.Model)
	}
	if resume {
		return append(argv, "--resume", sessionID)
	}
	return append(argv, "--session-id", sessionID)
}

// InteractiveArgs builds the argument vector of the owner's own
// conversation with the agent on a terminal (`maestro talk`). It keeps
// what keeps the agent apart from the repository: no hooks or MCP servers
// of the repository, and the same tools taken away. With resume the
// session is continued, otherwise a new one with that id is started.
func InteractiveArgs(c config.Claude, sessionID string, resume bool) ([]string, error) {
	if !sessionRE.MatchString(sessionID) {
		return nil, errors.New("the session id is not a UUID")
	}
	argv := []string{c.Command,
		"--permission-mode", c.PermissionMode,
		"--setting-sources", "user",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
	}
	if len(c.DisallowedTools) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(c.DisallowedTools, ","))
	}
	if c.Model != "" {
		argv = append(argv, "--model", c.Model)
	}
	if resume {
		return append(argv, "--resume", sessionID), nil
	}
	return append(argv, "--session-id", sessionID), nil
}

// NewSessionID makes the id of a new session.
func NewSessionID() (string, error) { return newUUID() }

// ValidSessionID reports whether s has the shape of a session id.
func ValidSessionID(s string) bool { return sessionRE.MatchString(s) }

type usageJSON struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u usageJSON) usage() orchestrator.Usage {
	return orchestrator.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

func add(a, b orchestrator.Usage) orchestrator.Usage {
	return orchestrator.Usage{Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite}
}

// streamEvent is the part of a stream line the runner reads. Everything in
// it was produced inside the sandbox and is untrusted.
type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	Error     string `json:"error"`
	Attempt   int    `json:"attempt"`
	Status    int    `json:"error_status"`
	ToolName  string `json:"tool_name"`
	Usage     *usageJSON
	Message   struct {
		ID      string          `json:"id"`
		Usage   *usageJSON      `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	Input struct {
		Command string `json:"command"`
	} `json:"input"`
}

// summary makes untrusted text safe for a one-line status: control
// characters become spaces and the length is capped.
func summary(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n >= summaryRunes {
			break
		}
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

type turnState struct {
	base      orchestrator.Usage // earlier turns of the session
	live      orchestrator.Usage // this turn, from assistant messages
	seenMsg   map[string]bool
	sessionID string
	events    int
	authRetry bool
	rateRetry bool
	result    *streamEvent
	// foreignSession: a first turn reported an id the service did not
	// choose. The turn is failed, because its session cannot be resumed
	// with confidence.
	foreignSession bool
}

// TurnTrace receives what a turn does, for a trace. It must not block.
type TurnTrace interface {
	Line(raw []byte)
	End(err error)
}

// RunTurn runs one turn and waits for it. When ctx ends the sandbox unit
// is killed through the launcher's context.
func (r *Runner) RunTurn(ctx context.Context, t worker.Turn, onEvent func(orchestrator.Event)) (worker.TurnResult, error) {
	var tr TurnTrace
	if r.Trace != nil {
		tr = r.Trace(t)
	}
	res, err := r.runTurn(ctx, t, onEvent, tr)
	if tr != nil {
		tr.End(err)
	}
	return res, err
}

func (r *Runner) runTurn(ctx context.Context, t worker.Turn, onEvent func(orchestrator.Event), tr TurnTrace) (worker.TurnResult, error) {
	var none worker.TurnResult
	cfg := r.Config()
	c := cfg.Claude
	if t.Model != "" {
		c.Model = t.Model
	}
	id, resume := t.ResumeSessionID, t.ResumeSessionID != ""
	if !resume {
		newID := r.NewID
		if newID == nil {
			newID = newUUID
		}
		var err error
		if id, err = newID(); err != nil {
			return none, &worker.AgentError{Category: sandbox.LaunchFailed, Msg: "no session id could be generated"}
		}
	}
	// The id goes into the argument vector, so only the exact shape passes.
	if !sessionRE.MatchString(id) {
		return none, &worker.AgentError{Category: worker.TurnFailed, Msg: "the session id is not a UUID"}
	}
	req := sandbox.Request{
		Step: sandbox.StepAgent, WorkspaceKey: t.WorkspaceKey,
		Argv:           Args(c, id, resume),
		NetworkProfile: sandbox.NetAgent,
		Limits:         sandbox.Limits{TimeoutMS: c.TurnTotalTimeoutMS + unitGraceMS},
		Env:            cfg.Sandbox.Env,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close() // unblocks the launcher if the turn is abandoned
	type ended struct {
		res sandbox.Result
		err error
	}
	done := make(chan ended, 1)
	go func() {
		res, err := r.Launcher.Stream(runCtx, req, []byte(t.Prompt), pw)
		pw.Close()
		done <- ended{res, err}
	}()
	lines := make(chan rawLine)
	stop := make(chan struct{})
	defer close(stop)
	go readLines(pr, MaxLineBytes, lines, stop)

	var audit io.Writer
	if r.Audit != nil {
		audit = r.Audit(t.WorkspaceKey)
	}
	st := &turnState{seenMsg: map[string]bool{}}
	if resume {
		r.mu.Lock()
		st.base = r.sessions[id]
		r.mu.Unlock()
	}
	emit := func(kind, msg string, withUsage bool) {
		ev := orchestrator.Event{Kind: kind, At: r.now().UTC(), Message: msg, SessionID: st.sessionID}
		if withUsage {
			u := add(st.base, st.live)
			ev.Usage = &u
		}
		onEvent(ev)
	}

	idle := time.NewTimer(time.Duration(c.ReadTimeoutMS) * time.Millisecond)
	defer idle.Stop()
	total := time.NewTimer(time.Duration(c.TurnTotalTimeoutMS) * time.Millisecond)
	defer total.Stop()
	timeout := func(category, msg string) (worker.TurnResult, error) {
		cancel()
		pr.Close()
		<-done // the launcher kills the unit when its context ends
		emit(category, msg, false)
		return none, &worker.AgentError{Category: category, Msg: msg}
	}
loop:
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(time.Duration(c.TurnTimeoutMS) * time.Millisecond)
			if audit != nil && !l.tooLong {
				audit.Write(append(l.data, '\n'))
			}
			if tr != nil && !l.tooLong {
				tr.Line(l.data)
			}
			st.events++
			r.handle(st, l, id, resume, emit)
		case <-idle.C:
			if st.events == 0 {
				return timeout(ResponseTimeout, "the agent produced no output after it was started")
			}
			return timeout(TurnTimeout, "the agent was silent for too long")
		case <-total.C:
			return timeout(TurnTimeout, "the turn ran longer than its wall-clock limit")
		case <-ctx.Done():
			cancel()
			pr.Close()
			<-done
			return none, ctx.Err()
		}
	}
	end := <-done
	if ctx.Err() != nil {
		return none, ctx.Err()
	}
	if end.err != nil {
		return none, &worker.AgentError{Category: sandbox.LaunchFailed, Msg: summary(end.err.Error())}
	}
	res, err := r.classify(st, end.res, id, resume)
	if err != nil {
		var ae *worker.AgentError
		if errors.As(err, &ae) {
			emit(eventFor(ae.Category), ae.Msg, true)
		}
		return none, err
	}
	r.remember(id, res.SessionID, add(st.base, st.live))
	emit("turn_completed", "", true)
	return res, nil
}

func eventFor(category string) string {
	if category == MaxSteps {
		return "turn_ended_with_error"
	}
	return "turn_failed"
}

// remember keeps the session's token total for its next turn.
func (r *Runner) remember(oldID, newID string, u orchestrator.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]orchestrator.Usage{}
	}
	if oldID != newID {
		delete(r.sessions, oldID)
	}
	if _, ok := r.sessions[newID]; !ok {
		r.order = append(r.order, newID)
	}
	r.sessions[newID] = u
	for len(r.order) > maxSessions {
		delete(r.sessions, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *Runner) handle(st *turnState, l rawLine, id string, resume bool, emit func(kind, msg string, withUsage bool)) {
	if l.tooLong {
		emit("malformed", "a stream line was longer than the limit", false)
		return
	}
	var ev streamEvent
	if err := json.Unmarshal(l.data, &ev); err != nil || ev.Type == "" {
		emit("malformed", "", false)
		return
	}
	switch {
	case ev.Type == "system" && ev.Subtype == "init":
		// A first turn must report the id the service chose. On resume
		// the CLI may continue under a new id, which is then used for
		// the next turn.
		if !resume && ev.SessionID != id {
			st.foreignSession = true
			emit("malformed", "the agent reported a session id the service did not choose", false)
			return
		}
		if sessionRE.MatchString(ev.SessionID) {
			st.sessionID = ev.SessionID
		}
		emit("session_started", "", false)
	case ev.Type == "assistant":
		if u := ev.Message.Usage; u != nil && !st.seenMsg[ev.Message.ID] {
			// One model response can arrive as several lines that repeat
			// the same usage; it is counted once.
			st.seenMsg[ev.Message.ID] = true
			st.live = add(st.live, u.usage())
		}
		var blocks []block
		json.Unmarshal(ev.Message.Content, &blocks)
		sent := false
		for _, b := range blocks {
			switch b.Type {
			case "text":
				emit("notification", summary(b.Text), true)
				sent = true
			case "tool_use":
				msg := summary(b.Name)
				if b.Input.Command != "" {
					msg = summary(b.Name + ": " + b.Input.Command)
				}
				emit("tool_call", msg, true)
				sent = true
			}
		}
		if !sent {
			emit("notification", "", true)
		}
	case ev.Type == "user":
		emit("tool_result", "", false)
	case ev.Type == "system" && ev.Subtype == "api_retry":
		emit("api_retry", summary(fmt.Sprintf("attempt %d, status %d, %s", ev.Attempt, ev.Status, ev.Error)), false)
		switch ev.Error {
		case "authentication_failed":
			st.authRetry = true
		case "rate_limit", "billing_error":
			st.rateRetry = true
			emit("rate_limit", summary(ev.Error), false)
		}
	case ev.Type == "permission_denied":
		emit("permission_denied", summary(ev.ToolName), false)
	case ev.Type == "result":
		e := ev
		st.result = &e
		if ev.Usage != nil {
			// The result's own total replaces the running sum.
			st.live = ev.Usage.usage()
		}
		if sessionRE.MatchString(ev.SessionID) {
			st.sessionID = ev.SessionID
		}
	default:
		emit("other_message", "", false)
	}
}

var (
	loginRE   = regexp.MustCompile(`^(Not logged in|Login expired)`)
	limitRE   = regexp.MustCompile(`^You['’]ve hit your`)
	quotaRE   = regexp.MustCompile(`(?i)\bhit your\b.{0,60}\blimit\b`)
	resetRE   = regexp.MustCompile(`(?i)\bresets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	unknownRE = regexp.MustCompile(`(?i)no conversation found`)
)

// classify decides how the turn ended (SPEC 10.3 and 10.7). The exit
// status and result subtype for a missing login and for a usage limit are
// not documented, so the result text is looked at first.
func (r *Runner) classify(st *turnState, res sandbox.Result, id string, resume bool) (worker.TurnResult, error) {
	var none worker.TurnResult
	fail := func(category, msg string) (worker.TurnResult, error) {
		return none, &worker.AgentError{Category: category, Msg: msg}
	}
	stderr := string(res.Stderr)
	if st.result == nil {
		switch {
		case resume && unknownRE.MatchString(stderr):
			return fail(worker.SessionUnknown, "the agent no longer knows the session")
		case loginRE.MatchString(strings.TrimSpace(stderr)) || st.authRetry:
			return fail(worker.NotLoggedIn, "the agent is not logged in")
		case st.events == 0 && (res.ExitStatus == 127 || res.ExitStatus == 126):
			return fail(AgentNotFound, "the agent executable could not be run in the sandbox")
		}
		return fail(PortExit, fmt.Sprintf("the agent exited with status %d without a result", res.ExitStatus))
	}
	text := strings.TrimSpace(st.result.Result)
	failed := st.result.Subtype != "success" || st.result.IsError || res.ExitStatus != 0
	quota := func() (worker.TurnResult, error) {
		return none, &worker.AgentError{Category: worker.QuotaExhausted, Msg: "the agent reported a usage limit",
			QuotaResetAt: resetTime(text, r.now().In(r.location()))}
	}
	switch {
	case loginRE.MatchString(text):
		return fail(worker.NotLoggedIn, "the agent is not logged in")
	case quotaRE.MatchString(text) && (failed || limitRE.MatchString(text)):
		// A turn that succeeded is read as a usage limit only when the
		// text starts the way the CLI words it, so that an agent's own
		// summary mentioning a limit is not mistaken for one.
		return quota()
	case st.foreignSession:
		return fail(worker.TurnFailed, "the agent reported a session id the service did not choose")
	case !failed:
		sid := st.sessionID
		if sid == "" {
			sid = id
		}
		return worker.TurnResult{SessionID: sid, ResultText: st.result.Result}, nil
	case resume && (unknownRE.MatchString(text) || unknownRE.MatchString(stderr)):
		return fail(worker.SessionUnknown, "the agent no longer knows the session")
	case st.authRetry:
		return fail(worker.NotLoggedIn, "the agent is not logged in")
	case st.rateRetry:
		return quota()
	case st.result.Subtype == "error_max_turns":
		return fail(MaxSteps, "the agent used all its steps for this turn")
	}
	return fail(worker.TurnFailed, summary(fmt.Sprintf("result %s, exit status %d", st.result.Subtype, res.ExitStatus)))
}

// resetTime reads "resets 3:45pm" as the next time the clock shows that.
// It returns the zero time when the text carries no such time.
func resetTime(text string, now time.Time) time.Time {
	m := resetRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	h, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	if h < 1 || h > 12 || min > 59 {
		return time.Time{}
	}
	h %= 12
	if strings.EqualFold(m[3], "pm") {
		h += 12
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

type rawLine struct {
	data    []byte
	tooLong bool
}

// readLines splits r into lines. A line longer than max is reported once
// as tooLong and its bytes are dropped, so memory stays bounded.
func readLines(r io.Reader, max int, out chan<- rawLine, stop <-chan struct{}) {
	defer close(out)
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	tooLong := false
	send := func() bool {
		data := []byte(strings.TrimRight(string(buf), "\r\n"))
		l := rawLine{data: data, tooLong: tooLong}
		buf, tooLong = nil, false
		if len(data) == 0 && !l.tooLong {
			return true
		}
		select {
		case out <- l:
			return true
		case <-stop:
			return false
		}
	}
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !send() || err != nil {
			return
		}
	}
}
