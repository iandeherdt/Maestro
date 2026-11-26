// Package telegram lets the owner follow and talk to the agents from a
// Telegram chat. The service long-polls Telegram's Bot API, so nothing on
// this machine listens for connections. It answers one private chat only:
// the one linked with `maestro telegram link`. The bot's token stays with
// the service and never reaches the sandbox.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Telegram's Bot API.
const DefaultBaseURL = "https://api.telegram.org"

// Client calls the Bot API. It is safe for concurrent use.
type Client struct {
	Token   string
	BaseURL string // DefaultBaseURL when empty
	HTTP    *http.Client
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Chat is where a message was sent.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is a message in a chat.
type Message struct {
	MessageID int64    `json:"message_id"`
	From      *User    `json:"from"`
	Chat      Chat     `json:"chat"`
	Text      string   `json:"text"`
	ReplyTo   *Message `json:"reply_to_message"`
}

// Update is one event from getUpdates. Only messages are asked for.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// APIError is an answer of the Bot API that says ok: false.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Telegram answered %d: %s", e.Code, e.Description)
}

// Conflict reports whether another program polls the same bot.
func Conflict(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == http.StatusConflict
}

// Unauthorized reports whether Telegram refused the token.
func Unauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Code == http.StatusUnauthorized || ae.Code == http.StatusNotFound)
}

type answer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call posts params to a method and decodes its result into out. An error
// never holds the request's URL, which holds the token.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	if c.Token == "" || strings.ContainsAny(c.Token, "/?# \t\r\n") {
		return errors.New("no valid Telegram bot token")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/bot"+c.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("the Telegram request could not be made")
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("could not reach Telegram: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("the answer from Telegram could not be read: %w", err)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Errorf("the answer from Telegram to %s is not JSON (HTTP %d)", method, resp.StatusCode)
	}
	if !a.OK {
		code := a.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{Code: code, Description: a.Description, RetryAfter: a.Parameters.RetryAfter}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(a.Result, out)
}

// GetMe is the bot itself, and checks the token.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

// GetUpdates waits up to wait for new messages after offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	var us []Update
	p := map[string]any{"offset": offset, "timeout": int(wait / time.Second), "allowed_updates": []string{"message"}}
	ctx, cancel := context.WithTimeout(ctx, wait+30*time.Second)
	defer cancel()
	err := c.call(ctx, "getUpdates", p, &us)
	return us, err
}

// MaxText is how long one message may be. Telegram allows 4096
// characters; some room is left for the ticket in front.
const MaxText = 4000

// SendMessage sends plain text, without any markup, so that nothing in it
// is read as formatting or links to follow. replyTo 0 is no reply.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) (int64, error) {
	p := map[string]any{"chat_id": chatID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if replyTo != 0 {
		p["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var m Message
	if err := c.call(ctx, "sendMessage", p, &m); err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// Typing shows "typing…" in the chat for a few seconds.
func (c *Client) Typing(ctx context.Context, chatID int64) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}

// Split cuts text into messages Telegram accepts, at line ends where it
// can.
func Split(text string, max int) []string {
	var parts []string
	r := []rune(text)
	for len(r) > max {
		cut := max
		for i := max; i > max/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		parts = append(parts, strings.TrimRight(string(r[:cut]), "\n"))
		r = r[cut:]
	}
	if s := strings.TrimSpace(string(r)); s != "" || len(parts) == 0 {
		parts = append(parts, string(r))
	}
	return parts
}
