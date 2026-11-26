package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Talker runs one turn of Claude Code in a ticket's sandbox, continuing
// the agent's conversation on it, and returns its answer. fresh starts a
// new conversation instead. ticket is what the owner typed; the answer
// names the ticket as it is known on this machine.
type Talker interface {
	Talk(ctx context.Context, ticket, text string, fresh bool) (answer, key string, err error)
}

// Bot is the service's side of the chat.
type Bot struct {
	Client *Client
	Link   Link
	// Talk is optional. Without it the chat only gets notices.
	Talk Talker
	// Status is optional: a few lines on what the service does now.
	Status func() string
	// Project names this installation in the help text.
	Project string
	Log     *slog.Logger

	once   sync.Once
	out    chan string
	talkMu sync.Mutex
	// current is the ticket plain messages go to: the one the owner talked
	// with last.
	mu      sync.Mutex
	current string
	ignored map[int64]bool
}

func (b *Bot) init() {
	b.once.Do(func() {
		b.out = make(chan string, 64)
		b.ignored = map[int64]bool{}
		if b.Log == nil {
			b.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// sep goes between the ticket and the text. Answers carry it too, so that
// a reply to any of the bot's messages reaches the right ticket.
const sep = " · "

// Notify sends a notice about a ticket. It never blocks: when Telegram
// cannot keep up, notices are dropped and logged.
func (b *Bot) Notify(ticket, text string) {
	b.init()
	msg := text
	if ticket != "" {
		msg = ticket + sep + text
	}
	select {
	case b.out <- msg:
	default:
		b.Log.Warn("Telegram notice dropped, too many waiting", "issue", ticket)
	}
}

// Run sends notices and answers the owner until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	b.init()
	go b.sendLoop(ctx)
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		us, err := b.Client.GetUpdates(ctx, offset, 50*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			switch {
			case Conflict(err):
				b.Log.Warn("another program reads this Telegram bot's messages; only one may. Is the bot used by a second project or `maestro telegram link`?")
				wait = 30 * time.Second
			case Unauthorized(err):
				b.Log.Error("Telegram refused the bot token. Store a new one with: maestro telegram token")
				wait = 10 * time.Minute
			default:
				var ae *APIError
				if errors.As(err, &ae) && ae.RetryAfter > 0 {
					wait = time.Duration(ae.RetryAfter) * time.Second
				}
				b.Log.Warn("Telegram not reached, will retry", "error", err, "in", wait)
				backoff = min(backoff*2, time.Minute)
			}
			sleep(ctx, wait)
			continue
		}
		backoff = time.Second
		for _, u := range us {
			offset = u.UpdateID + 1
			if u.Message != nil {
				b.handle(ctx, u.Message)
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (b *Bot) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-b.out:
			b.send(ctx, msg, 0)
		}
	}
}

// send sends text in as many messages as it takes. Every part after the
// first starts with the ticket too.
func (b *Bot) send(ctx context.Context, text string, replyTo int64) {
	ticket, _, _ := strings.Cut(text, sep)
	if !ticketRE.MatchString(ticket) {
		ticket = ""
	}
	for i, part := range Split(text, MaxText) {
		if i > 0 && ticket != "" {
			part = ticket + sep + part
		}
		for try := 0; ; try++ {
			_, err := b.Client.SendMessage(ctx, b.Link.ChatID, part, replyTo)
			if err == nil {
				break
			}
			var ae *APIError
			if try < 3 && ctx.Err() == nil && (!errors.As(err, &ae) || ae.Code == 429 || ae.Code >= 500) {
				wait := time.Duration(1<<try) * time.Second
				if ae != nil && ae.RetryAfter > 0 {
					wait = time.Duration(ae.RetryAfter) * time.Second
				}
				sleep(ctx, wait)
				continue
			}
			b.Log.Warn("Telegram message not sent", "error", err)
			return
		}
		replyTo = 0
	}
}

// ticketRE is an identifier such as GH-12 or ENG-7.
var ticketRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,15}-[0-9]{1,9}$`)

// typedTicketRE is what the owner may type for a ticket: GH-12, gh-12,
// 12 or #12.
var typedTicketRE = regexp.MustCompile(`^(#?[0-9]{1,9}|[A-Za-z][A-Za-z0-9]{0,15}-[0-9]{1,9})$`)

// fromOwner reports whether a message comes from the linked person in the
// linked private chat. Everything else is ignored without an answer, so
// that the bot does not even tell a stranger that it is there.
func (b *Bot) fromOwner(m *Message) bool {
	return m.From != nil && !m.From.IsBot && m.Chat.Type == "private" &&
		m.Chat.ID == b.Link.ChatID && m.From.ID == b.Link.UserID && b.Link.Valid()
}

func (b *Bot) handle(ctx context.Context, m *Message) {
	if !b.fromOwner(m) {
		var from int64
		if m.From != nil {
			from = m.From.ID
		}
		b.mu.Lock()
		first := !b.ignored[from] && len(b.ignored) < 1000
		b.ignored[from] = true
		b.mu.Unlock()
		if first {
			b.Log.Warn("Telegram message from a chat that is not linked, ignored", "chat", m.Chat.ID, "from", from)
		}
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		b.reply(ctx, m, "Only text reaches the agents.")
		return
	}
	cmd, rest := "", text
	if strings.HasPrefix(text, "/") {
		cmd, rest, _ = strings.Cut(text, " ")
		cmd, _, _ = strings.Cut(strings.ToLower(cmd), "@") // /status@MyBot in a menu
		rest = strings.TrimSpace(rest)
	}
	switch cmd {
	case "/start", "/help":
		b.reply(ctx, m, b.help())
	case "/status":
		if b.Status == nil {
			b.reply(ctx, m, "The status is not available here.")
			return
		}
		b.reply(ctx, m, b.Status())
	case "/talk", "/new":
		ticket, msg, _ := strings.Cut(rest, " ")
		msg = strings.TrimSpace(msg)
		if !typedTicketRE.MatchString(ticket) || msg == "" {
			b.reply(ctx, m, "Write "+cmd+" TICKET and your message, for example: "+cmd+" GH-12 what do you need to know?")
			return
		}
		b.talk(ctx, m, ticket, msg, cmd == "/new")
	case "":
		ticket := b.repliedTicket(m)
		if ticket == "" {
			b.mu.Lock()
			ticket = b.current
			b.mu.Unlock()
		}
		if ticket == "" {
			b.reply(ctx, m, "Which ticket is this for? Reply to one of my messages about it, or write /talk TICKET and your message.")
			return
		}
		b.talk(ctx, m, ticket, text, false)
	default:
		b.reply(ctx, m, "I don't know "+cmd+".\n\n"+b.help())
	}
}

// repliedTicket is the ticket of the bot's message that m replies to.
func (b *Bot) repliedTicket(m *Message) string {
	r := m.ReplyTo
	if r == nil || r.From == nil || !r.From.IsBot {
		return ""
	}
	t, _, ok := strings.Cut(r.Text, sep)
	if !ok || !ticketRE.MatchString(t) {
		return ""
	}
	return t
}

func (b *Bot) reply(ctx context.Context, m *Message, text string) {
	b.send(ctx, text, m.MessageID)
}

// talk runs one turn with the agent, one at a time.
func (b *Bot) talk(ctx context.Context, m *Message, ticket, text string, fresh bool) {
	if b.Talk == nil {
		b.reply(ctx, m, "Talking to the agents is not available on this machine.")
		return
	}
	if !b.talkMu.TryLock() {
		b.reply(ctx, m, "I'm still answering your previous message. Send this one again when that answer is here.")
		return
	}
	go func() {
		defer b.talkMu.Unlock()
		tctx, stop := context.WithCancel(ctx)
		defer stop()
		go func() {
			for {
				b.Client.Typing(tctx, b.Link.ChatID)
				select {
				case <-tctx.Done():
					return
				case <-time.After(4 * time.Second):
				}
			}
		}()
		ans, key, err := b.Talk.Talk(tctx, ticket, text, fresh)
		stop()
		if err != nil {
			b.reply(ctx, m, ticketLabel(key, ticket)+sep+err.Error())
			return
		}
		b.mu.Lock()
		b.current = key
		b.mu.Unlock()
		if strings.TrimSpace(ans) == "" {
			ans = "(no answer)"
		}
		b.reply(ctx, m, key+sep+ans)
	}()
}

func ticketLabel(key, typed string) string {
	if key != "" {
		return key
	}
	if ticketRE.MatchString(typed) {
		return strings.ToUpper(typed)
	}
	return "#" + strings.TrimPrefix(typed, "#")
}

func (b *Bot) help() string {
	var s strings.Builder
	if b.Project != "" {
		fmt.Fprintf(&s, "Maestro for %s.\n\n", b.Project)
	}
	s.WriteString("I tell you when an idea has questions or a plan, a design or pull request is ready, a ticket is blocked, and when work is merged.\n\n")
	if b.Talk != nil {
		s.WriteString("Reply to one of my messages to talk to the agent of that ticket. It continues the agent's own conversation, in the ticket's sandbox, with the agent's limits. The next run on the ticket takes what you agreed into account: for an idea, move its card back to planning on the board when you are done.\n\n")
		s.WriteString("/talk GH-12 text: talk to the agent of a ticket. Messages without a command go to the ticket you talked to last.\n")
		s.WriteString("/new GH-12 text: start a new conversation on a ticket.\n")
	}
	s.WriteString("/status: what runs now.")
	return s.String()
}
