package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/telegram"
)

const testBotToken = "123456:ABCdefGhIJKlmnOPQrstUVwxyz_0123456"

// fakeBotAPI accepts testBotToken, and hands out one message with the text
// that code() returns once a code was printed.
func fakeBotAPI(t *testing.T, code func() string) *[]string {
	t.Helper()
	var mu sync.Mutex
	sent := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testBotToken+"/")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 123456, "is_bot": true, "username": "maestro_bot"}
		case "getUpdates":
			result = []any{}
			if c := code(); c != "" && p["offset"].(float64) == 0 {
				result = []any{map[string]any{"update_id": 1, "message": map[string]any{
					"message_id": 1, "from": map[string]any{"id": 42, "username": "ian"},
					"chat": map[string]any{"id": 42, "type": "private"}, "text": "/start " + c}}}
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		case "sendMessage":
			mu.Lock()
			sent = append(sent, p["text"].(string))
			mu.Unlock()
			result = map[string]any{"message_id": 9}
		}
		raw, _ := json.Marshal(result)
		w.Write([]byte(`{"ok":true,"result":` + string(raw) + `}`))
	}))
	t.Cleanup(srv.Close)
	old := telegramBase
	telegramBase = srv.URL
	t.Cleanup(func() { telegramBase = old })
	return &sent
}

// codeWriter catches the code `maestro telegram link` prints.
type codeWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *codeWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *codeWriter) code() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, after, ok := strings.Cut(c.buf.String(), "this code: ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(after, "\n", 2)[0])
}

func TestTelegramTokenAndLink(t *testing.T) {
	out := &codeWriter{}
	sent := fakeBotAPI(t, out.code)
	dir := t.TempDir()
	run := func(stdin string, words ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := runTelegram(append([]string{"--dir", dir}, words...), strings.NewReader(stdin), &o, &e)
		return code, o.String(), e.String()
	}
	if code, o, _ := run(""); code != 0 || !strings.Contains(o, "No Telegram bot is set up") {
		t.Fatalf("%d %q", code, o)
	}
	if code, _, e := run("not-a-token\n", "token"); code != 1 || !strings.Contains(e, "not a bot token") {
		t.Fatalf("%d %q", code, e)
	}
	if code, _, e := run("1:"+strings.Repeat("x", 30)+"\n", "token"); code == 0 || !strings.Contains(e, "Nothing was stored") {
		t.Fatalf("%d %q", code, e)
	}
	if code, o, e := run(testBotToken+"\n", "token"); code != 0 || !strings.Contains(o, "@maestro_bot") {
		t.Fatalf("%d %q %q", code, o, e)
	}
	if code, _, e := run("", "test"); code == 0 || !strings.Contains(e, "no chat is linked") {
		t.Fatalf("%d %q", code, e)
	}
	var e bytes.Buffer
	if code := runTelegram([]string{"--dir", dir, "link"}, strings.NewReader(""), out, &e); code != 0 {
		t.Fatalf("link: %d %q %q", code, out.buf.String(), e.String())
	}
	l, ok, err := telegram.ReadLink(dir)
	if !ok || err != nil || l.ChatID != 42 || l.Name != "@ian" {
		t.Fatalf("%+v %v %v", l, ok, err)
	}
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "Linked.") {
		t.Fatalf("sent %q", *sent)
	}
	if code, o, _ := run(""); code != 0 || !strings.Contains(o, "yours with @ian") {
		t.Fatalf("%d %q", code, o)
	}
	if code, _, _ := run("", "remove"); code != 0 {
		t.Fatal(code)
	}
	if tok, _ := telegram.ReadToken(dir); tok != "" {
		t.Fatal("token kept")
	}
}

func TestParseResult(t *testing.T) {
	raw := []byte("some noise\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"Blue.\",\"session_id\":\"0b6c4a52-6f11-4c1e-9a3e-2d7f8f0d1a11\"}\n")
	r, ok := parseResult(raw)
	if !ok || r.Result != "Blue." || r.SessionID != "0b6c4a52-6f11-4c1e-9a3e-2d7f8f0d1a11" || r.IsError {
		t.Fatalf("%+v %v", r, ok)
	}
	if _, ok := parseResult([]byte(`{"type":"assistant"}`)); ok {
		t.Fatal("not a result")
	}
}
