package setup

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// How the wizard looks. In a terminal it draws: colour, a rail down the
// side of each step, a spinner while it waits, single keys for yes and no,
// arrow keys for a choice. Anywhere else (a pipe, a file, a terminal that
// asked for no colour) it prints the same words as plain lines, which is
// also what the tests read.

const (
	reset     = "\x1b[0m"
	bold      = "\x1b[1m"
	dim       = "\x1b[2m"
	underline = "\x1b[4m"
	red       = "\x1b[31m"
	green     = "\x1b[32m"
	yellow    = "\x1b[33m"
	cyan      = "\x1b[36m"

	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	clearLine  = "\r\x1b[2K"
)

const rail = "  " + dim + "│" + reset + "  "

// Terminal makes a UI for the process's own input and output. It draws
// when both are a terminal and nothing asks for plain text.
func Terminal(in io.Reader, out io.Writer) *UI {
	u := NewUI(in, out)
	fin, okIn := in.(*os.File)
	fout, okOut := out.(*os.File)
	if !okIn || !okOut || !term.IsTerminal(int(fin.Fd())) || !term.IsTerminal(int(fout.Fd())) {
		return u
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || os.Getenv("MAESTRO_PLAIN") != "" {
		return u
	}
	u.Styled = true
	u.Width = func() int {
		if w, _, err := term.GetSize(int(fout.Fd())); err == nil {
			return w
		}
		return 80
	}
	u.Raw = func() (func(), error) {
		old, err := term.MakeRaw(int(fin.Fd()))
		if err != nil {
			return nil, err
		}
		return func() { term.Restore(int(fin.Fd()), old) }, nil
	}
	return u
}

func (u *UI) width() int {
	w := 80
	if u.Width != nil {
		w = u.Width()
	}
	return min(max(w, 40), 100)
}

var (
	urlRE  = regexp.MustCompile(`https?://[^\s<>"]*[^\s<>".,;:)]`)
	tickRE = regexp.MustCompile("`([^`]+)`")
)

// paint gives addresses and commands their colour.
func paint(s string) string {
	s = tickRE.ReplaceAllString(s, bold+"$1"+reset)
	return urlRE.ReplaceAllString(s, cyan+underline+"$0"+reset)
}

// wrap breaks text at spaces so that no line is wider than width. A word
// that is too long, such as an address, stays whole.
func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	return append(lines, line)
}

// say draws one line of a step: on the rail, wrapped, with what is meant
// to be typed or opened set apart.
func (u *UI) say(text string) {
	body := strings.TrimLeft(text, " ")
	indent := len(text) - len(body)
	if !u.inStep {
		for _, l := range wrap(body, u.width()-4) {
			fmt.Fprintln(u.Out, "  "+paint(l))
		}
		return
	}
	hang := u.hang
	u.hang = 0
	switch {
	case body == "":
		fmt.Fprintln(u.Out, strings.TrimRight(rail, " "))
	case hang > 0 && indent == hang:
		// The second line of a list item.
		u.hang = hang
		pad := strings.Repeat(" ", indent-2)
		for _, l := range wrap(body, u.width()-5-len(pad)) {
			fmt.Fprintln(u.Out, rail+pad+paint(l))
		}
	case indent >= 6:
		// Something to copy: never wrapped, never touched.
		fmt.Fprintln(u.Out, rail+"  "+cyan+body+reset)
	default:
		pad := strings.Repeat(" ", max(indent-2, 0))
		bullet := ""
		if strings.HasPrefix(body, "- ") {
			body, bullet = body[2:], dim+"•"+reset+" "
			u.hang = indent + 2
		}
		room := u.width() - 5 - len(pad)
		if bullet != "" {
			room -= 2
		}
		for i, l := range wrap(body, room) {
			lead := bullet
			if i > 0 && bullet != "" {
				lead = "  "
			}
			fmt.Fprintln(u.Out, rail+pad+lead+paint(l))
		}
	}
}

// Show prints a line exactly as it is: a line of a file, a row of a
// table. Nothing in it is wrapped, joined or coloured by its content.
func (u *UI) Show(format string, a ...any) {
	if !u.Styled {
		fmt.Fprintf(u.Out, format+"\n", a...)
		return
	}
	u.hang = 0
	text := fmt.Sprintf(format, a...)
	if strings.HasPrefix(text, "    ") {
		text = text[2:]
	}
	fmt.Fprintln(u.Out, strings.TrimRight(rail+dim+text+reset, " "))
}

// Restore puts the terminal back as a stopped wizard should leave it.
func (u *UI) Restore() {
	if u.Styled {
		fmt.Fprint(u.Out, clearLine+showCursor)
	}
}

// Banner opens the wizard.
func (u *UI) Banner(name, plain string, lines ...string) {
	if !u.Styled {
		u.Say("%s", plain)
		for _, l := range lines {
			u.Say("%s", l)
		}
		return
	}
	fmt.Fprintf(u.Out, "\n  %s%s♪ %s%s\n", bold, cyan, name, reset)
	for _, l := range lines {
		for _, w := range wrap(l, u.width()-4) {
			fmt.Fprintln(u.Out, "  "+dim+w+reset)
		}
	}
}

// Headline is a line that stands out, outside the steps.
func (u *UI) Headline(ok bool, text string) {
	if !u.Styled {
		u.Say("%s", text)
		return
	}
	mark := green + "✔" + reset
	if !ok {
		mark = yellow + "▲" + reset
	}
	lines := wrap(text, u.width()-7)
	fmt.Fprintf(u.Out, "\n  %s  %s%s%s\n", mark, bold, lines[0], reset)
	for _, l := range lines[1:] {
		fmt.Fprintf(u.Out, "     %s%s%s\n", bold, l, reset)
	}
}

// Commands lists commands with what each does.
func (u *UI) Commands(rows [][2]string) {
	w := 0
	for _, r := range rows {
		w = max(w, len(r[0]))
	}
	for i, r := range rows {
		if !u.Styled {
			u.Say("  %-*s    %s", w, r[0], r[1])
			continue
		}
		fmt.Fprintf(u.Out, "     %s%d%s  %s%-*s%s  %s%s%s\n", dim, i+1, reset, cyan+bold, w, r[0], reset, dim, r[1], reset)
	}
}

func pips(i, n int) string {
	return green + strings.Repeat("●", i) + reset + dim + strings.Repeat("○", n-i) + reset
}

// stepHead draws the title of a step that is about to run.
func (u *UI) stepHead(i, n int, title string) {
	u.inStep = true
	fmt.Fprintf(u.Out, "\n  %s◆%s  %s%s%s  %s  %sstep %d of %d%s\n", cyan, reset, bold, title, reset, pips(i-1, n), dim, i, n, reset)
}

// stepEnd closes a step with its outcome.
func (u *UI) stepEnd(colour, mark, text string) {
	u.inStep = false
	lines := wrap(text, u.width()-7)
	fmt.Fprintf(u.Out, "  %s%s%s  %s\n", colour, mark, reset, paint(lines[0]))
	for _, l := range lines[1:] {
		fmt.Fprintln(u.Out, "     "+paint(l))
	}
}

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Waiting shows that something is going on until the returned function is
// called. In plain output it is one line.
func (u *UI) Waiting(text string) (stop func()) {
	if !u.Styled {
		if text != "" {
			u.Say("  %s", text)
		}
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	label := strings.TrimSuffix(strings.TrimSpace(text), "...")
	go func() {
		defer wg.Done()
		fmt.Fprint(u.Out, hideCursor)
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			fmt.Fprintf(u.Out, "%s  %s%s%s  %s%s%s", clearLine, cyan, frames[i%len(frames)], reset, dim, label, reset)
			select {
			case <-done:
				fmt.Fprint(u.Out, clearLine+showCursor)
				return
			case <-tick.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); wg.Wait() }) }
}

// key reads one key press. Arrow keys come back as "up" and "down".
func (u *UI) key() (string, error) {
	b, err := u.In.ReadByte()
	if err != nil {
		return "", ErrAborted
	}
	switch b {
	case 3, 4: // Ctrl-C, Ctrl-D
		return "", ErrAborted
	case '\r', '\n':
		return "enter", nil
	case 0x1b:
		if u.In.Buffered() >= 2 {
			seq, _ := u.In.Peek(2)
			if seq[0] == '[' || seq[0] == 'O' {
				u.In.Discard(2)
				switch seq[1] {
				case 'A':
					return "up", nil
				case 'B':
					return "down", nil
				}
				return "", nil
			}
		}
		return "", nil
	}
	return string(rune(b)), nil
}

// question draws a question. The answer is typed after it.
func (u *UI) question(q, hint string) {
	fmt.Fprintln(u.Out, strings.TrimRight(rail, " "))
	lines := wrap(strings.TrimSpace(q), u.width()-7)
	for i, l := range lines {
		lead := "  " + cyan + "?" + reset + "  "
		if i > 0 {
			lead = "     "
		}
		fmt.Fprint(u.Out, lead+bold+l+reset)
		if i < len(lines)-1 {
			fmt.Fprintln(u.Out)
		}
	}
	if hint == "" {
		return
	}
	if utf8.RuneCountInString(lines[len(lines)-1])+utf8.RuneCountInString(hint)+9 > u.width() {
		fmt.Fprint(u.Out, "\n   ")
	}
	fmt.Fprint(u.Out, "  "+dim+hint+reset)
}

// Fail prints why the wizard stopped. A stop the operator chose is not
// shown as an error.
func (u *UI) Fail(w io.Writer, err error, aborted bool) {
	switch {
	case !u.Styled && aborted:
		fmt.Fprintln(w, err)
	case !u.Styled:
		fmt.Fprintln(w, "setup failed:", err)
	case aborted:
		fmt.Fprintf(w, "\n  %s▲%s  %s\n", yellow, reset, strings.Join(wrap(err.Error(), u.width()-7), "\n     "))
	default:
		fmt.Fprintf(w, "\n  %s✖  Setup stopped%s\n     %s\n", red, reset, strings.Join(wrap(err.Error(), u.width()-7), "\n     "))
	}
}

const prompt = "  " + cyan + "›" + reset + "  "

// confirmKey asks yes or no with one key press.
func (u *UI) confirmKey(q string) (bool, error) {
	u.question(q, "y / n")
	fmt.Fprint(u.Out, "  ")
	restore, err := u.Raw()
	if err != nil {
		return u.confirmLine()
	}
	defer restore()
	for {
		k, err := u.key()
		if err != nil {
			fmt.Fprint(u.Out, "\r\n")
			return false, err
		}
		switch strings.ToLower(k) {
		case "y":
			fmt.Fprint(u.Out, green+"yes"+reset+"\r\n")
			return true, nil
		case "n":
			fmt.Fprint(u.Out, yellow+"no"+reset+"\r\n")
			return false, nil
		}
	}
}

func (u *UI) confirmLine() (bool, error) {
	for {
		ans, err := u.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprint(u.Out, prompt)
	}
}

// Choose lets the operator pick one of the options and returns its index.
// With keys it is a list to move through; without, the options are
// numbered and the number is typed.
func (u *UI) Choose(question string, options []string) (int, error) {
	if len(options) == 0 {
		return 0, ErrAborted
	}
	if !u.Styled || u.Raw == nil {
		return u.chooseLine(question, options)
	}
	u.question(question, "arrow keys, then Enter")
	fmt.Fprintln(u.Out)
	restore, err := u.Raw()
	if err != nil {
		return u.chooseLine("", options)
	}
	defer restore()
	fmt.Fprint(u.Out, hideCursor)
	defer fmt.Fprint(u.Out, showCursor)
	at := 0
	draw := func(first bool) {
		if !first {
			fmt.Fprintf(u.Out, "\x1b[%dA", len(options))
		}
		for i, o := range options {
			if i == at {
				fmt.Fprintf(u.Out, "%s  %s❯  %s%s\r\n", clearLine, cyan, o, reset)
			} else {
				fmt.Fprintf(u.Out, "%s     %s%s%s\r\n", clearLine, dim, o, reset)
			}
		}
	}
	draw(true)
	for {
		k, err := u.key()
		if err != nil {
			return 0, err
		}
		switch k {
		case "up", "k":
			at = (at + len(options) - 1) % len(options)
		case "down", "j", "\t":
			at = (at + 1) % len(options)
		case "enter":
			// The list folds into the one line that was chosen.
			fmt.Fprintf(u.Out, "\x1b[%dA\x1b[J  %s✔%s  %s\r\n", len(options), green, reset, options[at])
			return at, nil
		default:
			if n := int(k[0] - '0'); len(k) == 1 && n >= 1 && n <= len(options) {
				at = n - 1
			}
		}
		draw(false)
	}
}

func (u *UI) chooseLine(question string, options []string) (int, error) {
	if question != "" {
		u.Say("  %s", strings.TrimSpace(question))
	}
	for i, o := range options {
		u.Say("    %d  %s", i+1, o)
	}
	for {
		ans, err := u.Ask("  Type a number", "1", nil)
		if err != nil {
			return 0, err
		}
		var n int
		if _, err := fmt.Sscanf(ans, "%d", &n); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		u.Say("  Type a number from 1 to %d.", len(options))
	}
}
