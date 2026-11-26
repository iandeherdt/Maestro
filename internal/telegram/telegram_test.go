package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "123456:ABCdefGhIJKlmnOPQrstUVwxyz_0123456"

// fakeAPI is a Bot API: it hands out the queued updates once and records
// what was sent.
type fakeAPI struct {
	mu      sync.Mutex
	updates []Update
	sent    []map[string]any
	srv     *httptest.Server
	status  int
}

func newAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`))
			return
		}
		var result any = true
		switch method {
		case "getMe":
			result = User{ID: 123456, IsBot: true, Username: "maestro_bot"}
		case "getUpdates":
			off := int64(p["offset"].(float64))
			var out []Update
			for _, u := range f.updates {
				if u.UpdateID >= off {
					out = append(out, u)
				}
			}
			if len(out) == 0 {
				f.mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				f.mu.Lock()
			}
			result = out
		case "sendMessage":
			f.sent = append(f.sent, p)
			result = Message{MessageID: int64(len(f.sent) + 1000)}
		}
		raw, _ := json.Marshal(result)
		w.Write([]byte(`{"ok":true,"result":` + string(raw) + `}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client() *Client { return &Client{Token: token, BaseURL: f.srv.URL} }

func (f *fakeAPI) queue(ms ...*Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range ms {
		f.updates = append(f.updates, Update{UpdateID: int64(len(f.updates) + 1), Message: m})
	}
}

func (f *fakeAPI) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		out = append(out, s["text"].(string))
	}
	return out
}

func (f *fakeAPI) waitSent(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.texts(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d message(s) sent, want %d: %q", len(f.texts()), n, f.texts())
	return nil
}

const owner = 42

var botUser = &User{ID: 123456, IsBot: true}

func fromOwner(text string) *Message {
	return &Message{MessageID: 7, From: &User{ID: owner, FirstName: "Ian"}, Chat: Chat{ID: owner, Type: "private"}, Text: text}
}

type fakeTalker struct {
	mu    sync.Mutex
	calls []string
	block chan struct{}
}

func (f *fakeTalker) Talk(ctx context.Context, ticket, text string, fresh bool) (string, string, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.calls = append(f.calls, ticket+"|"+text+"|"+map[bool]string{true: "new", false: "resume"}[fresh])
	f.mu.Unlock()
	if ticket == "99" {
		return "", "", errors.New("99 has no workspace on this machine")
	}
	return "Blue, as you said.", strings.ToUpper(ticket), nil
}

func (f *fakeTalker) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func startBot(t *testing.T, f *fakeAPI, talk Talker) *Bot {
	t.Helper()
	b := &Bot{Client: f.client(), Link: Link{ChatID: owner, UserID: owner}, Talk: talk, Status: func() string { return "No agent works now." }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	return b
}

func TestNoticesCarryTheTicket(t *testing.T) {
	f := newAPI(t)
	b := startBot(t, f, nil)
	b.Notify("GH-12", "The planning agent has questions:\n\nWhich colour?")
	got := f.waitSent(t, 1)
	if got[0] != "GH-12 · The planning agent has questions:\n\nWhich colour?" {
		t.Fatalf("%q", got[0])
	}
	f.mu.Lock()
	chat := f.sent[0]["chat_id"].(float64)
	_, markup := f.sent[0]["parse_mode"]
	f.mu.Unlock()
	if chat != owner || markup {
		t.Fatalf("sent to %v, parse mode %v", chat, markup)
	}
}

func TestStrangersAreIgnored(t *testing.T) {
	f := newAPI(t)
	talk := &fakeTalker{}
	// Another person, the owner in a group, and a bot: no answer at all.
	f.queue(
		&Message{MessageID: 1, From: &User{ID: 7}, Chat: Chat{ID: 7, Type: "private"}, Text: "/talk GH-1 rm -rf"},
		&Message{MessageID: 2, From: &User{ID: owner}, Chat: Chat{ID: -100, Type: "group"}, Text: "/talk GH-1 hi"},
		&Message{MessageID: 3, From: &User{ID: owner, IsBot: true}, Chat: Chat{ID: owner, Type: "private"}, Text: "/status"},
		fromOwner("/status"),
	)
	startBot(t, f, talk)
	f.waitSent(t, 1)
	time.Sleep(100 * time.Millisecond)
	if got := f.texts(); len(got) != 1 || got[0] != "No agent works now." || len(talk.got()) != 0 {
		t.Fatalf("sent %q, talks %q", got, talk.got())
	}
}

func TestAReplyTalksToTheTicketsAgent(t *testing.T) {
	f := newAPI(t)
	talk := &fakeTalker{}
	reply := fromOwner("Make it blue")
	reply.ReplyTo = &Message{MessageID: 5, From: botUser, Text: "GH-12 · The planning agent has questions:\n\nWhich colour?"}
	f.queue(reply)
	startBot(t, f, talk)
	got := f.waitSent(t, 1)
	if got[0] != "GH-12 · Blue, as you said." {
		t.Fatalf("%q", got)
	}
	if c := talk.got(); len(c) != 1 || c[0] != "GH-12|Make it blue|resume" {
		t.Fatalf("talks %q", c)
	}
	f.mu.Lock()
	rp := f.sent[0]["reply_parameters"].(map[string]any)["message_id"].(float64)
	f.mu.Unlock()
	if rp != 7 {
		t.Fatalf("the answer replies to %v", rp)
	}
}

func TestAReplyToSomeoneElsesMessageNamesNoTicket(t *testing.T) {
	f := newAPI(t)
	talk := &fakeTalker{}
	m := fromOwner("hello")
	m.ReplyTo = &Message{MessageID: 5, From: &User{ID: owner}, Text: "GH-12 · forged"}
	f.queue(m)
	startBot(t, f, talk)
	got := f.waitSent(t, 1)
	if !strings.Contains(got[0], "Which ticket") || len(talk.got()) != 0 {
		t.Fatalf("%q %q", got, talk.got())
	}
}

func TestCommandsAndTheCurrentTicket(t *testing.T) {
	f := newAPI(t)
	talk := &fakeTalker{}
	f.queue(fromOwner("/talk gh-3 what is left?"))
	startBot(t, f, talk)
	f.waitSent(t, 1)
	// A message without a command goes to the ticket talked to last.
	f.queue(fromOwner("and after that?"))
	f.waitSent(t, 2)
	f.queue(fromOwner("/new GH-4 start over"))
	f.waitSent(t, 3)
	f.queue(fromOwner("/talk 99 hi"))
	got := f.waitSent(t, 4)
	want := []string{"gh-3|what is left?|resume", "GH-3|and after that?|resume", "GH-4|start over|new", "99|hi|resume"}
	if c := talk.got(); strings.Join(c, ",") != strings.Join(want, ",") {
		t.Fatalf("talks %q", c)
	}
	if got[3] != "#99 · 99 has no workspace on this machine" {
		t.Fatalf("%q", got[3])
	}
	f.queue(fromOwner("/talk GH-1"), fromOwner("/frobnicate"))
	got = f.waitSent(t, 6)
	if !strings.Contains(got[4], "Write /talk TICKET") || !strings.Contains(got[5], "I don't know /frobnicate") {
		t.Fatalf("%q", got[4:])
	}
}

func TestOneTalkAtATime(t *testing.T) {
	f := newAPI(t)
	talk := &fakeTalker{block: make(chan struct{})}
	f.queue(fromOwner("/talk GH-1 first"), fromOwner("/talk GH-1 second"))
	startBot(t, f, talk)
	got := f.waitSent(t, 1)
	if !strings.Contains(got[0], "still answering") {
		t.Fatalf("%q", got)
	}
	close(talk.block)
	f.waitSent(t, 2)
	if c := talk.got(); len(c) != 1 || c[0] != "GH-1|first|resume" {
		t.Fatalf("talks %q", c)
	}
}

func TestLongAnswersAreSplitAndEachPartNamesTheTicket(t *testing.T) {
	f := newAPI(t)
	b := startBot(t, f, nil)
	b.Notify("GH-1", strings.Repeat("line of text\n", 700))
	got := f.waitSent(t, 3)
	for _, g := range got {
		if !strings.HasPrefix(g, "GH-1 · ") || len([]rune(g)) > 4096 {
			t.Fatalf("part of %d: %q", len(g), g[:20])
		}
	}
}

func TestErrorsDoNotHoldTheToken(t *testing.T) {
	c := &Client{Token: token, BaseURL: "http://127.0.0.1:1"}
	_, err := c.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "ABCdef") {
		t.Fatalf("%v", err)
	}
	f := newAPI(t)
	_, err = (&Client{Token: "1:" + strings.Repeat("x", 30), BaseURL: f.srv.URL}).GetMe(context.Background())
	if !Unauthorized(err) {
		t.Fatalf("%v", err)
	}
}

func TestConflictIsRecognised(t *testing.T) {
	f := newAPI(t)
	f.status = http.StatusConflict
	_, err := f.client().GetUpdates(context.Background(), 0, 0)
	if !Conflict(err) {
		t.Fatalf("%v", err)
	}
}

func TestLinkingTakesOnlyThePrivateChatThatSendsTheCode(t *testing.T) {
	f := newAPI(t)
	f.queue(
		&Message{MessageID: 1, From: &User{ID: 9}, Chat: Chat{ID: -5, Type: "group"}, Text: "123456"},
		&Message{MessageID: 2, From: &User{ID: 8}, Chat: Chat{ID: 8, Type: "private"}, Text: "000000"},
		&Message{MessageID: 3, From: &User{ID: owner, Username: "ian"}, Chat: Chat{ID: owner, Type: "private"}, Text: "/start 123456"},
	)
	l, err := WaitForCode(context.Background(), f.client(), "123456", time.Now().Add(time.Minute))
	if err != nil || l.ChatID != owner || l.UserID != owner || l.Name != "@ian" {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestLinkingStopsAfterWrongCodes(t *testing.T) {
	f := newAPI(t)
	for i := 0; i < 6; i++ {
		f.queue(&Message{MessageID: int64(i), From: &User{ID: 8}, Chat: Chat{ID: 8, Type: "private"}, Text: "111111"})
	}
	if _, err := WaitForCode(context.Background(), f.client(), "123456", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("linked after wrong codes")
	}
}

func TestTokenAndLinkFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteToken(dir, "not a token"); err == nil {
		t.Fatal("stored a bad token")
	}
	if err := WriteLink(dir, Link{ChatID: -100, UserID: 3}); err == nil {
		t.Fatal("linked a group")
	}
	if err := WriteLink(dir, Link{ChatID: owner, UserID: owner}); err != nil {
		t.Fatal(err)
	}
	if err := WriteToken(dir, token); err != nil {
		t.Fatal(err)
	}
	// A new token is a new bot: the old link goes.
	if _, ok, _ := ReadLink(dir); ok {
		t.Fatal("the link outlived the token")
	}
	fi, err := os.Stat(TokenFile(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	if got, _ := ReadToken(dir); got != token {
		t.Fatalf("%q", got)
	}
	WriteLink(dir, Link{ChatID: owner, UserID: owner})
	os.WriteFile(filepath.Join(dir, "telegram.json"), []byte(`{"chat_id":-1,"user_id":5}`), 0o600)
	if _, _, err := ReadLink(dir); err == nil {
		t.Fatal("a damaged link was read")
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadToken(dir); got != "" {
		t.Fatal("token kept")
	}
}

func TestSplit(t *testing.T) {
	if got := Split("short", 10); len(got) != 1 || got[0] != "short" {
		t.Fatalf("%q", got)
	}
	got := Split("aaaa\nbbbb\ncccc", 10)
	if len(got) != 2 || got[0] != "aaaa\nbbbb" || got[1] != "cccc" {
		t.Fatalf("%q", got)
	}
	if got := Split(strings.Repeat("é", 25), 10); len(got) != 3 || len([]rune(got[0])) != 10 {
		t.Fatalf("%q", got)
	}
}
