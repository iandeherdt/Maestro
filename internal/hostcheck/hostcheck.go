// Package hostcheck runs the unity pack's checks on this machine instead of
// in the sandbox, with the Unity Editor that Unity Hub installed here.
//
// Unity's Editor for Linux crashes when it runs emulated, which is the only
// way it runs on a Mac with Apple silicon. So on such a Mac the service
// checks out exactly what the export step produced into a folder of its
// own and runs the pack's verify script there, inside macOS's sandbox
// (sandbox-exec): no network but the loopback, where a proxy lets through
// only Unity's hosts and the allowlist of the hooks; no writes outside the
// check's folder and Unity's own folders; and no reads of Maestro's setup
// and state, where the keys are. It is a weaker fence than the container:
// the code the agent wrote runs as the user who runs Maestro.
package hostcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/iandeherdt/Maestro/internal/proxy"
)

// MaxOutput is how much of the end of the output is kept.
const MaxOutput = 256 << 10

// UnityHosts are the hosts Unity needs besides the packages: its licensing
// and its accounts. They are Unity's own domains.
var UnityHosts = []string{"*.unity.com", "*.unity3d.com"}

// Runner runs checks on this machine.
type Runner struct {
	// MirrorDir is the service's bare copy of the repository.
	MirrorDir string
	// HandoffDir is where the export step leaves <workspace_key>.bundle.
	HandoffDir string
	// WorkDir holds a folder per workspace, kept between checks so that
	// Unity's Library (the imported assets) is kept too.
	WorkDir string
	// Secret are folders the checks may not read, such as Maestro's setup
	// and state.
	Secret []string
	// Allow returns the hosts the checks may reach, besides UnityHosts.
	Allow func() []string
	Log   *slog.Logger

	// Set in tests.
	sandboxExec string
	home        string
	noSandbox   bool
}

// Result is what a check printed and how it ended.
type Result struct {
	ExitStatus int
	Output     []byte
}

var (
	keyRE     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	shaRE     = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	versionRE = regexp.MustCompile(`/Editor/([0-9]+\.[0-9]+\.[0-9]+[abfpx][0-9]+)/`)
)

// EditorVersion is the Unity version of an Editor that Unity Hub
// installed, read from its path. Empty when the path does not say.
func EditorVersion(editor string) string {
	if m := versionRE.FindStringSubmatch(filepath.ToSlash(editor)); m != nil {
		return m[1]
	}
	return ""
}

// HubLicence says whether Unity Hub activated a licence for this user on
// this Mac. The Editor uses it; the checks need nothing else.
func HubLicence(home string) bool {
	for _, d := range []string{
		filepath.Join(home, "Library", "Unity", "licenses"),
		"/Library/Application Support/Unity",
		filepath.Join(home, "Library", "Application Support", "Unity"),
	} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if e.Type().IsRegular() && (strings.HasSuffix(n, ".xml") || strings.HasSuffix(n, ".ulf")) {
				return true
			}
		}
	}
	return false
}

func (r *Runner) homeDir() (string, error) {
	if r.home != "" {
		return r.home, nil
	}
	return os.UserHomeDir()
}

// Unity runs script, the unity pack's verify script, against the commit
// head of a workspace, with editor. An error is a problem of this machine,
// not of the change: the Editor or the licence is missing, or the commit
// could not be checked out.
func (r *Runner) Unity(ctx context.Context, editor, key, head, script string) (Result, error) {
	switch {
	case !keyRE.MatchString(key) || strings.Contains(key, ".."):
		return Result{}, errors.New("invalid workspace key")
	case !shaRE.MatchString(head):
		return Result{}, errors.New("invalid commit")
	case !filepath.IsAbs(editor) || strings.ContainsAny(editor, "\"$`\\\n"):
		return Result{}, errors.New("unity.editor is not a full path")
	}
	if st, err := os.Stat(editor); err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
		return Result{}, fmt.Errorf("the Unity Editor %s is not there. Install it with Unity Hub, "+
			"or run `maestro new` again to find the one this project needs", editor)
	}
	version := EditorVersion(editor)
	if version == "" {
		return Result{}, fmt.Errorf("the Unity version cannot be told from the path %s; unity.editor must be an Editor that Unity Hub installed", editor)
	}
	home, err := r.homeDir()
	if err != nil {
		return Result{}, err
	}
	if !HubLicence(home) {
		return Result{}, errors.New("Unity Hub has no licence on this Mac. Open Unity Hub, sign in, " +
			"and add a licence (Preferences, Licenses, Add, Get a free personal licence)")
	}

	dir := filepath.Join(r.WorkDir, key)
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, err
	}
	if err := r.checkout(ctx, repo, key, head); err != nil {
		return Result{}, fmt.Errorf("the change could not be checked out for the Unity checks: %w", err)
	}
	shim, fakeHome, err := shims(dir, editor, version, home)
	if err != nil {
		return Result{}, err
	}

	// The proxy is the only way out. It listens on the loopback, which is
	// all the sandbox lets through.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Result{}, err
	}
	p := &proxy.Proxy{Profile: "unity-checks", Log: r.Log, Allow: func() []string {
		hosts := append([]string{}, UnityHosts...)
		if r.Allow != nil {
			hosts = append(hosts, r.Allow()...)
		}
		return hosts
	}}
	go p.Serve(ln)
	defer func() { ln.Close(); p.CloseConnections() }()
	proxyURL := "http://" + ln.Addr().String()

	argv := []string{"/bin/sh", "-c", script}
	if !r.noSandbox {
		profile := filepath.Join(dir, "sandbox.sb")
		text, err := Profile(dir, home, r.Secret)
		if err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(profile, []byte(text), 0o600); err != nil {
			return Result{}, err
		}
		se := r.sandboxExec
		if se == "" {
			se = "/usr/bin/sandbox-exec"
		}
		argv = append([]string{se, "-f", profile}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = repo
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	cmd.Env = []string{
		"PATH=" + filepath.Join(shim, "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + fakeHome, "TMPDIR=" + tmp, "LANG=en_US.UTF-8", "USER=" + os.Getenv("USER"),
		"UNITY_PATH=" + filepath.Join(shim, "unity"),
		"HTTPS_PROXY=" + proxyURL, "HTTP_PROXY=" + proxyURL, "https_proxy=" + proxyURL, "http_proxy=" + proxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
	}
	out := &tailBuffer{max: MaxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	// Unity starts helpers of its own; a stop ends all of them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // what is left of the group
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return Result{Output: out.Bytes()}, nil
	case ctx.Err() != nil:
		return Result{ExitStatus: -1, Output: out.Bytes()}, ctx.Err()
	case errors.As(err, &exit):
		return Result{ExitStatus: exit.ExitCode(), Output: out.Bytes()}, nil
	}
	return Result{}, err
}

// Forget removes the folder of a workspace that was removed.
func (r *Runner) Forget(key string) {
	if keyRE.MatchString(key) && !strings.Contains(key, "..") && r.WorkDir != "" {
		os.RemoveAll(filepath.Join(r.WorkDir, key))
	}
}

// git runs git as the service, without the user's or the system's git
// settings: the tree comes from the agent, and a filter or hook in the
// user's settings must not run on it.
func git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return fmt.Errorf("git %s: %v: %s", args[0], err, msg)
	}
	return nil
}

// checkout puts the exported commit head into repo. The folders Unity
// makes (Library and the like) stay, so the next check is fast.
func (r *Runner) checkout(ctx context.Context, repo, key, head string) error {
	bundle := filepath.Join(r.HandoffDir, key+".bundle")
	st, err := os.Lstat(bundle)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("the bundle is not a regular file")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		os.RemoveAll(repo)
		if err := git(ctx, filepath.Dir(repo), "init", "--quiet", repo); err != nil {
			return err
		}
	}
	// The base the bundle builds on comes from the mirror.
	if err := git(ctx, repo, "fetch", "--quiet", "--no-tags", r.MirrorDir, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return err
	}
	if err := git(ctx, repo, "-c", "protocol.file.allow=always", "fetch", "--quiet", "--no-tags", bundle, "+HEAD:refs/maestro/check"); err != nil {
		return err
	}
	rp := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/maestro/check^{commit}")
	rp.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := rp.Output()
	if err != nil {
		return fmt.Errorf("git rev-parse: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != head {
		return fmt.Errorf("the bundle holds %s, not the exported %s", got, head)
	}
	if err := git(ctx, repo, "checkout", "--quiet", "--force", "--detach", head); err != nil {
		return err
	}
	return git(ctx, repo, "clean", "-ffdxq", "-e", "/Library/", "-e", "/Temp/", "-e", "/Logs/", "-e", "/UserSettings/", "-e", "/obj/")
}

// shims lets the pack's scripts, written for the Linux Editor in the
// image, run the Editor of this Mac unchanged: env.sh finds "Unity" and
// "version" under UNITY_PATH, xvfb-run only runs the rest of its command,
// and the home directory has a sign that a licence is there (licence() in
// env.sh looks for one). Its Library is the user's, where Unity Hub keeps
// the licence the Editor uses.
func shims(dir, editor, version, home string) (shim, fakeHome string, err error) {
	shim = filepath.Join(dir, "shim")
	fakeHome = filepath.Join(dir, "home")
	files := map[string]string{
		"unity/Editor/Unity": "#!/bin/sh\nexec \"" + editor + "\" \"$@\"\n",
		"unity/version":      version + "\n",
		"bin/xvfb-run": `#!/bin/sh
# There is no X server on a Mac; the Editor needs none in batch mode.
while [ $# -gt 0 ]; do
    case $1 in
        -a|--auto-servernum) shift ;;
        -s|--server-args|-n|--server-num|-f|--auth-file|-e|--error-file|-p|--xauth-protocol) shift 2 ;;
        --*=*) shift ;;
        *) break ;;
    esac
done
exec "$@"
`,
	}
	for name, text := range files {
		p := filepath.Join(shim, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(p, []byte(text), 0o700); err != nil {
			return "", "", err
		}
	}
	marker := filepath.Join(fakeHome, ".config", "unity3d", "Unity", "licenses", "unity-hub")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(marker, []byte("The Editor uses the licence of Unity Hub, in ~/Library/Unity/licenses.\n"), 0o600); err != nil {
		return "", "", err
	}
	lib := filepath.Join(fakeHome, "Library")
	if t, err := os.Readlink(lib); err != nil || t != filepath.Join(home, "Library") {
		os.RemoveAll(lib)
		if err := os.Symlink(filepath.Join(home, "Library"), lib); err != nil {
			return "", "", err
		}
	}
	return shim, fakeHome, nil
}

// Profile is the sandbox-exec profile of a check in dir.
func Profile(dir, home string, secret []string) (string, error) {
	for _, p := range append([]string{dir, home}, secret...) {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\"\\\n") {
			return "", fmt.Errorf("the path %q cannot be put in a sandbox profile", p)
		}
	}
	// The sandbox sees resolved paths: /tmp is /private/tmp, and a folder
	// may be reached through a link.
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	q := func(p string) string { return `"` + p + `"` }
	re := func(p string) string { return `#"^` + regexp.QuoteMeta(p) + `"` }
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("(version 1)")
	w("(allow default)")
	w(";; The network: only the loopback, where the proxy is, and local sockets.")
	w("(deny network*)")
	w(`(allow network* (local ip "localhost:*"))`)
	w(`(allow network* (remote ip "localhost:*"))`)
	w("(allow network* (remote unix-socket))")
	w(";; Not the sockets that would lead out of the fence: Docker's and the SSH agent's.")
	w(`(deny network-outbound (remote unix-socket (path-regex #"^/private/tmp/com\.apple\.launchd\.")))`)
	w(`(deny network-outbound (remote unix-socket (path-regex %s)))`, re(real(home)+"/.docker/"))
	w(`(deny network-outbound (remote unix-socket (path-literal "/private/var/run/docker.sock")))`)
	w(";; No starting of programs outside the fence through Launch Services or Apple events.")
	w(`(deny process-exec (literal "/usr/bin/open") (literal "/usr/bin/osascript"))`)
	w(";; Writes: the check's folder, the temporary folders and Unity's own folders.")
	w("(deny file-write*)")
	w("(allow file-write*")
	w(`    (subpath "/dev")`)
	w(`    (subpath "/private/tmp")`)
	w(`    (subpath "/private/var/folders")`)
	w("    (subpath %s)", q(real(dir)))
	for _, l := range []string{"Library/Unity", "Library/Caches", "Library/Logs",
		"Library/Application Support/Unity", "Library/Application Support/UnityHub", "Library/Saved Application State"} {
		w("    (subpath %s)", q(filepath.Join(real(home), l)))
	}
	w(")")
	w(";; Reads: not Maestro's keys, and not the usual places of the user's own.")
	w("(deny file-read*")
	for _, s := range secret {
		w("    (subpath %s)", q(real(s)))
	}
	for _, s := range []string{".ssh", ".aws", ".gnupg", ".docker", ".kube", ".config/gh", ".config/gcloud", ".netrc", ".git-credentials"} {
		w("    (subpath %s)", q(filepath.Join(real(home), s)))
	}
	w(")")
	w(";; The check's own folder, even when it lies below one of those.")
	w("(allow file-read* (subpath %s))", q(real(dir)))
	return b.String(), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte{}, t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	if len(t.buf) > t.max {
		return t.buf[len(t.buf)-t.max:]
	}
	return t.buf
}
