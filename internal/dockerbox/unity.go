package dockerbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/iandeherdt/Maestro/internal/unity"
)

// activateScript runs Unity's own activation in a container of the sandbox
// image and writes what it left in the home directory to standard output
// as a tar stream. The account comes on standard input, one line each:
// email, password, serial (empty for a Personal licence), so it is not in
// the container's settings. Unity takes it only as arguments, so it is in
// the Editor's command line while the activation runs.
const activateScript = `set -eu
IFS= read -r email
IFS= read -r password
IFS= read -r serial || true
U=/opt/unity/Editor/Unity
if [ ! -x "$U" ]; then
  echo "The sandbox image has no Unity Editor. Switch the unity conventions pack on and run maestro image first." >&2
  exit 3
fi
set -- -batchmode -nographics -quit -username "$email" -password "$password" -logFile /tmp/activate.log
if [ -n "$serial" ]; then set -- "$@" -serial "$serial"; fi
pre=""
if [ -f /usr/lib/game-ci/libsem_eintr_retry.so ]; then pre=/usr/lib/game-ci/libsem_eintr_retry.so; fi
status=0
LD_PRELOAD="$pre" xvfb-run -a -s "-screen 0 1024x768x24" "$U" "$@" >/dev/null 2>&1 || status=$?
grep -iE 'licen[cs]|entitlement' /tmp/activate.log 2>/dev/null | grep -viE 'password' | tail -n 15 >&2 || true
cd "$HOME"
found=""
for p in ` + "{{places}}" + `; do
  if [ -d "$p" ]; then found="$found $p"; fi
done
if [ -z "$found" ]; then
  echo "Unity ended with status $status and left no licence." >&2
  exit 1
fi
tar -cf - $found
`

// signInScript installs Unity's command-line tool in the container, signs
// in through the browser, activates a Personal licence and writes it to
// standard output as a tar stream, like activateScript. The sign-in is
// kept in a plain file: a container has no keyring, and it is thrown away
// afterwards, so nothing of it stays. Everything the
// person should read goes to standard error.
const signInScript = `set -eu
export PATH="$HOME/.local/bin:$PATH"
if [ ! -x /opt/unity/Editor/Unity ]; then
  echo "The sandbox image has no Unity Editor. Switch the unity conventions pack on and run maestro image first." >&2
  exit 3
fi
if ! command -v unzip >/dev/null 2>&1; then
  echo "The sandbox image is older than this version of Maestro and lacks unzip, which Unity's" >&2
  echo "tool needs. Build it again with: maestro image" >&2
  exit 3
fi
echo "Installing Unity's command-line tool in the container (it is thrown away afterwards)." >&2
got=""
for url in https://unity.com/install.sh https://public-cdn.cloud.unity3d.com/hub/prod/cli/install.sh; do
  if curl -fsSL --proto '=https' --tlsv1.2 -o /tmp/unity-install.sh "$url"; then got=1; break; fi
done
if [ -z "$got" ]; then
  echo "Unity's install script could not be downloaded." >&2
  exit 1
fi
if ! bash /tmp/unity-install.sh >/tmp/install.log 2>&1 || ! command -v unity >/dev/null 2>&1; then
  tail -n 20 /tmp/install.log >&2
  echo "Unity's command-line tool did not install." >&2
  exit 1
fi
echo >&2
echo "Open the link below in your browser, sign in to Unity (two-step verification works)" >&2
echo "and allow the login request. The link works once, for about five minutes." >&2
echo >&2
unity auth login --insecure-storage </dev/null >&2
echo >&2
echo "Signed in. Activating a Personal licence." >&2
# Unity's tool downloads Unity's licensing client itself and unpacks it
# with unzip; the client then activates the licence.
activated=""
if unity license activate --personal --accept-eula </dev/null >&2; then activated=1; fi
if [ -z "$activated" ]; then
  echo "What Unity's tool logged:" >&2
  unity doctor </dev/null 2>&1 | grep -E '^recentLog' | tail -n 8 >&2 || true
  exit 1
fi
cd "$HOME"
found=""
for p in ` + "{{places}}" + `; do
  if [ -d "$p" ]; then found="$found $p"; fi
done
if [ -z "$found" ]; then
  echo "No licence came out of the activation. Files with licence in their name:" >&2
  find . -iname '*licen*' -o -iname '*.ulf' -o -iname '*entitlement*' 2>/dev/null | head -n 20 >&2
  echo "What Unity's tool reports:" >&2
  unity doctor </dev/null 2>&1 | head -n 40 >&2 || true
  exit 1
fi
tar -cf - $found
`

// Account is what Unity's activation needs.
type Account struct{ Email, Password, Serial string }

// ActivateUnity runs Unity's activation in a container of the sandbox
// image and returns the licence it produced, relative path to content. The
// container has the internet directly, not through a proxy, because it
// runs nothing of the repository: no workspace is mounted. It has the
// hostname of every sandbox step, which a licence may be tied to.
func (b *Box) ActivateUnity(ctx context.Context, acct Account, stderr io.Writer) (map[string][]byte, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	for _, v := range []string{acct.Email, acct.Password, acct.Serial} {
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fail("the account details must be one line each")
		}
	}
	if acct.Email == "" || acct.Password == "" {
		return nil, fail("the activation needs the email address and the password of the Unity account")
	}
	return b.activate(ctx, activateScript, acct.Email+"\n"+acct.Password+"\n"+acct.Serial+"\n", stderr)
}

// SignInUnity activates a Personal licence through Unity's own sign-in in
// a browser, so an account with two-step verification works. Unity's
// command-line tool prints a link and waits while the person signs in on
// any machine; nothing is typed into the container.
func (b *Box) SignInUnity(ctx context.Context, stderr io.Writer) (map[string][]byte, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	return b.activate(ctx, signInScript, "", stderr)
}

// activate runs script in a container of the sandbox image, with input on
// its standard input, and returns the licence it wrote to standard output.
func (b *Box) activate(ctx context.Context, script, input string, stderr io.Writer) (map[string][]byte, error) {
	name := b.prefix() + "-unity-" + token()
	args := []string{"run", "--rm", "--interactive", "--name", name,
		"--label", "maestro.worker=" + b.Worker, "--label", "maestro.step=unity-activate",
		"--user", AgentUID + ":" + AgentUID,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--hostname", "sandbox", "--init", "--entrypoint", "",
		"--memory", "4g", "--pids-limit", "1024",
		"--workdir", "/home/agent",
	}
	args = append(args, b.engineFlags()...)
	env := b.baseEnv("")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}
	places := make([]string, len(unity.Places))
	for i, p := range unity.Places {
		places[i] = "'" + p + "'"
	}
	script = strings.Replace(script, "{{places}}", strings.Join(places, " "), 1)
	args = append(args, b.Image, "sh", "-c", script)

	cmd := exec.CommandContext(ctx, b.docker(), args...)
	cmd.Stdin = strings.NewReader(input)
	var out capped
	out.max = 8 << 20
	cmd.Stdout, cmd.Stderr = &out, stderr
	err := cmd.Run()
	b.remove(name)
	if err != nil {
		return nil, fail("the Unity activation did not succeed: %v", err)
	}
	return readLicence(out.buf.Bytes())
}

// readLicence takes the files of a licence out of a tar stream. Anything
// but a regular file in one of the places a licence is kept is skipped.
func readLicence(raw []byte) (map[string][]byte, error) {
	files := map[string][]byte{}
	r := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fail("the licence could not be read: %v", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg || !unity.Allowed(name) {
			continue
		}
		if h.Size > 1<<20 {
			return nil, fail("%s is larger than a licence", name)
		}
		body, err := io.ReadAll(io.LimitReader(r, h.Size))
		if err != nil {
			return nil, fail("the licence could not be read: %v", err)
		}
		files[name] = body
	}
	if len(files) == 0 {
		return nil, fail("the activation left no licence behind")
	}
	return files, nil
}

// String keeps the password out of any log line that prints an Account.
func (a Account) String() string { return fmt.Sprintf("Unity account %s", a.Email) }
