// Package httpapi serves the read-only status page and JSON API (SPEC
// section 16.5).
//
// It is reachable from the local machine only, and is built so that a web
// page open in a browser on that machine cannot use it either: the listener
// binds a loopback address, requests must name a loopback host (which
// defeats DNS rebinding), no cross-origin header is ever sent, and the one
// mutating endpoint needs a custom header that a cross-site form or image
// request cannot set.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

// State is what the server needs from the orchestrator. Both methods must
// be safe to call from any goroutine.
type State interface {
	Latest() orchestrator.Snapshot
	RequestTick()
	Pause(reason string)
	Resume()
}

// LocalHeader must be present on mutating requests.
const LocalHeader = "X-Maestro-Local"

// Server is the loopback status server.
type Server struct {
	state State
	ln    net.Listener
	srv   *http.Server
}

// Listen binds 127.0.0.1 on the given port. Port 0 picks a free port.
func Listen(port int, state State) (*Server, error) {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	s := &Server{state: state, ln: ln}
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return s, nil
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve blocks until Shutdown is called.
func (s *Server) Serve() error {
	if err := s.srv.Serve(s.ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Handler returns the request handler, for tests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/api/v1/state", s.apiState)
	mux.HandleFunc("/api/v1/refresh", s.apiRefresh)
	mux.HandleFunc("/api/v1/pause", s.apiPause)
	mux.HandleFunc("/api/v1/resume", s.apiResume)
	mux.HandleFunc("/api/v1/", s.apiIssue)
	return guard(mux)
}

// guard applies the checks and headers common to every request.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		if !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden_host", "this server answers only to a loopback host name")
			return
		}
		// A browser sends Origin on cross-site requests. Nothing legitimate
		// reaches this server from another origin.
		if o := r.Header.Get("Origin"); o != "" && !loopbackOrigin(o) {
			writeError(w, http.StatusForbidden, "forbidden_origin", "cross-origin requests are not accepted")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackOrigin(origin string) bool {
	rest, ok := strings.CutPrefix(origin, "http://")
	return ok && !strings.ContainsAny(rest, "/@") && loopbackHost(rest)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use "+method)
	return false
}

// ---- JSON shapes ----------------------------------------------------------

type tokens struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Total      int64 `json:"total_tokens"`
}

func tok(u orchestrator.Usage) tokens {
	return tokens{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Total()}
}

type runningJSON struct {
	IssueID         string  `json:"issue_id"`
	Identifier      string  `json:"issue_identifier"`
	State           string  `json:"state"`
	SessionID       string  `json:"session_id"`
	Phase           string  `json:"phase"`
	Attempt         int     `json:"attempt"`
	TurnCount       int     `json:"turn_count"`
	LastEvent       string  `json:"last_event"`
	LastMessage     string  `json:"last_message"`
	StartedAt       *string `json:"started_at"`
	LastEventAt     *string `json:"last_event_at"`
	Tokens          tokens  `json:"tokens"`
	FailureAttempts int64   `json:"failure_attempts"`
}

type retryJSON struct {
	IssueID    string  `json:"issue_id"`
	Identifier string  `json:"issue_identifier"`
	Attempt    int     `json:"attempt"`
	Kind       string  `json:"kind"`
	DueAt      *string `json:"due_at"`
	Error      string  `json:"error"`
}

type publishJSON struct {
	IssueID        string   `json:"issue_id"`
	Identifier     string   `json:"issue_identifier"`
	Branch         string   `json:"branch"`
	HeadSHA        string   `json:"head_sha"`
	PRNumber       int      `json:"pr_number"`
	ChecksDeadline *string  `json:"checks_deadline"`
	Flags          []string `json:"flags"`
}

func ts(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func running(v orchestrator.RunningView) runningJSON {
	return runningJSON{v.IssueID, v.Identifier, v.State, v.SessionID, string(v.Phase), v.Attempt, v.TurnCount,
		v.LastEvent, v.LastMessage, ts(v.StartedAt), ts(v.LastEventAt), tok(v.Usage), v.FailureAttempts}
}

func retry(r orchestrator.RetryEntry) retryJSON {
	return retryJSON{r.IssueID, r.Identifier, r.Attempt, r.Kind, ts(r.DueAt), r.Error}
}

func publish(p orchestrator.PublishRecord) publishJSON {
	flags := p.Flags
	if flags == nil {
		flags = []string{}
	}
	return publishJSON{p.IssueID, p.Identifier, p.Branch, p.HeadSHA, p.PRNumber, ts(p.ChecksDeadline), flags}
}

func stateJSON(s orchestrator.Snapshot) map[string]any {
	run := make([]runningJSON, len(s.Running))
	for i, v := range s.Running {
		run[i] = running(v)
	}
	ret := make([]retryJSON, len(s.Retrying))
	for i, v := range s.Retrying {
		ret[i] = retry(v)
	}
	aw := make([]publishJSON, len(s.AwaitingChecks))
	for i, v := range s.AwaitingChecks {
		aw[i] = publish(v)
	}
	totals := map[string]any{
		"input_tokens": s.Totals.Usage.Input, "output_tokens": s.Totals.Usage.Output,
		"cache_read_tokens": s.Totals.Usage.CacheRead, "cache_write_tokens": s.Totals.Usage.CacheWrite,
		"total_tokens": s.Totals.Usage.Total(), "seconds_running": s.Totals.SecondsRunning,
	}
	var cfgErr any
	if s.ConfigError != "" {
		cfgErr = s.ConfigError
	}
	return map[string]any{
		"generated_at":    ts(s.GeneratedAt),
		"worker":          s.Worker,
		"counts":          map[string]int{"running": len(run), "retrying": len(ret), "awaiting_checks": len(aw)},
		"running":         run,
		"retrying":        ret,
		"awaiting_checks": aw,
		"agent_totals":    totals,
		"budget": map[string]any{"runs_today": s.Today.Runs, "tokens_today": s.Today.TotalTokens,
			"agent_seconds_today": s.Today.AgentSeconds},
		"paused":              s.Paused,
		"pause_reason":        s.PauseReason,
		"quota_blocked_until": ts(s.QuotaBlockedUntil),
		"config_error":        cfgErr,
	}
}

// ---- handlers -------------------------------------------------------------

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	if allow(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, stateJSON(s.state.Latest()))
	}
}

func (s *Server) apiRefresh(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	if r.Header.Get(LocalHeader) != "1" {
		writeError(w, http.StatusForbidden, "local_header_required", "send the header "+LocalHeader+": 1")
		return
	}
	s.state.RequestTick()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

// local reports whether a mutating request may proceed: a POST with the
// header a cross-site form or image request cannot set.
func local(w http.ResponseWriter, r *http.Request) bool {
	if !allow(w, r, http.MethodPost) {
		return false
	}
	if r.Header.Get(LocalHeader) != "1" {
		writeError(w, http.StatusForbidden, "local_header_required", "send the header "+LocalHeader+": 1")
		return false
	}
	return true
}

// apiPause stops new dispatches. The optional reason is shown by status.
func (s *Server) apiPause(w http.ResponseWriter, r *http.Request) {
	if !local(w, r) {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	reason := strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return ' '
		}
		return c
	}, in.Reason)
	if len(reason) > 200 {
		reason = reason[:200]
	}
	s.state.Pause(strings.TrimSpace(strings.ToValidUTF8(reason, "")))
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

func (s *Server) apiResume(w http.ResponseWriter, r *http.Request) {
	if !local(w, r) {
		return
	}
	s.state.Resume()
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

func (s *Server) apiIssue(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	snap := s.state.Latest()
	out := map[string]any{"issue_identifier": id, "running": nil, "retry": nil, "publish": nil}
	found := false
	for _, v := range snap.Running {
		if v.Identifier == id {
			out["running"], out["status"], found = running(v), "running", true
		}
	}
	for _, v := range snap.Retrying {
		if v.Identifier == id {
			out["retry"], out["status"], found = retry(v), "retrying", true
		}
	}
	for _, v := range snap.AwaitingChecks {
		if v.Identifier == id {
			out["publish"], out["status"], found = publish(v), "awaiting_checks", true
		}
	}
	if id == "" || !found {
		writeError(w, http.StatusNotFound, "issue_not_found", "no running, retrying or published issue has that identifier")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Every dynamic value goes through html/template, which escapes it for the
// place it lands in. Issue titles and agent messages are untrusted.
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Maestro {{.Worker}}</title>
<style>body{font:14px system-ui,sans-serif;margin:2rem;max-width:70rem}table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:.3rem .6rem;border-bottom:1px solid #ddd;vertical-align:top}
.warn{background:#fff3cd;padding:.5rem .8rem}h2{margin-top:2rem}</style></head><body>
<h1>Maestro worker {{.Worker}}</h1>
<p>Generated {{.Generated}}. Runs today: {{.S.Today.Runs}}. Tokens today: {{.S.Today.TotalTokens}}.</p>
{{if .S.Paused}}<p class="warn">Dispatch is paused: {{.S.PauseReason}}</p>{{end}}
{{if .Quota}}<p class="warn">Usage limit reached. Dispatch resumes after {{.Quota}}.</p>{{end}}
{{if .S.ConfigError}}<p class="warn">Workflow file problem, dispatch on hold: {{.S.ConfigError}}</p>{{end}}
<h2>Running ({{len .S.Running}})</h2>
<table><tr><th>Issue</th><th>Phase</th><th>Attempt</th><th>Turns</th><th>Tokens</th><th>Last event</th></tr>
{{range .S.Running}}<tr><td>{{.Identifier}}</td><td>{{.Phase}}</td><td>{{.Attempt}}</td><td>{{.TurnCount}}</td><td>{{.Usage.Total}}</td><td>{{.LastEvent}} {{.LastMessage}}</td></tr>
{{end}}</table>
<h2>Waiting to retry ({{len .S.Retrying}})</h2>
<table><tr><th>Issue</th><th>Kind</th><th>Attempt</th><th>Reason</th></tr>
{{range .S.Retrying}}<tr><td>{{.Identifier}}</td><td>{{.Kind}}</td><td>{{.Attempt}}</td><td>{{.Error}}</td></tr>
{{end}}</table>
<h2>Waiting for checks ({{len .S.AwaitingChecks}})</h2>
<table><tr><th>Issue</th><th>Pull request</th><th>Branch</th></tr>
{{range .S.AwaitingChecks}}<tr><td>{{.Identifier}}</td><td>#{{.PRNumber}}</td><td>{{.Branch}}</td></tr>
{{end}}</table>
</body></html>
`))

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	if !allow(w, r, http.MethodGet) {
		return
	}
	snap := s.state.Latest()
	data := struct {
		S                        orchestrator.Snapshot
		Worker, Generated, Quota string
	}{S: snap, Worker: snap.Worker, Generated: snap.GeneratedAt.UTC().Format(time.RFC3339)}
	if snap.QuotaBlockedUntil.After(snap.GeneratedAt) {
		data.Quota = snap.QuotaBlockedUntil.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, data); err != nil {
		fmt.Fprint(w, "\n<!-- render error -->")
	}
}
