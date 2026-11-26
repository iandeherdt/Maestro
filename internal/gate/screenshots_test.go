package gate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// local runs the steps on this machine, in a directory laid out like a
// workspace, with nothing on PATH but the tools the scripts name. That is
// the scripts themselves under test, not a description of them.
type local struct {
	t    *testing.T
	repo string
	bin  string
}

func newLocal(t *testing.T, extra ...string) *local {
	t.Helper()
	root := t.TempDir()
	l := &local{t: t, repo: filepath.Join(root, "repo"), bin: filepath.Join(root, "bin")}
	os.MkdirAll(l.repo, 0o755)
	os.MkdirAll(l.bin, 0o755)
	for _, tool := range append([]string{"sh", "git", "find", "sort", "sed", "basename", "dirname", "wc", "head", "base64", "tr", "rm", "mkdir", "sleep", "cat", "printf", "touch"}, extra...) {
		// The system's own tools first: PATH may put a wrapper in front.
		p := ""
		for _, dir := range []string{"/usr/bin", "/bin"} {
			if exists(filepath.Join(dir, tool)) {
				p = filepath.Join(dir, tool)
				break
			}
		}
		if p == "" {
			var err error
			if p, err = exec.LookPath(tool); err != nil {
				t.Skipf("%s is not installed", tool)
			}
		}
		os.Symlink(p, filepath.Join(l.bin, tool))
	}
	l.git("init", "--quiet")
	os.WriteFile(filepath.Join(l.repo, "index.html"), []byte("<!doctype html><title>t</title><h1 style='color:#c00'>Hello world</h1>"), 0o644)
	l.git("add", "-A")
	l.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "first")
	return l
}

func (l *local) git(args ...string) {
	l.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = l.repo
	if out, err := cmd.CombinedOutput(); err != nil {
		l.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (l *local) Run(ctx context.Context, req sandbox.Request, stdin []byte) (sandbox.Result, error) {
	if req.Step != sandbox.StepVerify || req.NetworkProfile != sandbox.NetNone {
		l.t.Errorf("step %s with network %q", req.Step, req.NetworkProfile)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(l.bin, req.Argv[0]), req.Argv[1:]...)
	cmd.Dir = l.repo
	cmd.Env = []string{"PATH=" + l.bin, "HOME=" + filepath.Dir(l.repo)}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = bytes.NewReader(stdin)
	err := cmd.Run()
	res := sandbox.Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitStatus = ee.ExitCode()
		err = nil
	}
	return res, err
}

func (l *local) gates(sh config.Screenshots) *Gates {
	if sh.TimeoutMS == 0 {
		sh.TimeoutMS = 120000
	}
	cfg := &config.Config{Screenshots: sh}
	return &Gates{Config: func() *config.Config { return cfg }, Launcher: l}
}

func picture(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{200, 0, 0, 255})
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestScreenshotsFromACommand(t *testing.T) {
	l := newLocal(t)
	good := base64.StdEncoding.EncodeToString(picture(40, 30))
	// A text chunk after the header: a place to hide words in a picture.
	raw := picture(8, 8)
	chunk := []byte{0, 0, 0, 6, 't', 'E', 'X', 't', 'a', 0, 'h', 'i', 'd', 'e'}
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	hidden := base64.StdEncoding.EncodeToString(append(append(append([]byte{}, raw[:33]...), chunk...), raw[33:]...))
	g := l.gates(config.Screenshots{Command: `
printf %s '` + good + `' | base64 -d > "$SCREENSHOT_DIR/b home <@page>.png"
printf %s '` + hidden + `' | base64 -d > "$SCREENSHOT_DIR/a.png"
printf 'not a picture' > "$SCREENSHOT_DIR/c.png"
printf 'ignored' > "$SCREENSHOT_DIR/notes.txt"
touch built.js; printf changed > index.html`})
	shots, note := g.screenshots(ctx, "GH-1")
	if len(shots) != 2 || shots[0].Caption != "a" || shots[1].Caption != "b home page" {
		t.Fatalf("shots %+v, note %q", shots, note)
	}
	if !strings.Contains(note, `"c" is not a PNG picture`) {
		t.Fatalf("note %q", note)
	}
	for _, s := range shots {
		if _, err := png.Decode(bytes.NewReader(s.PNG)); err != nil || bytes.Contains(s.PNG, []byte("tEXt")) || bytes.Contains(s.PNG, []byte("hide")) {
			t.Fatalf("%s was not written again as pixels only: %v", s.Caption, err)
		}
	}
	// The repository is as it was exported, and the pictures are gone.
	if _, err := os.Stat(filepath.Join(l.repo, "built.js")); err == nil {
		t.Fatal("a file the command left behind is still there")
	}
	if b, _ := os.ReadFile(filepath.Join(l.repo, "index.html")); !strings.Contains(string(b), "Hello world") {
		t.Fatal("a file the command changed was not put back")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.repo), "screenshots")); err == nil {
		t.Fatal("the pictures were left in the workspace")
	}
}

func TestScreenshotsThatGoWrong(t *testing.T) {
	l := newLocal(t)
	if shots, note := l.gates(config.Screenshots{Command: "exit 7"}).screenshots(ctx, "GH-1"); len(shots) != 0 || note != "No screenshots: screenshots.command exited with status 7." {
		t.Fatalf("%d shots, note %q", len(shots), note)
	}
	if shots, note := l.gates(config.Screenshots{Command: "true"}).screenshots(ctx, "GH-1"); len(shots) != 0 || !strings.Contains(note, "wrote no PNG file") {
		t.Fatalf("%d shots, note %q", len(shots), note)
	}
	// What the agent put there before the step is not taken for a picture.
	dir := filepath.Join(filepath.Dir(l.repo), "screenshots")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "planted.png"), picture(4, 4), 0o644)
	if shots, _ := l.gates(config.Screenshots{Command: "true"}).screenshots(ctx, "GH-1"); len(shots) != 0 {
		t.Fatalf("a planted picture was taken: %+v", shots)
	}
	// More than the pull request shows, and one that is too large.
	many := `i=1; while [ $i -le 8 ]; do printf %s '` + base64.StdEncoding.EncodeToString(picture(4, 4)) + `' | base64 -d > "$SCREENSHOT_DIR/$i.png"; i=$((i+1)); done
head -c 800000 /dev/zero > "$SCREENSHOT_DIR/0.png"`
	shots, note := l.gates(config.Screenshots{Command: many}).screenshots(ctx, "GH-1")
	if len(shots) != config.MaxScreenshots-1 || !strings.Contains(note, `"0" is larger than 700 KB`) || !strings.Contains(note, "only the first 6 are shown") {
		t.Fatalf("%d shots, note %q", len(shots), note)
	}
	// No browser in the image.
	_, note = l.gates(config.Screenshots{Pages: []string{"index.html"}, Width: 800, Height: 600}).screenshots(ctx, "GH-1")
	if !strings.Contains(note, "the sandbox image has no browser") {
		t.Fatalf("note %q", note)
	}
}

func TestRepaintRefusesWhatIsNotASmallPicture(t *testing.T) {
	huge := picture(1, 1)
	// The header claims 60000 x 60000.
	copy(huge[16:24], []byte{0, 0, 0xea, 0x60, 0, 0, 0xea, 0x60})
	for name, raw := range map[string][]byte{"text": []byte("hello"), "empty": nil, "claims to be huge": huge} {
		if _, ok := repaint(base64.StdEncoding.EncodeToString(raw)); ok {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, ok := repaint("not base64!"); ok {
		t.Error("text that is not base64 was accepted")
	}
}

// browser finds a real browser, or skips the test.
func browser(t *testing.T) string {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if p := "/opt/pw-browsers/chromium"; exists(p) {
		return p
	}
	t.Skip("no browser on this machine")
	return ""
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestScreenshotsOfPagesWithARealBrowser(t *testing.T) {
	path := browser(t)
	l := newLocal(t, "node")
	// Chromium finds its own files next to the real program, so the name
	// on PATH is a small script and not a link.
	os.WriteFile(filepath.Join(l.bin, "chromium"), []byte("#!/bin/sh\nexec "+path+" \"$@\"\n"), 0o755)
	os.WriteFile(filepath.Join(l.repo, "serve.js"), []byte(`require("http").createServer((q, s) => { s.setHeader("content-type", "text/html"); s.end("<h1>Served</h1>") }).listen(38917, "127.0.0.1")`), 0o644)
	l.git("add", "-A")
	l.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "server")
	pages := []string{"index.html", "http://127.0.0.1:38917/"}
	shots, note := l.gates(config.Screenshots{Pages: pages, Start: "sleep 2; node serve.js", Width: 640, Height: 400}).screenshots(ctx, "GH-1")
	if len(shots) != 2 || note != "" {
		t.Fatalf("%d shots, note %q", len(shots), note)
	}
	for i, s := range shots {
		c, err := png.DecodeConfig(bytes.NewReader(s.PNG))
		if err != nil || c.Width != 640 || c.Height != 400 || s.Caption != pages[i] {
			t.Fatalf("shot %d: %q %dx%d %v", i, s.Caption, c.Width, c.Height, err)
		}
	}
	// The first page has red text on white: the picture is not blank.
	img, _ := png.Decode(bytes.NewReader(shots[0].PNG))
	red := 0
	for y := 0; y < 400; y++ {
		for x := 0; x < 640; x++ {
			if r, g, b, _ := img.At(x, y).RGBA(); r > 0x8000 && g < 0x4000 && b < 0x4000 {
				red++
			}
		}
	}
	if red < 50 {
		t.Fatalf("the page was not drawn: %d red pixels", red)
	}
	// A page that is not there, and an address nothing answers at.
	if _, note := l.gates(config.Screenshots{Pages: []string{"missing.html"}, Width: 640, Height: 400}).screenshots(ctx, "GH-1"); note != `No screenshots: "missing.html" is not a file in the repository.` {
		t.Fatalf("note %q", note)
	}
}

func TestAFailedScreenshotCommandIsExplainedInTheLog(t *testing.T) {
	l := newLocal(t)
	var logged bytes.Buffer
	g := l.gates(config.Screenshots{Command: `printf 'one\ntwo\nconventions: the Unity Editor is missing\033[31m\n' >&2; exit 1`})
	g.Config().Worker.Name = "mac-mini"
	g.Log = slog.New(slog.NewTextHandler(&logged, nil))
	_, note := g.screenshots(ctx, "GH-1")
	if note != "No screenshots: screenshots.command exited with status 1. Its last lines are in the service log of worker `mac-mini`: search it for `screenshots.command failed`." {
		t.Fatalf("note %q", note)
	}
	out := logged.String()
	if !strings.Contains(out, "screenshots.command failed") || !strings.Contains(out, "the Unity Editor is missing") || strings.Contains(out, "\033") {
		t.Fatalf("log %q", out)
	}
	// The command's text never reaches the note, which goes into the pull request.
	if strings.Contains(note, "Unity") {
		t.Fatal("the command's output is in the note")
	}
}
