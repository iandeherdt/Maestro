// Package trace sends what an agent does in a turn to an OpenTelemetry
// collector as a trace, in the OpenInference conventions that Arize Phoenix
// shows: one AGENT span per turn, an LLM span for each thing the model says
// or thinks, and a TOOL span for each tool call with its input and result.
//
// It reads the same stream the runner reads, on the host. Nothing is added
// to the sandbox, and the sandbox gets no network for it. Everything in the
// stream was produced inside the sandbox and is untrusted; it is sent as
// data and never interpreted beyond its JSON shape.
//
// Sending never holds up a turn: spans go to a bounded queue that one
// goroutine posts, and what does not fit or fails is dropped and logged.
package trace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaxValue is the most bytes of one input or output that is sent.
const MaxValue = 32 << 10

// flushEvery is how often a running turn sends the spans it has finished,
// so that a long turn shows up while it runs.
const flushEvery = 3 * time.Second

// queueLen bounds the batches waiting to be posted.
const queueLen = 64

// Endpoint is where a turn's trace goes.
type Endpoint struct {
	// URL is the collector's OTLP/HTTP traces URL, such as
	// http://127.0.0.1:6006/v1/traces for Phoenix.
	URL string
	// Project is the Phoenix project the traces are filed under.
	Project string
}

// Exporter posts traces. Its zero value is not usable; use New.
type Exporter struct {
	Version string
	client  *http.Client
	log     *slog.Logger
	queue   chan batch
	once    sync.Once
	mu      sync.Mutex
	failing bool
}

type batch struct {
	url, project string
	spans        []span
}

// New returns an exporter. log may be nil.
func New(version string, log *slog.Logger) *Exporter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Exporter{Version: version, log: log, queue: make(chan batch, queueLen),
		client: &http.Client{Timeout: 10 * time.Second,
			// A collector does not redirect; following one could send the
			// agent's text somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *Exporter) start() {
	e.once.Do(func() {
		go func() {
			for b := range e.queue {
				e.post(b)
			}
		}()
	})
}

func (e *Exporter) post(b batch) {
	body := encode([]kv{str("service.name", "maestro"), str("openinference.project.name", b.project)},
		"maestro", e.Version, b.spans)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		e.failed(err)
		return
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	res, err := e.client.Do(req)
	if err != nil {
		e.failed(err)
		return
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		e.failed(fmt.Errorf("the collector answered %d", res.StatusCode))
		return
	}
	e.mu.Lock()
	if e.failing {
		e.log.Info("traces are being sent again", "url", b.url)
	}
	e.failing = false
	e.mu.Unlock()
}

// failed logs the first of a run of failures, not each one.
func (e *Exporter) failed(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.failing {
		e.log.Warn("traces could not be sent; the agents are not affected", "error", err)
	}
	e.failing = true
}

func (e *Exporter) send(ep Endpoint, spans []span) {
	if len(spans) == 0 {
		return
	}
	e.start()
	select {
	case e.queue <- batch{ep.URL, ep.Project, spans}:
	default:
		e.failed(fmt.Errorf("the queue is full; %d spans were dropped", len(spans)))
	}
}

// Turn starts the trace of one agent turn. ticket names the issue the
// turn works on; it is the session that Phoenix groups the turns under.
func (e *Exporter) Turn(ep Endpoint, ticket, prompt, model string) *Turn {
	t := &Turn{e: e, ep: ep, now: time.Now, tools: map[string]*span{}, seen: map[string]bool{}}
	start := t.now()
	t.root = span{traceID: newID16(), spanID: newID8(), name: "turn " + ticket, start: start.UnixNano()}
	t.root.attrs = []kv{
		str("openinference.span.kind", "AGENT"),
		str("session.id", ticket),
		str("maestro.ticket", ticket),
		str("input.value", clip(prompt)),
	}
	t.model = model
	t.last, t.flushed = start, start
	return t
}

// Turn collects the spans of one turn. Its methods are called from one
// goroutine.
type Turn struct {
	e       *Exporter
	ep      Endpoint
	now     func() time.Time
	root    span
	model   string
	session string
	tools   map[string]*span // open tool calls by tool_use id
	seen    map[string]bool  // message ids whose usage is counted
	done    []span
	last    time.Time // when the previous line arrived
	flushed time.Time
	result  string
	usage   *usage
	cost    float64
	ended   bool
}

type usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u *usage) attrs() []kv {
	prompt := u.Input + u.CacheRead + u.CacheWrite
	return []kv{
		num("llm.token_count.prompt", prompt),
		num("llm.token_count.completion", u.Output),
		num("llm.token_count.total", prompt+u.Output),
		num("llm.token_count.prompt_details.cache_read", u.CacheRead),
		num("llm.token_count.prompt_details.cache_write", u.CacheWrite),
	}
}

type line struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	Model     string  `json:"model"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	NumTurns  int64   `json:"num_turns"`
	Cost      float64 `json:"total_cost_usd"`
	Usage     *usage  `json:"usage"`
	Message   struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   *usage          `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Line reads one line of the agent's stream.
func (t *Turn) Line(raw []byte) {
	if t == nil || t.ended {
		return
	}
	now := t.now()
	defer func() {
		t.last = now
		if now.Sub(t.flushed) >= flushEvery {
			t.flush(now)
		}
	}()
	var l line
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			if l.Model != "" {
				t.model = l.Model
			}
			t.session = l.SessionID
		}
	case "assistant":
		var blocks []block
		json.Unmarshal(l.Message.Content, &blocks)
		var u *usage
		if l.Message.Usage != nil && !t.seen[l.Message.ID] {
			t.seen[l.Message.ID] = true
			u = l.Message.Usage
		}
		model := t.model
		if l.Message.Model != "" {
			model = l.Message.Model
		}
		for _, b := range blocks {
			switch b.Type {
			case "text", "thinking":
				text, name := b.Text, "says"
				if b.Type == "thinking" {
					text, name = b.Thinking, "thinks"
				}
				if strings.TrimSpace(text) == "" {
					continue
				}
				s := t.child(name, t.last, now, "LLM")
				s.attrs = append(s.attrs, str("output.value", clip(text)))
				if model != "" {
					s.attrs = append(s.attrs, str("llm.model_name", model), str("llm.provider", "anthropic"))
				}
				if u != nil {
					s.attrs = append(s.attrs, u.attrs()...)
					u = nil
				}
				t.done = append(t.done, s)
			case "tool_use":
				name := b.Name
				if name == "" {
					name = "tool"
				}
				s := t.child(clipName(name), now, now, "TOOL")
				s.attrs = append(s.attrs, str("tool.name", clipName(name)),
					str("input.value", clip(string(b.Input))), str("input.mime_type", "application/json"))
				if b.ID != "" && len(t.tools) < 1000 {
					t.tools[b.ID] = &s
				} else {
					t.done = append(t.done, s)
				}
			}
		}
	case "user":
		var blocks []block
		json.Unmarshal(l.Message.Content, &blocks)
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			s, ok := t.tools[b.ToolUseID]
			if !ok {
				continue
			}
			delete(t.tools, b.ToolUseID)
			s.end = now.UnixNano()
			s.attrs = append(s.attrs, str("output.value", clip(resultText(b.Content))))
			if b.IsError {
				s.failed, s.message = true, "the tool reported an error"
			}
			t.done = append(t.done, *s)
		}
	case "result":
		t.result = l.Result
		t.usage = l.Usage
		t.cost = l.Cost
		if l.SessionID != "" {
			t.session = l.SessionID
		}
	}
}

// End finishes the turn and sends what is left. err is how the turn
// ended; nil is a turn that succeeded.
func (t *Turn) End(err error) {
	if t == nil || t.ended {
		return
	}
	t.ended = true
	now := t.now()
	for _, s := range t.tools {
		s.end = now.UnixNano()
		s.failed, s.message = true, "the turn ended before the tool returned"
		t.done = append(t.done, *s)
	}
	t.tools = nil
	r := t.root
	r.end = now.UnixNano()
	if t.result != "" {
		r.attrs = append(r.attrs, str("output.value", clip(t.result)))
	}
	if t.model != "" {
		r.attrs = append(r.attrs, str("llm.model_name", t.model))
	}
	if t.session != "" {
		r.attrs = append(r.attrs, str("maestro.claude_session", t.session))
	}
	if t.usage != nil {
		r.attrs = append(r.attrs, t.usage.attrs()...)
	}
	if t.cost > 0 {
		r.attrs = append(r.attrs, str("maestro.cost_usd", fmt.Sprintf("%.4f", t.cost)))
	}
	if err != nil {
		r.failed, r.message = true, clipName(err.Error())
	}
	t.done = append(t.done, r)
	t.flush(now)
}

func (t *Turn) flush(now time.Time) {
	t.flushed = now
	if len(t.done) == 0 {
		return
	}
	t.e.send(t.ep, t.done)
	t.done = nil
}

func (t *Turn) child(name string, start, end time.Time, kind string) span {
	return span{traceID: t.root.traceID, spanID: newID8(), parentID: t.root.spanID, name: name,
		start: start.UnixNano(), end: end.UnixNano(), attrs: []kv{str("openinference.span.kind", kind)}}
}

// resultText is a tool result's content: a string, or text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
				b.WriteString("\n")
			} else if p.Type != "" {
				fmt.Fprintf(&b, "[%s]\n", p.Type)
			}
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	return string(raw)
}

// clip cuts s to MaxValue bytes of valid text.
func clip(s string) string {
	if len(s) > MaxValue {
		s = strings.ToValidUTF8(s[:MaxValue], "") + "\n[cut]"
	}
	return strings.ToValidUTF8(s, "?")
}

func clipName(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.ToValidUTF8(s, "")
}

func newID16() (b [16]byte) { rand.Read(b[:]); return }
func newID8() (b [8]byte)   { rand.Read(b[:]); return }
