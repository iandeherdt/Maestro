package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/budget"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

type fakeState struct {
	snap  orchestrator.Snapshot
	ticks int
	calls []string
}

func (f *fakeState) Pause(reason string) { f.calls = append(f.calls, "pause:"+reason) }
func (f *fakeState) Resume()             { f.calls = append(f.calls, "resume") }

func (f *fakeState) Latest() orchestrator.Snapshot { return f.snap }
func (f *fakeState) RequestTick()                  { f.ticks++ }

const evil = `<script>alert(1)</script>"><img src=x onerror=alert(2)>`

func state() *fakeState {
	at := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	return &fakeState{snap: orchestrator.Snapshot{
		GeneratedAt: at, Worker: "linux", Today: budget.Day{Runs: 3, TotalTokens: 1500},
		Running: []orchestrator.RunningView{{IssueID: "id-1", Identifier: "GH-1", State: "In Progress",
			SessionID: "s-1", Phase: orchestrator.StreamingTurn, TurnCount: 2, StartedAt: at,
			LastEvent: "assistant", LastMessage: evil, Usage: orchestrator.Usage{Input: 10, Output: 5}}},
		Retrying: []orchestrator.RetryEntry{{IssueID: "id-2", Identifier: "GH-2", Attempt: 2,
			Kind: orchestrator.KindFailure, DueAt: at.Add(time.Minute), Error: evil}},
		AwaitingChecks: []orchestrator.PublishRecord{{IssueID: "id-3", Identifier: "GH-3",
			Branch: "maestro/GH-3", HeadSHA: "abc", PRNumber: 12}},
		Paused: true, PauseReason: evil,
	}}
}

func do(t *testing.T, st State, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	(&Server{state: st}).Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Result(), string(body)
}

func TestStateShape(t *testing.T) {
	res, body := do(t, state(), "GET", "/api/v1/state", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, type %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"generated_at", "counts", "running", "retrying", "awaiting_checks",
		"agent_totals", "budget", "paused", "quota_blocked_until", "worker", "config_error"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	run := got["running"].([]any)[0].(map[string]any)
	if run["issue_identifier"] != "GH-1" || run["phase"] != "StreamingTurn" ||
		run["tokens"].(map[string]any)["total_tokens"].(float64) != 15 {
		t.Errorf("running entry = %v", run)
	}
	if got["counts"].(map[string]any)["retrying"].(float64) != 1 || got["quota_blocked_until"] != nil {
		t.Errorf("counts or quota wrong: %v", got)
	}
}

func TestEmptyStateUsesArraysNotNull(t *testing.T) {
	_, body := do(t, &fakeState{}, "GET", "/api/v1/state", nil)
	if !strings.Contains(body, `"running": []`) || !strings.Contains(body, `"retrying": []`) {
		t.Fatalf("body = %s", body)
	}
}

func TestIssueEndpoint(t *testing.T) {
	for id, status := range map[string]string{"GH-1": "running", "GH-2": "retrying", "GH-3": "awaiting_checks"} {
		res, body := do(t, state(), "GET", "/api/v1/"+id, nil)
		if res.StatusCode != 200 || !strings.Contains(body, `"status": "`+status+`"`) {
			t.Errorf("%s: %d %s", id, res.StatusCode, body)
		}
	}
	for _, id := range []string{"GH-404", "", "GH-1/extra", "../state"} {
		res, body := do(t, state(), "GET", "/api/v1/"+id, nil)
		if id == "../state" {
			continue // cleaned by the router; covered by not panicking
		}
		if res.StatusCode != 404 || !strings.Contains(body, `"code": "issue_not_found"`) {
			t.Errorf("%q: %d %s", id, res.StatusCode, body)
		}
	}
}

func TestMethods(t *testing.T) {
	for _, c := range [][2]string{{"POST", "/api/v1/state"}, {"DELETE", "/api/v1/GH-1"}, {"GET", "/api/v1/refresh"},
		{"POST", "/"}, {"PUT", "/api/v1/refresh"}} {
		res, body := do(t, state(), c[0], c[1], map[string]string{LocalHeader: "1"})
		if res.StatusCode != 405 || res.Header.Get("Allow") == "" || !strings.Contains(body, "method_not_allowed") {
			t.Errorf("%s %s: %d %s", c[0], c[1], res.StatusCode, body)
		}
	}
	if res, _ := do(t, state(), "GET", "/nope", nil); res.StatusCode != 404 {
		t.Errorf("unknown page: %d", res.StatusCode)
	}
}

func TestRefreshNeedsLocalHeader(t *testing.T) {
	st := state()
	if res, _ := do(t, st, "POST", "/api/v1/refresh", nil); res.StatusCode != 403 || st.ticks != 0 {
		t.Fatalf("without header: %d, ticks %d", res.StatusCode, st.ticks)
	}
	if res, _ := do(t, st, "POST", "/api/v1/refresh", map[string]string{LocalHeader: "1"}); res.StatusCode != 202 || st.ticks != 1 {
		t.Fatalf("with header: %d, ticks %d", res.StatusCode, st.ticks)
	}
}

func TestPauseAndResume(t *testing.T) {
	st := state()
	for _, path := range []string{"/api/v1/pause", "/api/v1/resume"} {
		if res, _ := do(t, st, "POST", path, nil); res.StatusCode != 403 {
			t.Errorf("%s without the header: %d", path, res.StatusCode)
		}
		if res, _ := do(t, st, "GET", path, map[string]string{LocalHeader: "1"}); res.StatusCode != 405 {
			t.Errorf("GET %s: %d", path, res.StatusCode)
		}
		if res, _ := do(t, st, "POST", path, map[string]string{LocalHeader: "1", "Origin": "https://evil.example"}); res.StatusCode != 403 {
			t.Errorf("%s from another origin: %d", path, res.StatusCode)
		}
	}
	if len(st.calls) != 0 {
		t.Fatalf("refused requests had an effect: %v", st.calls)
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/v1/pause", strings.NewReader(`{"reason":"on\nholiday \u001b[31m`+strings.Repeat("x", 500)+`"}`))
	req.Header.Set(LocalHeader, "1")
	rec := httptest.NewRecorder()
	(&Server{state: st}).Handler().ServeHTTP(rec, req)
	do(t, st, "POST", "/api/v1/resume", map[string]string{LocalHeader: "1"})
	if rec.Code != 202 || len(st.calls) != 2 || st.calls[1] != "resume" {
		t.Fatalf("%d %v", rec.Code, st.calls)
	}
	if got := st.calls[0]; !strings.HasPrefix(got, "pause:on holiday  [31mxxx") || len(got) > 210 {
		t.Errorf("reason = %q", got)
	}
}

func TestBrowserAttacksAreRefused(t *testing.T) {
	st := state()
	for name, hdr := range map[string]map[string]string{
		"dns rebinding":        {"Host": "attacker.example:8080", LocalHeader: "1"},
		"lan address":          {"Host": "192.168.1.10:8080", LocalHeader: "1"},
		"cross-site origin":    {"Origin": "https://attacker.example", LocalHeader: "1"},
		"lookalike origin":     {"Origin": "http://127.0.0.1.attacker.example", LocalHeader: "1"},
		"origin with userinfo": {"Origin": "http://127.0.0.1@attacker.example", LocalHeader: "1"},
		"null origin":          {"Origin": "null", LocalHeader: "1"},
	} {
		for _, c := range [][2]string{{"POST", "/api/v1/refresh"}, {"GET", "/api/v1/state"}, {"GET", "/"}} {
			res, body := do(t, st, c[0], c[1], hdr)
			if res.StatusCode != 403 {
				t.Errorf("%s, %s %s: status %d", name, c[0], c[1], res.StatusCode)
			}
			if strings.Contains(body, "GH-1") {
				t.Errorf("%s: state leaked", name)
			}
		}
	}
	if st.ticks != 0 {
		t.Fatalf("ticks = %d", st.ticks)
	}
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "localhost"} {
		if res, _ := do(t, st, "GET", "/api/v1/state", map[string]string{"Host": host, "Origin": "http://" + host}); res.StatusCode != 200 {
			t.Errorf("host %s refused: %d", host, res.StatusCode)
		}
	}
}

func TestPageEscapesUntrustedText(t *testing.T) {
	res, body := do(t, state(), "GET", "/", nil)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img") {
		t.Fatalf("untrusted text rendered as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "GH-3") || !strings.Contains(body, "Dispatch is paused") {
		t.Fatalf("page content missing:\n%s", body)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if res.Header.Get(h) == "" {
			t.Errorf("header %s missing", h)
		}
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("a cross-origin header is sent")
	}
}

func TestListenerBindsLoopbackOnly(t *testing.T) {
	st := state()
	s, err := Listen(0, st)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	defer s.Shutdown(context.Background())
	addr := s.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("bound to %s", addr.IP)
	}
	res, err := http.Get("http://" + addr.String() + "/api/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
}
