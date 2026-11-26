package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/claude"
	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/telegram"
)

const telegramUsage = `usage:
  maestro telegram          whether a bot is set up, and which chat it answers
  maestro telegram token    store the token of the bot that @BotFather gave you
  maestro telegram link     link your own private chat with the bot
  maestro telegram test     send a test message to the linked chat
  maestro telegram remove   delete the token and the link

The service tells you in Telegram when an idea has questions or a plan, a
design or pull request is ready, a ticket is blocked, and when work is
merged. Reply to one of its messages to talk to the agent of that ticket.
The bot answers the linked chat only. Its token stays in the setup folder,
readable by you only, and never reaches the agent.`

// telegramBase is the Bot API; tests point it elsewhere.
var telegramBase = telegram.DefaultBaseURL

// linkTimeout is how long `maestro telegram link` waits for the code.
var linkTimeout = 10 * time.Minute

// runTelegram is `maestro telegram`.
func runTelegram(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, _, rest, ok := parseWords("telegram", args, stderr, true)
	if !ok {
		return 2
	}
	if dir == "" {
		found, err := configDir()
		if err != nil {
			return report(stderr, explain("no configuration directory was found for this user, pass --dir: %v", err))
		}
		dir = found
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return report(stderr, err)
	}
	ctx, stop := interruptible()
	defer stop()
	word := ""
	if len(rest) == 1 {
		word = rest[0]
	} else if len(rest) > 1 {
		fmt.Fprintln(stderr, telegramUsage)
		return 2
	}
	client := func() (*telegram.Client, telegram.User, error) {
		token, err := telegram.ReadToken(dir)
		if err != nil {
			return nil, telegram.User{}, err
		}
		if token == "" {
			return nil, telegram.User{}, explain("No Telegram bot is set up. Make one with @BotFather in Telegram (/newbot), then run %s.",
				mention("maestro telegram token"))
		}
		c := &telegram.Client{Token: token, BaseURL: telegramBase}
		me, err := c.GetMe(ctx)
		if err != nil {
			return nil, me, explain("Telegram did not accept the bot token: %v\nStore a new one with: %s", err, mention("maestro telegram token"))
		}
		return c, me, nil
	}
	switch word {
	case "":
		token, err := telegram.ReadToken(dir)
		if err != nil {
			return report(stderr, err)
		}
		if token == "" {
			fmt.Fprintln(stdout, "No Telegram bot is set up. Make one with @BotFather in Telegram (/newbot), then run "+mention("maestro telegram token")+".")
			return 0
		}
		_, me, err := client()
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "The bot is @%s.\n", plain(me.Username, 64))
		l, ok, err := telegram.ReadLink(dir)
		switch {
		case err != nil:
			return report(stderr, err)
		case !ok:
			fmt.Fprintln(stdout, "No chat is linked yet. Link yours with: "+mention("maestro telegram link"))
		default:
			fmt.Fprintf(stdout, "It answers one chat: yours with %s (id %d), linked %s.\n", plain(l.Name, 64), l.ChatID, l.At)
		}
		return 0
	case "remove":
		if err := telegram.Remove(dir); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "The bot token and the linked chat are removed from this machine. Restart the service to stop the bot.")
		fmt.Fprintln(stdout, "To make the token useless, revoke it with @BotFather (/revoke).")
		return 0
	case "token":
		r := bufio.NewReader(stdin)
		token, err := askSecret(r, stdin, stdout, "Bot token from @BotFather (it is not shown): ")
		if err != nil {
			return report(stderr, err)
		}
		token = strings.TrimSpace(token)
		if telegram.ValidToken(token) != nil {
			fmt.Fprintln(stderr, "That is not a bot token. It looks like 123456789:AA... Nothing was stored.")
			return 1
		}
		c := &telegram.Client{Token: token, BaseURL: telegramBase}
		me, err := c.GetMe(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "Nothing was stored.")
			return report(stderr, explain("Telegram did not accept the token: %v", err))
		}
		if err := telegram.WriteToken(dir, token); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "Stored. The bot is @%s. Now link your chat with it: %s\n", plain(me.Username, 64), mention("maestro telegram link"))
		return 0
	case "link":
		c, me, err := client()
		if err != nil {
			return report(stderr, err)
		}
		code, err := telegram.NewCode()
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "Open https://t.me/%s?start=%s on your phone and press Start,\n", plain(me.Username, 64), code)
		fmt.Fprintf(stdout, "or send the bot @%s this code: %s\n", plain(me.Username, 64), code)
		fmt.Fprintf(stdout, "Waiting up to %d minutes. Only the chat that sends the code is linked; the bot ignores every other.\n", int(linkTimeout/time.Minute))
		l, err := telegram.WaitForCode(ctx, c, code, time.Now().Add(linkTimeout))
		if err != nil {
			if telegram.Conflict(err) {
				return report(stderr, explain("the running service already reads the bot's messages. Stop it (maestro service uninstall, or Ctrl-C\n"+
					"on maestro run), link the chat, then start it again."))
			}
			return report(stderr, err)
		}
		if err := telegram.WriteLink(dir, l); err != nil {
			return report(stderr, err)
		}
		c.SendMessage(ctx, l.ChatID, "Linked. Maestro on this machine tells you here when an idea or a ticket needs you, and you can reply to talk to its agent. Send /help to see more.", 0)
		fmt.Fprintf(stdout, "Linked to %s. Restart the service to start the bot: %s\n", plain(l.Name, 64), mention("maestro restart"))
		return 0
	case "test":
		c, _, err := client()
		if err != nil {
			return report(stderr, err)
		}
		l, ok, err := telegram.ReadLink(dir)
		if err != nil {
			return report(stderr, err)
		}
		if !ok {
			return report(stderr, explain("no chat is linked yet. Link yours with: %s", mention("maestro telegram link")))
		}
		if _, err := c.SendMessage(ctx, l.ChatID, "A test message from Maestro.", 0); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "Sent. Look in your chat with the bot.")
		return 0
	}
	fmt.Fprintln(stderr, telegramUsage)
	return 2
}

// telegramBot is the chat the service uses, or nil when none is set up.
// The token is returned too: the secret scan looks for it in the agent's
// work.
func (in *install) telegramBot(ctx context.Context, log *slog.Logger) (*telegram.Bot, string) {
	token, err := telegram.ReadToken(in.dir)
	if err != nil {
		log.Warn("the Telegram bot token could not be read; no Telegram", "error", err)
		return nil, ""
	}
	if token == "" {
		return nil, ""
	}
	l, ok, err := telegram.ReadLink(in.dir)
	if err != nil || !ok {
		log.Warn("a Telegram bot is set up but no chat is linked; link one with: maestro telegram link", "error", err)
		return nil, token
	}
	c := &telegram.Client{Token: token, BaseURL: telegramBase}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	me, err := c.GetMe(cctx)
	switch {
	case telegram.Unauthorized(err):
		log.Error("Telegram refused the bot token; no Telegram. Store a new one with: maestro telegram token")
		return nil, token
	case err != nil:
		// Telegram may come back; the bot keeps trying.
		log.Warn("Telegram not reached at start, will keep trying", "error", err)
	default:
		log.Info("Telegram bot", "bot", "@"+me.Username, "chat", l.Name)
	}
	return &telegram.Bot{Client: c, Link: l, Project: in.cfg.Workspace.Repo, Log: log}, token
}

// telegramStatus is the answer to /status.
func telegramStatus(orch *orchestrator.Orchestrator) string {
	s := orch.Latest()
	var b strings.Builder
	if s.Paused {
		fmt.Fprintf(&b, "Paused: %s\n\n", s.PauseReason)
	}
	if len(s.Running) == 0 {
		b.WriteString("No agent works now.")
	} else {
		b.WriteString("Working now:")
		for _, r := range s.Running {
			fmt.Fprintf(&b, "\n%s (%s), %s, turn %d", r.Identifier, r.State, time.Since(r.StartedAt).Round(time.Minute), r.TurnCount)
		}
	}
	if len(s.AwaitingChecks) > 0 {
		b.WriteString("\n\nWaiting for checks:")
		for _, a := range s.AwaitingChecks {
			fmt.Fprintf(&b, "\n%s, pull request #%d", a.Identifier, a.PRNumber)
		}
	}
	if len(s.Retrying) > 0 {
		b.WriteString("\n\nTo be tried again:")
		for _, r := range s.Retrying {
			fmt.Fprintf(&b, "\n%s at %s", r.Identifier, r.DueAt.Local().Format("15:04"))
		}
	}
	if s.DailyLimit != "" {
		fmt.Fprintf(&b, "\n\nNo new work today: %s", s.DailyLimit)
	}
	return b.String()
}

// chatTalker runs the owner's messages from the chat as turns of Claude
// Code in a ticket's sandbox. It is `maestro talk` one message at a time:
// the same view, login, network and tools as the agent, and the
// conversation is kept so that the next run on the ticket continues it.
type chatTalker struct {
	in   *install
	orch *orchestrator.Orchestrator
	// Timeout bounds one turn.
	Timeout time.Duration
}

func (t *chatTalker) Talk(ctx context.Context, typed, text string, fresh bool) (string, string, error) {
	in := t.in
	if in.dbox == nil {
		return "", "", errors.New("talking to the agent works with the container sandbox only")
	}
	keys, err := in.dbox.Workspaces(ctx)
	if err != nil {
		return "", "", err
	}
	key, err := findWorkspace(keys, typed)
	if err != nil {
		return "", "", errors.New(errText(err))
	}
	running, err := in.dbox.Running(ctx, key)
	if err != nil {
		return "", key, err
	}
	busy := len(running) > 0
	for _, r := range t.orch.Latest().Running {
		if k, err := issue.WorkspaceKey(r.Identifier); err == nil && k == key {
			busy = true
		}
	}
	if busy {
		return "", key, errors.New("an agent works on this ticket now. Talk to it when its run has ended, so that the two of you do not change the workspace at the same time")
	}
	talks := in.conversations()
	id := ""
	if !fresh {
		id = talks.Last(key, filepath.Join(in.auditDir, key+".jsonl"))
	}
	resume := id != ""
	if !resume {
		if id, err = claude.NewSessionID(); err != nil {
			return "", key, err
		}
	}
	argv, err := claude.InteractiveArgs(in.cfg.Claude, id, resume)
	if err != nil {
		return "", key, err
	}
	argv = append(argv, "--print", "--output-format", "json")
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errOut bytes.Buffer
	code, err := in.dbox.Batch(tctx, dockerbox.Session{Key: key, View: dockerbox.ViewAgent, Argv: argv, Env: in.cfg.Sandbox.Env},
		strings.NewReader(text), &limited{w: &out, n: 4 << 20}, &limited{w: &errOut, n: 64 << 10})
	if tctx.Err() == context.DeadlineExceeded {
		return "", key, fmt.Errorf("the agent took longer than %d minutes and was stopped", int(timeout/time.Minute))
	}
	if err != nil {
		return "", key, errors.New(errText(err))
	}
	res, ok := parseResult(out.Bytes())
	if !ok {
		in := strings.TrimSpace(errOut.String())
		if r := []rune(in); len(r) > 500 {
			in = string(r[len(r)-500:])
		}
		return "", key, fmt.Errorf("the agent ended (status %d) without an answer. %s", code, in)
	}
	sid := res.SessionID
	if !claude.ValidSessionID(sid) {
		sid = id
	}
	if err := talks.Save(key, sid); err != nil {
		return "", key, err
	}
	if res.IsError {
		return "", key, fmt.Errorf("the agent reported an error: %s", res.Result)
	}
	return res.Result, key, nil
}

type turnResult struct {
	Type      string `json:"type"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

// parseResult reads the result of `claude --print --output-format json`.
func parseResult(raw []byte) (turnResult, bool) {
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var r turnResult
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &r) == nil && r.Type == "result" {
			return r, true
		}
	}
	return turnResult{}, false
}

// limited keeps at most n bytes and drops the rest.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	l.w.Write(q)
	return len(p), nil
}
