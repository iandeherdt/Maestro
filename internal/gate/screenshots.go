package gate

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// Screenshots (SPEC 24). The service takes them itself, in the sandbox and
// without network, from exactly the commits that passed verify. The agent
// has no part in it, but it wrote the code the pictures show.
//
// A picture leaves the sandbox as text on standard output, one step per
// picture, so no new directory is shared with the sandbox. The service
// decodes it and writes it again: what is uploaded holds pixels only.

const (
	// maxShotBytes is the largest file taken out of the sandbox. With
	// base64 it stays below the launcher's cap on standard output.
	maxShotBytes  = 700 << 10
	maxShotPixels = 24_000_000
	maxShotSide   = 12000
	collectTime   = time.Minute
)

// shotDir is where the pictures are written: next to the repository, so
// that they are never committed. It is emptied first, because the agent
// can write there too.
const shotDir = `d="$(dirname "$PWD")/screenshots"
`

const pagesScript = `set -u
` + shotDir + `{ rm -rf "$d" && mkdir -p "$d"; } || exit 2
w="$1"; h="$2"; start="$3"; shift 3
if [ -n "$start" ]; then
  ( sh -c "$start" >"$d/.start.log" 2>&1 & )
fi
browser=""
for b in chromium chromium-browser google-chrome chrome; do
  if command -v "$b" >/dev/null 2>&1; then browser="$b"; break; fi
done
if [ -z "$browser" ]; then echo "NO_BROWSER"; exit 3; fi
i=0
for page in "$@"; do
  i=$((i+1))
  case "$page" in
    http://*)
      n=0
      until node -e 'fetch(process.argv[1]).then(()=>process.exit(0),()=>process.exit(1))' "$page" 2>/dev/null; do
        n=$((n+1))
        if [ "$n" -ge 90 ]; then echo "NO_ANSWER $i"; exit 4; fi
        sleep 1
      done
      url="$page" ;;
    *)
      if [ ! -f "$page" ]; then echo "NO_FILE $i"; exit 5; fi
      url="file://$PWD/$page" ;;
  esac
  "$browser" --headless --no-sandbox --disable-gpu --disable-dev-shm-usage --hide-scrollbars \
    --window-size="$w,$h" --virtual-time-budget=4000 --user-data-dir="/tmp/browser-$i" \
    --screenshot="$d/$i.png" "$url" >/dev/null 2>&1
  if [ ! -s "$d/$i.png" ]; then echo "NO_PICTURE $i"; exit 6; fi
done
`

const commandScript = `set -u
` + shotDir + `{ rm -rf "$d" && mkdir -p "$d"; } || exit 2
SCREENSHOT_DIR="$d"; export SCREENSHOT_DIR
sh -c "$1"
`

// collectScript prints the n-th picture: its name, its size and its
// content. Nothing is printed when there is no n-th picture.
const collectScript = `set -u
` + shotDir + `f="$(find "$d" -maxdepth 1 -type f -name '*.png' 2>/dev/null | LC_ALL=C sort | sed -n "$1p")"
[ -n "$f" ] || exit 0
basename "$f"
wc -c < "$f"
head -c "$2" "$f" | base64 | tr -d '\n'
`

// tidyScript puts the repository back as it was exported. A command that
// builds the site may have left files behind, and they must not turn up in
// a later commit.
const tidyScript = `set -u
G="git -c core.hooksPath=/dev/null -c core.fsmonitor=false"
$G reset --quiet --hard
$G clean --quiet -fd
` + shotDir + `rm -rf "$d"
`

func (g *Gates) step(ctx context.Context, key string, timeout time.Duration, argv ...string) (sandbox.Result, error) {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The verify step is the one for commands of the repository that run
	// without the agent's login.
	return g.Launcher.Run(sctx, sandbox.Request{
		Step: sandbox.StepVerify, WorkspaceKey: key, Argv: argv, NetworkProfile: sandbox.NetNone,
		Limits: sandbox.Limits{TimeoutMS: timeout.Milliseconds()},
	}, nil)
}

var captionRE = regexp.MustCompile(`[^A-Za-z0-9._ -]`)

// screenshots never fails the work: what goes wrong is told in the note,
// which ends up in the pull request.
func (g *Gates) screenshots(ctx context.Context, key string) ([]worker.Screenshot, string) {
	cfg := g.Config().Screenshots
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), collectTime)
		defer cancel()
		g.step(tctx, key, collectTime, "sh", "-c", tidyScript)
	}()

	argv := []string{"sh", "-c", commandScript, "sh", cfg.Command}
	if len(cfg.Pages) > 0 {
		argv = append([]string{"sh", "-c", pagesScript, "sh",
			strconv.FormatInt(cfg.Width, 10), strconv.FormatInt(cfg.Height, 10), cfg.Start}, cfg.Pages...)
	}
	res, err := g.step(ctx, key, timeout, argv...)
	switch {
	case ctx.Err() != nil:
		return nil, ""
	case err != nil:
		return nil, fmt.Sprintf("No screenshots: taking them did not finish within %s, or the sandbox could not be started.", timeout)
	case res.ExitStatus != 0:
		return nil, "No screenshots: " + reason(cfg, res)
	}

	var shots []worker.Screenshot
	var left []string
	for n := 1; n <= config.MaxScreenshots+1; n++ {
		res, err := g.step(ctx, key, collectTime, "sh", "-c", collectScript, "sh", strconv.Itoa(n), strconv.Itoa(maxShotBytes))
		if err != nil || res.ExitStatus != 0 {
			return shots, "Some screenshots could not be read from the sandbox."
		}
		lines := strings.SplitN(strings.TrimSpace(string(res.Stdout)), "\n", 3)
		if len(lines) < 3 {
			break
		}
		if n > config.MaxScreenshots {
			left = append(left, fmt.Sprintf("only the first %d are shown", config.MaxScreenshots))
			break
		}
		caption := strings.TrimSpace(captionRE.ReplaceAllString(strings.TrimSuffix(lines[0], ".png"), ""))
		if r := []rune(caption); len(r) > 60 {
			caption = string(r[:60])
		}
		if len(cfg.Pages) > 0 {
			// The name is a number the script chose; the caption is the
			// page from the settings.
			i, err := strconv.Atoi(caption)
			if err != nil || i < 1 || i > len(cfg.Pages) {
				continue
			}
			caption = cfg.Pages[i-1]
		}
		if caption == "" {
			caption = "screenshot " + strconv.Itoa(n)
		}
		size, _ := strconv.Atoi(strings.TrimSpace(lines[1]))
		if size > maxShotBytes {
			left = append(left, fmt.Sprintf("%q is larger than %d KB", caption, maxShotBytes>>10))
			continue
		}
		clean, ok := repaint(lines[2])
		if !ok {
			left = append(left, fmt.Sprintf("%q is not a PNG picture of a usable size", caption))
			continue
		}
		shots = append(shots, worker.Screenshot{Caption: caption, PNG: clean})
	}
	note := ""
	if len(left) > 0 {
		note = "Left out: " + strings.Join(left, "; ") + "."
	} else if len(shots) == 0 {
		note = "No screenshots: the command wrote no PNG file into $SCREENSHOT_DIR."
	}
	return shots, note
}

// reason turns a failed step into words. The output of the step is not
// quoted: it is the sandbox's text.
func reason(cfg config.Screenshots, res sandbox.Result) string {
	if len(cfg.Pages) == 0 {
		return fmt.Sprintf("screenshots.command exited with status %d.", res.ExitStatus)
	}
	word, arg, _ := strings.Cut(strings.TrimSpace(lastLine(res.Stdout)), " ")
	page := "a page"
	if i, err := strconv.Atoi(arg); err == nil && i >= 1 && i <= len(cfg.Pages) {
		page = fmt.Sprintf("%q", cfg.Pages[i-1])
	}
	switch word {
	case "NO_BROWSER":
		return "the sandbox image has no browser. Run `symphony image` again; it adds one when screenshots.pages is set."
	case "NO_ANSWER":
		return "nothing answered at " + page + " within 90 seconds. Check screenshots.start."
	case "NO_FILE":
		return page + " is not a file in the repository."
	case "NO_PICTURE":
		return "the browser could not show " + page + "."
	}
	return fmt.Sprintf("taking them stopped with status %d.", res.ExitStatus)
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// repaint decodes a PNG and encodes it again. Text chunks, trailing data
// and anything else that is not a pixel does not survive that.
func repaint(b64 string) ([]byte, bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) > maxShotBytes {
		return nil, false
	}
	// The size is read first, so that a small file that claims to be a
	// huge picture is never unpacked.
	c, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || c.Width < 1 || c.Height < 1 || c.Width > maxShotSide || c.Height > maxShotSide || c.Width*c.Height > maxShotPixels {
		return nil, false
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, img); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}
