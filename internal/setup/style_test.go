package setup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// styled is a UI that draws into a buffer, with keys read from in.
func styled(in string) (*UI, *bytes.Buffer) {
	var out bytes.Buffer
	u := &UI{In: bufio.NewReader(strings.NewReader(in)), Out: &out, Styled: true,
		Width: func() int { return 60 }, Raw: func() (func(), error) { return func() {}, nil }}
	return u, &out
}

// text is what stays on the screen: a line that was cleared is gone.
func text(b *bytes.Buffer) string {
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		if at := strings.LastIndex(l, clearLine); at >= 0 {
			l = l[at+len(clearLine):]
		}
		lines[i] = strings.ReplaceAll(ansiRE.ReplaceAllString(l, ""), "\r", "")
	}
	return strings.Join(lines, "\n")
}

func TestStyledLines(t *testing.T) {
	u, out := styled("")
	u.inStep = true
	u.Say("  The board is at https://github.com/orgs/acme/projects/3. Open it and look at the columns there.")
	u.Say("    - read and write code, pull requests and issues, and more,")
	u.Say("      only in repositories you install it on,")
	u.Say("    - read boards.")
	u.Say("")
	u.Say("      ssh -N -L 8765:127.0.0.1:8765 <this-machine> and this line is long but is never wrapped")
	want := `  │  The board is at
  │  https://github.com/orgs/acme/projects/3. Open it and
  │  look at the columns there.
  │    • read and write code, pull requests and issues, and
  │      more,
  │      only in repositories you install it on,
  │    • read boards.
  │
  │    ssh -N -L 8765:127.0.0.1:8765 <this-machine> and this line is long but is never wrapped
`
	if got := text(out); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// The address is coloured without the full stop that follows it.
	if !strings.Contains(out.String(), underline+"https://github.com/orgs/acme/projects/3"+reset+".") {
		t.Fatalf("the address is not set apart: %q", out.String())
	}
}

func TestStyledAnswers(t *testing.T) {
	u, out := styled("nope\nacme/shop\n\n")
	got, err := u.Ask("  Which repository?", "", validRepo)
	if err != nil || got != "acme/shop" {
		t.Fatalf("%q, %v", got, err)
	}
	if got, _ := u.Ask("  Name?", "mac", nil); got != "mac" {
		t.Fatalf("default: %q", got)
	}
	if s := text(out); !strings.Contains(s, "?  Which repository?") || !strings.Contains(s, "✖  ") || !strings.Contains(s, "Enter for mac") {
		t.Fatalf("output:\n%s", s)
	}

	// Yes and no are one key; other keys are ignored; Ctrl-C stops.
	u, out = styled("xqY")
	if ok, err := u.Confirm("  Create it?"); err != nil || !ok || !strings.Contains(text(out), "Create it?  y / n  yes") {
		t.Fatalf("%v %v\n%s", ok, err, text(out))
	}
	u, _ = styled("n")
	if ok, _ := u.Confirm("  Create it?"); ok {
		t.Fatal("n was taken for yes")
	}
	u, _ = styled("\x03")
	if _, err := u.Confirm("  Create it?"); !errors.Is(err, ErrAborted) {
		t.Fatalf("Ctrl-C: %v", err)
	}
}

func TestChoose(t *testing.T) {
	options := []string{"Create a new board", "Use board #1", "Use board #3"}
	for keys, want := range map[string]int{
		"\r":                   0,
		"\x1b[B\r":             1,
		"\x1b[B\x1b[B\r":       2,
		"\x1b[A\r":             2, // up from the first goes to the last
		"\x1b[B\x1b[B\x1b[B\r": 0,
		"jjk\r":                1,
		"3\r":                  2,
		"9x\x1bOB\r":           1,
	} {
		u, out := styled(keys)
		got, err := u.Choose("Which one?", options)
		if err != nil || got != want {
			t.Errorf("%q: %d, %v", keys, got, err)
		}
		if s := text(out); !strings.HasSuffix(s, "✔  "+options[want]+"\n") {
			t.Errorf("%q: ends with %q", keys, s[max(0, len(s)-40):])
		}
	}
	u, _ := styled("\x1b[B")
	if _, err := u.Choose("Which one?", options); !errors.Is(err, ErrAborted) {
		t.Fatalf("input that ends: %v", err)
	}
	// Without keys the options are numbered.
	var out bytes.Buffer
	plain := NewUI(strings.NewReader("7\n2\n"), &out)
	if got, err := plain.Choose("Which one?", options); err != nil || got != 1 || !strings.Contains(out.String(), "    2  Use board #1") {
		t.Fatalf("%d, %v\n%s", got, err, out.String())
	}
}

type oneStep struct {
	done bool
	err  error
}

func (*oneStep) Title() string { return "Create the board" }
func (s *oneStep) Check(context.Context, *State) (Status, error) {
	if s.done {
		return Status{Done: true, Detail: "It is at https://example.com/b"}, nil
	}
	return Status{Detail: "There is none yet."}, nil
}
func (s *oneStep) Run(_ context.Context, ui *UI, _ *State) error {
	ui.Say("  Creating it.")
	s.done = s.err == nil
	return s.err
}

func TestStyledRun(t *testing.T) {
	st, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u, out := styled("")
	u.Width = func() int { return 100 }
	if err := Run(context.Background(), u, st, []Step{&oneStep{done: true}, &oneStep{}}); err != nil {
		t.Fatal(err)
	}
	s := text(out)
	for _, want := range []string{"✔  Create the board · It is at https://example.com/b\n", "◆  Create the board  ●○  step 2 of 2\n",
		"│  There is none yet.\n", "│  Creating it.\n", "✔  Done. It is at https://example.com/b\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Checking") || strings.Count(out.String(), hideCursor) != strings.Count(out.String(), showCursor) {
		t.Errorf("the spinner left something behind:\n%q", out.String())
	}
	// A step that fails leaves the terminal as it found it.
	u, out = styled("")
	if err := Run(context.Background(), u, st, []Step{&oneStep{err: errors.New("no")}}); err == nil || u.inStep {
		t.Fatalf("error %v, still in a step: %v", err, u.inStep)
	}
}
