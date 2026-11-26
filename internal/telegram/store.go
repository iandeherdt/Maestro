package telegram

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TokenFile is where the bot's token is kept, readable by the operator
// only.
func TokenFile(dir string) string { return filepath.Join(dir, "telegram.token") }

// LinkFile is where the linked chat is kept.
func LinkFile(dir string) string { return filepath.Join(dir, "telegram.json") }

// Link is the one chat the bot answers: the owner's private chat with it.
type Link struct {
	ChatID int64  `json:"chat_id"`
	UserID int64  `json:"user_id"`
	Name   string `json:"name,omitempty"`
	At     string `json:"linked_at,omitempty"`
}

// Valid reports whether the link names a private chat with a person. In a
// private chat the chat's id is the person's id.
func (l Link) Valid() bool { return l.ChatID > 0 && l.UserID == l.ChatID }

// ValidToken refuses what cannot be a bot token: digits, a colon, and
// letters, digits, - and _.
func ValidToken(t string) error {
	id, secret, ok := strings.Cut(t, ":")
	if !ok || id == "" || len(secret) < 20 {
		return errors.New("that is not a bot token")
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return errors.New("that is not a bot token")
		}
	}
	for _, c := range secret {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return errors.New("that is not a bot token")
		}
	}
	return nil
}

// ReadToken is the stored token, or "" without an error when there is
// none.
func ReadToken(dir string) (string, error) {
	raw, err := os.ReadFile(TokenFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// WriteToken stores the token, readable by the operator only. A new bot
// has new chats, so the link goes.
func WriteToken(dir, token string) error {
	if err := ValidToken(token); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(TokenFile(dir), []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Remove(LinkFile(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReadLink is the linked chat; ok is false when there is none.
func ReadLink(dir string) (Link, bool, error) {
	raw, err := os.ReadFile(LinkFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	var l Link
	if err := json.Unmarshal(raw, &l); err != nil || !l.Valid() {
		return Link{}, false, fmt.Errorf("%s is damaged; link the chat again with maestro telegram link", LinkFile(dir))
	}
	return l, true, nil
}

// WriteLink stores the linked chat.
func WriteLink(dir string, l Link) error {
	if !l.Valid() {
		return errors.New("only a private chat with a person can be linked")
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(LinkFile(dir), append(raw, '\n'), 0o600)
}

// Remove deletes the token and the link.
func Remove(dir string) error {
	for _, p := range []string{TokenFile(dir), LinkFile(dir)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// NewCode is a one-time code of six digits for linking a chat.
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// codeIn reports whether a message carries the code: the code alone, or
// after /start, which is what Telegram sends for a t.me/bot?start=CODE
// link.
func codeIn(text, code string) bool {
	t := strings.TrimSpace(text)
	t = strings.TrimSpace(strings.TrimPrefix(t, "/start"))
	return t == code
}

// WaitForCode polls the bot until someone sends it the code in a private
// chat, and returns that chat. Messages without the code are skipped.
// Each failed try counts, so a guesser cannot try every code.
func WaitForCode(ctx context.Context, c *Client, code string, until time.Time) (Link, error) {
	var offset int64
	tries := 0
	for time.Now().Before(until) {
		us, err := c.GetUpdates(ctx, offset, 25*time.Second)
		if err != nil {
			return Link{}, err
		}
		for _, u := range us {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" {
				continue
			}
			if codeIn(m.Text, code) {
				// Mark the updates as read, so the service does not see
				// the code again.
				c.GetUpdates(ctx, offset, 0)
				name := m.From.FirstName
				if m.From.Username != "" {
					name = "@" + m.From.Username
				}
				return Link{ChatID: m.Chat.ID, UserID: m.From.ID, Name: name, At: time.Now().UTC().Format(time.RFC3339)}, nil
			}
			if tries++; tries >= 5 {
				c.GetUpdates(ctx, offset, 0)
				return Link{}, errors.New("five messages without the code reached the bot; start again with a new code")
			}
		}
		if err := ctx.Err(); err != nil {
			return Link{}, err
		}
	}
	return Link{}, errors.New("the code was not sent to the bot in time")
}
