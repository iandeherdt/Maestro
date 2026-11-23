package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/sandbox/sandboxtest"
	"github.com/iandeherdt/Maestro/internal/worker"
)

const (
	sid  = "11111111-2222-4333-8444-555555555555"
	sid2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

var ctx = context.Background()

func testConfig() *config.Config {
	return &config.Config{
		Claude: config.Claude{Command: "claude", MaxStepsPerTurn: 150, PermissionMode: "bypassPermissions",
			DisallowedTools: []string{"WebFetch", "WebSearch"},
			TurnTimeoutMS:   5000, TurnTotalTimeoutMS: 10000, ReadTimeoutMS: 5000},
		Sandbox: config.Sandbox{Env: map[string]string{"NODE_ENV": "test"}},
	}
}

type rig struct {
	r      *Runner
	sb     *sandboxtest.Fake
	cfg    *config.Config
	mu     sync.Mutex
	events []orchestrator.Event
	audit  bytes.Buffer
}

// newRig returns a runner whose agent prints the given lines and exits
// with the given status.
func newRig(exit int, stderr string, lines ...string) *rig {
	g := &rig{cfg: testConfig()}
	g.sb = &sandboxtest.Fake{StreamHandler: func(_ context.Context, _ sandbox.Request, out io.Writer) (sandbox.Result, error) {
		for _, l := range lines {
			if _, err := io.WriteString(out, l+"\n"); err != nil {
				return sandbox.Result{}, err
			}
		}
		return sandbox.Result{ExitStatus: exit, Stderr: []byte(stderr)}, nil
	}}
	g.r = &Runner{Config: func() *config.Config { return g.cfg }, Launcher: g.sb,
		NewID:    func() (string, error) { return sid, nil },
		Audit:    func(string) io.Writer { return &g.audit },
		Now:      func() time.Time { return time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC) },
		Location: time.UTC}
	return g
}

func (g *rig) turn(resume string) (worker.TurnResult, error) {
	return g.r.RunTurn(ctx, worker.Turn{WorkspaceKey: "GH-1", Prompt: "do the work", ResumeSessionID: resume},
		func(e orchestrator.Event) {
			g.mu.Lock()
			g.events = append(g.events, e)
			g.mu.Unlock()
		})
}

func (g *rig) kinds() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, e := range g.events {
		out = append(out, e.Kind)
	}
	return strings.Join(out, " ")
}

func category(err error) string {
	var ae *worker.AgentError
	if errors.As(err, &ae) {
		return ae.Category
	}
	if err == nil {
		return "<nil>"
	}
	return "other: " + err.Error()
}

const initLine = `{"type":"system","subtype":"init","session_id":"` + sid + `"}`

func result(subtype, text string, extra string) string {
	return fmt.Sprintf(`{"type":"result","subtype":%q,"result":%q,"session_id":%q%s}`, subtype, text, sid, extra)
}

func TestArgs(t *testing.T) {
	c := testConfig().Claude
	c.Model = "some-model"
	got := strings.Join(Args(c, sid, false), " ")
	want := `claude -p --output-format stream-json --verbose --max-turns 150 --permission-mode bypassPermissions ` +
		`--permission-prompts none --setting-sources user --strict-mcp-config --mcp-config {"mcpServers":{}} ` +
		`--disallowedTools WebFetch,WebSearch --model some-model --session-id ` + sid
	if got != want {
		t.Fatalf("argv =\n%s\nwant\n%s", got, want)
	}
	c.Model, c.DisallowedTools = "", nil
	got = strings.Join(Args(c, sid, true), " ")
	if !strings.HasSuffix(got, `{"mcpServers":{}} --resume `+sid) || strings.Contains(got, "--session-id") {
		t.Fatalf("resume argv = %s", got)
	}
	for _, banned := range []string{"--bare", "--safe-mode", "--restricted", "--dangerously"} {
		if strings.Contains(got, banned) {
			t.Fatalf("argv contains %s", banned)
		}
	}
}

func TestSuccessfulTurn(t *testing.T) {
	g := newRig(0, "",
		initLine,
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":1},"content":[{"type":"text","text":"Looking\nat it"}]}}`,
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":1},"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`,
		`{"type":"permission_denied","tool_name":"WebFetch"}`,
		`{"type":"system","subtype":"hook_started"}`,
		`not json`,
		`{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":20,"output_tokens":7},"content":[]}}`,
		result("success", "All done.\nMAESTRO_STATUS: done", `,"usage":{"input_tokens":40,"output_tokens":15,"cache_read_input_tokens":100,"cache_creation_input_tokens":1}`))
	res, err := g.turn("")
	if err != nil || res.SessionID != sid || res.ResultText != "All done.\nMAESTRO_STATUS: done" {
		t.Fatalf("result %+v, error %v", res, err)
	}
	if got := g.kinds(); got != "session_started notification tool_call tool_result permission_denied other_message malformed notification turn_completed" {
		t.Fatalf("events = %s", got)
	}
	ev := g.events
	if ev[1].Message != "Looking at it" || ev[2].Message != "Bash: go test ./..." || ev[4].Message != "WebFetch" || ev[0].SessionID != sid {
		t.Fatalf("messages = %q %q %q", ev[1].Message, ev[2].Message, ev[4].Message)
	}
	// The repeated message is counted once while streaming; the result's
	// total is what is reported at the end.
	if u := ev[2].Usage; u == nil || u.Input != 10 || u.CacheRead != 100 {
		t.Fatalf("usage during the turn = %+v", u)
	}
	if u := ev[7].Usage; u.Input != 30 || u.Output != 12 {
		t.Fatalf("usage after two messages = %+v", u)
	}
	if u := ev[8].Usage; u == nil || *u != (orchestrator.Usage{Input: 40, Output: 15, CacheRead: 100, CacheWrite: 1}) {
		t.Fatalf("final usage = %+v", u)
	}
	if ev[0].At.IsZero() {
		t.Fatal("events carry no time")
	}

	c := g.sb.Calls()[0]
	if string(c.Stdin) != "do the work" || c.Req.Step != sandbox.StepAgent || c.Req.WorkspaceKey != "GH-1" ||
		c.Req.NetworkProfile != sandbox.NetAgent || c.Req.Limits.TimeoutMS != 70000 || c.Req.Env["NODE_ENV"] != "test" || len(c.Req.Env) != 1 {
		t.Fatalf("request = %+v", c.Req)
	}
	for _, a := range c.Req.Argv {
		if strings.Contains(a, "do the work") {
			t.Fatal("the prompt is in the argument vector")
		}
	}
	if got := strings.Count(g.audit.String(), "\n"); got != 9 || !strings.Contains(g.audit.String(), "not json\n") {
		t.Fatalf("audit has %d lines", got)
	}
}

func TestUsageAccumulatesAcrossTurnsOfOneSession(t *testing.T) {
	use := `,"usage":{"input_tokens":40,"output_tokens":15}`
	g := newRig(0, "", initLine, result("success", "more", use))
	if _, err := g.turn(""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.turn(sid); err != nil {
		t.Fatal(err)
	}
	last := g.events[len(g.events)-1]
	if last.Usage.Input != 80 || last.Usage.Output != 30 {
		t.Fatalf("cumulative usage = %+v", last.Usage)
	}
	argv := strings.Join(g.sb.Calls()[1].Req.Argv, " ")
	if !strings.Contains(argv, "--resume "+sid) || !strings.Contains(argv, "--strict-mcp-config") || !strings.Contains(argv, "--setting-sources user") {
		t.Fatalf("continuation argv = %s", argv)
	}
}

// A turn can name its own model; without one the configured model is used.
func TestTurnModelOverridesTheDefault(t *testing.T) {
	g := newRig(0, "", initLine, result("success", "ok", ""))
	g.cfg.Claude.Model = "sonnet"
	if _, err := g.turn(""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.RunTurn(ctx, worker.Turn{WorkspaceKey: "GH-1", Prompt: "x", Model: "opus"}, func(orchestrator.Event) {}); err != nil {
		t.Fatal(err)
	}
	first, second := strings.Join(g.sb.Calls()[0].Req.Argv, " "), strings.Join(g.sb.Calls()[1].Req.Argv, " ")
	if !strings.Contains(first, "--model sonnet") || !strings.Contains(second, "--model opus") || strings.Contains(second, "sonnet") {
		t.Fatalf("argv:\n%s\n%s", first, second)
	}
}

func TestResumeMayContinueUnderANewSessionID(t *testing.T) {
	g := newRig(0, "", `{"type":"system","subtype":"init","session_id":"`+sid2+`"}`,
		`{"type":"result","subtype":"success","result":"ok","session_id":"`+sid2+`"}`)
	res, err := g.turn(sid)
	if err != nil || res.SessionID != sid2 {
		t.Fatalf("result %+v, error %v", res, err)
	}
	// On a first turn the id must be the one the service chose.
	if _, err := g.turn(""); category(err) != worker.TurnFailed {
		t.Fatalf("first turn with a foreign id: %v", err)
	}
}

func TestSessionIDMustBeAUUID(t *testing.T) {
	g := newRig(0, "", initLine, result("success", "ok", ""))
	if _, err := g.turn("--dangerously-skip-permissions"); category(err) != worker.TurnFailed || len(g.sb.Calls()) != 0 {
		t.Fatalf("error %v, %d launches", err, len(g.sb.Calls()))
	}
}

func TestFailureMapping(t *testing.T) {
	retry := func(e string) string {
		return `{"type":"system","subtype":"api_retry","attempt":2,"error_status":429,"error":"` + e + `"}`
	}
	for name, tc := range map[string]struct {
		exit   int
		stderr string
		lines  []string
		resume bool
		want   string
	}{
		"success":                       {0, "", []string{initLine, result("success", "ok", "")}, false, "<nil>"},
		"max steps":                     {1, "", []string{initLine, result("error_max_turns", "", "")}, false, MaxSteps},
		"error during execution":        {1, "", []string{initLine, result("error_during_execution", "", "")}, false, worker.TurnFailed},
		"success but non-zero exit":     {1, "", []string{initLine, result("success", "ok", "")}, false, worker.TurnFailed},
		"success flagged as error":      {0, "", []string{initLine, result("success", "ok", `,"is_error":true`)}, false, worker.TurnFailed},
		"exit without result":           {1, "boom", []string{initLine}, false, PortExit},
		"clean exit without result":     {0, "", []string{initLine}, false, PortExit},
		"command not found":             {127, "claude: not found", nil, false, AgentNotFound},
		"127 after events":              {127, "", []string{initLine}, false, PortExit},
		"not logged in, by text":        {1, "", []string{initLine, result("success", "Not logged in · Please run /login", `,"is_error":true`)}, false, worker.NotLoggedIn},
		"login expired, exit zero":      {0, "", []string{initLine, result("success", "Login expired · Please run /login", "")}, false, worker.NotLoggedIn},
		"not logged in, on stderr":      {1, "Not logged in · Please run /login", nil, false, worker.NotLoggedIn},
		"auth retry then failure":       {1, "", []string{initLine, retry("authentication_failed"), result("error_during_execution", "", "")}, false, worker.NotLoggedIn},
		"auth retry then success":       {0, "", []string{initLine, retry("authentication_failed"), result("success", "ok", "")}, false, "<nil>"},
		"limit, by text":                {1, "", []string{initLine, result("error_during_execution", "You've hit your session limit · resets 3:45pm", "")}, false, worker.QuotaExhausted},
		"limit, exit zero":              {0, "", []string{initLine, result("success", "You’ve hit your weekly limit", "")}, false, worker.QuotaExhausted},
		"rate retry then failure":       {1, "", []string{initLine, retry("rate_limit"), result("error_during_execution", "", "")}, false, worker.QuotaExhausted},
		"billing retry then failure":    {1, "", []string{initLine, retry("billing_error"), result("error_during_execution", "", "")}, false, worker.QuotaExhausted},
		"rate retry then success":       {0, "", []string{initLine, retry("rate_limit"), result("success", "ok", "")}, false, "<nil>"},
		"summary that mentions a limit": {0, "", []string{initLine, result("success", "Fixed the bug where users hit your rate limit too early.", "")}, false, "<nil>"},
		"unknown session on resume":     {1, "No conversation found with session ID: x", nil, true, worker.SessionUnknown},
		"unknown session, in result":    {1, "", []string{result("error_during_execution", "No conversation found", "")}, true, worker.SessionUnknown},
		"same text on a first turn":     {1, "No conversation found with session ID: x", nil, false, PortExit},
	} {
		g := newRig(tc.exit, tc.stderr, tc.lines...)
		resume := ""
		if tc.resume {
			resume = sid
		}
		_, err := g.turn(resume)
		if got := category(err); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestQuotaResetTime(t *testing.T) {
	g := newRig(1, "", initLine, result("error_during_execution", "You've hit your session limit · resets 3:45pm", ""))
	_, err := g.turn("")
	var ae *worker.AgentError
	if !errors.As(err, &ae) || !ae.QuotaResetAt.Equal(time.Date(2026, 11, 21, 15, 45, 0, 0, time.UTC)) {
		t.Fatalf("reset at %v (%v)", ae, err)
	}
	if !strings.Contains(g.kinds(), "turn_failed") {
		t.Fatalf("events = %s", g.kinds())
	}
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	for text, want := range map[string]time.Time{
		"resets 9am":      time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC),
		"resets 12:05 AM": time.Date(2026, 11, 22, 0, 5, 0, 0, time.UTC),
		"resets 12pm":     time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
		"resets 11:59pm":  time.Date(2026, 11, 21, 23, 59, 0, 0, time.UTC),
		"resets 2pm":      time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC),
		"resets 13pm":     {},
		"resets 3:75pm":   {},
		"resets soon":     {},
	} {
		if got := resetTime(text, now); !got.Equal(want) {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

func TestLauncherFailure(t *testing.T) {
	g := newRig(0, "")
	g.sb.StreamHandler = func(context.Context, sandbox.Request, io.Writer) (sandbox.Result, error) {
		return sandbox.Result{}, &sandbox.Error{Msg: "no unit"}
	}
	if _, err := g.turn(""); category(err) != sandbox.LaunchFailed {
		t.Fatalf("error = %v", err)
	}
}

// hang returns a handler that prints lines and then waits to be killed.
func hang(killed chan<- struct{}, lines ...string) func(context.Context, sandbox.Request, io.Writer) (sandbox.Result, error) {
	return func(c context.Context, _ sandbox.Request, out io.Writer) (sandbox.Result, error) {
		for _, l := range lines {
			io.WriteString(out, l+"\n")
		}
		<-c.Done()
		close(killed)
		return sandbox.Result{ExitStatus: -1}, c.Err()
	}
}

func TestTimeouts(t *testing.T) {
	for name, tc := range map[string]struct {
		lines             []string
		read, turn, total int64
		want              string
	}{
		"no first event": {nil, 30, 5000, 5000, ResponseTimeout},
		"silence":        {[]string{initLine}, 5000, 30, 5000, TurnTimeout},
		"wall clock":     {[]string{initLine}, 5000, 5000, 30, TurnTimeout},
	} {
		g := newRig(0, "")
		killed := make(chan struct{})
		g.sb.StreamHandler = hang(killed, tc.lines...)
		g.cfg.Claude.ReadTimeoutMS, g.cfg.Claude.TurnTimeoutMS, g.cfg.Claude.TurnTotalTimeoutMS = tc.read, tc.turn, tc.total
		_, err := g.turn("")
		if got := category(err); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
		select {
		case <-killed:
		default:
			t.Errorf("%s: the sandbox unit was not killed before the turn returned", name)
		}
	}
}

func TestCancellationKillsTheUnit(t *testing.T) {
	g := newRig(0, "")
	killed := make(chan struct{})
	g.sb.StreamHandler = hang(killed, initLine)
	c, cancel := context.WithCancel(ctx)
	go func() {
		for g.kinds() == "" {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := g.r.RunTurn(c, worker.Turn{WorkspaceKey: "GH-1", Prompt: "x"}, func(e orchestrator.Event) {
		g.mu.Lock()
		g.events = append(g.events, e)
		g.mu.Unlock()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	<-killed
}

func TestOverlongLineIsDroppedNotBuffered(t *testing.T) {
	var lines []rawLine
	out := make(chan rawLine)
	in := "short\n" + strings.Repeat("x", 300_000) + "\n\n" + `{"a":1}` + "\r\nlast"
	go readLines(strings.NewReader(in), 100_000, out, make(chan struct{}))
	for l := range out {
		lines = append(lines, l)
	}
	if len(lines) != 4 || string(lines[0].data) != "short" || !lines[1].tooLong || len(lines[1].data) != 0 ||
		string(lines[2].data) != `{"a":1}` || string(lines[3].data) != "last" {
		t.Fatalf("lines = %+v", lines)
	}

	g := newRig(0, "", initLine, strings.Repeat("y", 200_000), result("success", "ok", ""))
	if _, err := g.turn(""); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryIsOneBoundedLine(t *testing.T) {
	got := summary("  a\x1b[31m\nb\x00" + strings.Repeat("é", 500))
	if strings.ContainsAny(got, "\x1b\n\x00") || len([]rune(got)) != 200 || !strings.HasPrefix(got, "a [31m b ") {
		t.Fatalf("summary = %q", got)
	}
}

type fakeTrace struct {
	lines []string
	ended int
	err   error
}

func (f *fakeTrace) Line(raw []byte) { f.lines = append(f.lines, string(raw)) }
func (f *fakeTrace) End(err error)   { f.ended++; f.err = err }

func TestTraceSeesEveryLineAndTheEnd(t *testing.T) {
	g := newRig(0, "", initLine, result("success", "done", ""))
	ft := &fakeTrace{}
	var got worker.Turn
	g.r.Trace = func(t worker.Turn) TurnTrace { got = t; return ft }
	if _, err := g.turn(""); err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceKey != "GH-1" || len(ft.lines) != 2 || ft.lines[0] != initLine || ft.ended != 1 || ft.err != nil {
		t.Errorf("turn %+v, trace %+v", got, ft)
	}

	g = newRig(3, "", initLine)
	ft = &fakeTrace{}
	g.r.Trace = func(worker.Turn) TurnTrace { return ft }
	if _, err := g.turn(""); err == nil || ft.ended != 1 || ft.err == nil {
		t.Errorf("a failed turn is not ended with its error: %+v", ft)
	}
}
