// Package hostcheck runs the unity pack's checks on this machine instead of
// in the sandbox, with the Unity Editor that Unity Hub installed here.
//
// Unity's Editor for Linux crashes when it runs emulated, which is the only
// way it runs on a Mac with Apple silicon. So on such a Mac the service
// checks out exactly what the export step produced into a folder of its
// own and runs the pack's verify script there, inside macOS's sandbox
// (sandbox-exec): no network but the loopback, where a proxy lets through
// only Unity's hosts and the allowlist of the hooks; no writes outside the
// check's folder and Unity's own folders; no reads of the home folder
// outside Unity's part of its Library; and no starting or steering of
// other apps. It is a weaker fence than the container: the code the agent
// wrote runs as the user who runs Maestro, and what the profile does not
// name is allowed.
package hostcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	res, _, err := r.run(ctx, editor, key, head, script, 0)
	return res, err
}

// Picture is a PNG file that a screenshot command left. Data is empty when
// the file was larger than the limit.
type Picture struct {
	Name   string
	Data   []byte
	TooBig bool
}

// ShotDir is the folder, in a check's own folder, where a screenshot
// command run on this machine writes its pictures ($SCREENSHOT_DIR).
const ShotDir = "screenshots"

// Shots runs command, the project's screenshots command, against the
// commit head of a workspace, with editor, in the same fence as the
// checks, and returns the PNG files it wrote into $SCREENSHOT_DIR, in the
// order of their names. A file over maxBytes comes back without its data.
// The pictures are what the code the agent wrote produced: the caller
// decodes and draws them again before they go anywhere.
func (r *Runner) Shots(ctx context.Context, editor, key, head, command string, maxBytes int) (Result, []Picture, error) {
	if maxBytes <= 0 {
		return Result{}, nil, errors.New("no size limit for the pictures")
	}
	return r.run(ctx, editor, key, head, command, maxBytes)
}

// run runs script in a check's folder. With shots above 0 it also gives
// the script $SCREENSHOT_DIR and reads the pictures back, each up to shots
// bytes.
func (r *Runner) run(ctx context.Context, editor, key, head, script string, shots int) (Result, []Picture, error) {
	switch {
	case !keyRE.MatchString(key) || strings.Contains(key, ".."):
		return Result{}, nil, errors.New("invalid workspace key")
	case !shaRE.MatchString(head):
		return Result{}, nil, errors.New("invalid commit")
	case !filepath.IsAbs(editor) || strings.ContainsAny(editor, "\"$`\\\n"):
		return Result{}, nil, errors.New("unity.editor is not a full path")
	}
	if st, err := os.Stat(editor); err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
		return Result{}, nil, fmt.Errorf("the Unity Editor %s is not there. Install it with Unity Hub, "+
			"or run `maestro new` again to find the one this project needs", editor)
	}
	version := EditorVersion(editor)
	if version == "" {
		return Result{}, nil, fmt.Errorf("the Unity version cannot be told from the path %s; unity.editor must be an Editor that Unity Hub installed", editor)
	}
	home, err := r.homeDir()
	if err != nil {
		return Result{}, nil, err
	}
	if !HubLicence(home) {
		return Result{}, nil, errors.New("Unity Hub has no licence on this Mac. Open Unity Hub, sign in, " +
			"and add a licence (Preferences, Licenses, Add, Get a free personal licence)")
	}

	// Two folders per workspace. The check may write only in work. What
	// the service itself runs or reads again later (git's own folder, the
	// shims, the profile) is in service, which the check can read but not
	// change: a hook or a link planted there would act outside the fence.
	top := filepath.Join(r.WorkDir, key)
	dir, service := filepath.Join(top, "work"), filepath.Join(top, "service")
	repo := filepath.Join(dir, "repo")
	if err := layout(top, dir, service); err != nil {
		return Result{}, nil, err
	}
	if err := r.checkout(ctx, filepath.Join(service, "git"), repo, key, head); err != nil {
		return Result{}, nil, fmt.Errorf("the change could not be checked out for the Unity checks: %w", err)
	}
	shim, fakeHome, err := shims(service, dir, editor, version, home, shots == 0)
	if err != nil {
		return Result{}, nil, err
	}
	shotDir := filepath.Join(dir, ShotDir)
	if shots > 0 {
		// Emptied first: a check before this one could have left files.
		if err := os.RemoveAll(shotDir); err != nil {
			return Result{}, nil, err
		}
		if err := os.MkdirAll(shotDir, 0o700); err != nil {
			return Result{}, nil, err
		}
		defer os.RemoveAll(shotDir)
	}

	// The proxy is the only way out. It listens on the loopback, which is
	// all the sandbox lets through.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Result{}, nil, err
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
		profile := filepath.Join(service, "sandbox.sb")
		// An Editor below the home folder has to stay readable.
		root := editor
		if i := strings.Index(filepath.ToSlash(editor), "/Editor/"+version+"/"); i >= 0 {
			root = editor[:i+len("/Editor/"+version)]
		}
		text, err := Profile(dir, home, r.Secret, []string{root}, service)
		if err != nil {
			return Result{}, nil, err
		}
		if err := os.WriteFile(profile, []byte(text), 0o600); err != nil {
			return Result{}, nil, err
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
	if shots > 0 {
		cmd.Env = append(cmd.Env, "SCREENSHOT_DIR="+shotDir)
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
		res := Result{Output: out.Bytes()}
		if shots > 0 {
			pics, err := pictures(shotDir, shots)
			return res, pics, err
		}
		return res, nil, nil
	case ctx.Err() != nil:
		return Result{ExitStatus: -1, Output: out.Bytes()}, nil, ctx.Err()
	case errors.As(err, &exit):
		return Result{ExitStatus: exit.ExitCode(), Output: out.Bytes()}, nil, nil
	}
	return Result{}, nil, err
}

// maxPictures bounds how many files are read back.
const maxPictures = 32

// pictures reads the PNG files in dir, by name. Only plain files with one
// name are read: a link, or a second name for a file elsewhere, could
// lead the service to a file the check itself may not read.
func pictures(dir string, max int) ([]Picture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Picture
	for _, e := range entries { // sorted by name
		if len(out) == maxPictures {
			break
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".png") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || links(st) != 1 {
			f.Close()
			continue
		}
		if st.Size() > int64(max) {
			f.Close()
			out = append(out, Picture{Name: name, TooBig: true})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
		f.Close()
		if err != nil {
			continue
		}
		if len(data) > max {
			out = append(out, Picture{Name: name, TooBig: true})
			continue
		}
		out = append(out, Picture{Name: name, Data: data})
	}
	return out, nil
}

// links is the number of names of a file; 1 when it cannot be told.
func links(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Nlink)
	}
	return 1
}

// Forget removes the folder of a workspace that was removed.
func (r *Runner) Forget(key string) {
	if keyRE.MatchString(key) && !strings.Contains(key, "..") && r.WorkDir != "" {
		os.RemoveAll(filepath.Join(r.WorkDir, key))
	}
}

// layout makes the two folders of a workspace's checks. A folder from
// before they were apart is taken along, without the git folder, the shims
// and the profile that a check could have changed.
func layout(top, dir, service string) error {
	if st, err := os.Lstat(dir); err == nil && !st.IsDir() {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if old := filepath.Join(top, "repo"); isDir(old) {
		os.RemoveAll(filepath.Join(old, ".git"))
		if err := os.Rename(old, filepath.Join(dir, "repo")); err != nil {
			os.RemoveAll(old)
		}
	}
	for _, name := range []string{"shim", "home", "sandbox.sb"} {
		os.RemoveAll(filepath.Join(top, name))
	}
	if st, err := os.Lstat(service); err == nil && !st.IsDir() {
		if err := os.RemoveAll(service); err != nil {
			return err
		}
	}
	return os.MkdirAll(service, 0o700)
}

// isDir reports whether path is a folder itself, not a link to one.
func isDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir()
}

// gitEnv is git as the service, without the user's or the system's git
// settings: the tree comes from the agent, and a filter or hook in the
// user's settings must not run on it.
func gitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

// gitArgs names git's own folder and the working tree apart, so that
// nothing in the tree is git's, and switches off what would start a
// program: hooks and the file system monitor.
func gitArgs(gitDir, tree string, args ...string) []string {
	return append([]string{"--git-dir=" + gitDir, "--work-tree=" + tree,
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
}

func git(ctx context.Context, gitDir, tree string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", gitArgs(gitDir, tree, args...)...)
	cmd.Dir = tree
	cmd.Env = gitEnv()
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

// checkout puts the exported commit head into repo. git's own folder is
// gitDir, outside what a check may write. The folders Unity makes (Library
// and the like) stay, so the next check is fast.
func (r *Runner) checkout(ctx context.Context, gitDir, repo, key, head string) error {
	bundle := filepath.Join(r.HandoffDir, key+".bundle")
	st, err := os.Lstat(bundle)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("the bundle is not a regular file")
	}
	if !isDir(repo) {
		os.RemoveAll(repo)
		if err := os.MkdirAll(repo, 0o700); err != nil {
			return err
		}
	}
	// git's folder is gitDir. One that a check made in the tree is not
	// used, and not kept: git's own clean leaves it.
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		os.RemoveAll(gitDir)
		init := exec.CommandContext(ctx, "git", "init", "--quiet", "--bare", gitDir)
		init.Env = gitEnv()
		if out, err := init.CombinedOutput(); err != nil {
			return fmt.Errorf("git init: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// The base the bundle builds on comes from the mirror.
	if err := git(ctx, gitDir, repo, "fetch", "--quiet", "--no-tags", r.MirrorDir, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return err
	}
	if err := git(ctx, gitDir, repo, "-c", "protocol.file.allow=always", "fetch", "--quiet", "--no-tags", bundle, "+HEAD:refs/maestro/check"); err != nil {
		return err
	}
	rp := exec.CommandContext(ctx, "git", gitArgs(gitDir, repo, "rev-parse", "--verify", "--quiet", "refs/maestro/check^{commit}")...)
	rp.Env = gitEnv()
	out, err := rp.Output()
	if err != nil {
		return fmt.Errorf("git rev-parse: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != head {
		return fmt.Errorf("the bundle holds %s, not the exported %s", got, head)
	}
	if err := git(ctx, gitDir, repo, "checkout", "--quiet", "--force", "--detach", head); err != nil {
		return err
	}
	return git(ctx, gitDir, repo, "clean", "-ffdxq", "-e", "/Library/", "-e", "/Temp/", "-e", "/Logs/", "-e", "/UserSettings/", "-e", "/obj/")
}

// shims lets the pack's scripts, written for the Linux Editor in the
// image, run the Editor of this Mac unchanged: env.sh finds "Unity" and
// "version" under UNITY_PATH, xvfb-run (not for pictures) only runs the
// rest of its command,
// and the home directory has a sign that a licence is there (licence() in
// env.sh looks for one). Its Library is the user's, where Unity Hub keeps
// the licence the Editor uses.
//
// The shims are in service, which a check cannot write, and are made anew
// each time. The home directory is in dir, because programs write there;
// what the service itself writes in it is made anew too, so that a link a
// check left cannot lead the write elsewhere.
func shims(service, dir, editor, version, home string, xvfb bool) (shim, fakeHome string, err error) {
	shim = filepath.Join(service, "shim")
	fakeHome = filepath.Join(dir, "home")
	if err := os.RemoveAll(shim); err != nil {
		return "", "", err
	}
	if !isDir(fakeHome) {
		os.RemoveAll(fakeHome)
	}
	if err := os.RemoveAll(filepath.Join(fakeHome, ".config")); err != nil {
		return "", "", err
	}
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
	if !xvfb {
		// The pictures need the Mac's own graphics. Without an xvfb-run,
		// env.sh of every version starts the Editor the Mac's way; with
		// one, an older env.sh asks for OpenGL, which Unity on a Mac no
		// longer has.
		delete(files, "bin/xvfb-run")
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
//
// It starts from "allow" and takes away: the network, the writes and the
// reads of the home folder, each given back only where the check and Unity
// need it. read are folders outside those that the check has to read, such
// as an Editor that Unity Hub installed below the home folder.
//
// own are folders of the check that it reads but may not change, even when
// they lie below a secret folder: where the service keeps the shims, the
// profile and git's own folder.
func Profile(dir, home string, secret, read []string, own ...string) (string, error) {
	for _, p := range append(append(append([]string{dir, home}, secret...), read...), own...) {
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
	// re is a regular expression for paths that start with p. more is
	// appended to it as it is.
	re := func(p string, more ...string) string {
		return `#"^` + regexp.QuoteMeta(p) + strings.Join(more, "") + `"`
	}
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
	w(";; No starting of programs outside the fence, and no steering of the user's apps:")
	w(";; not by name, not through Launch Services and not with Apple events.")
	w(`(deny process-exec (literal "/usr/bin/open") (literal "/usr/bin/osascript"))`)
	w("(deny lsopen)")
	w("(deny appleevent-send)")
	lib := filepath.Join(real(home), "Library")
	w(";; Writes: the check's folder, the temporary folders and Unity's own folders. Not the")
	w(";; caches and settings of other programs, which they trust: a build cache that was")
	w(";; written here would run as the user the next time they build. Not Unity Hub's settings,")
	w(";; which say where the Editors are.")
	w("(deny file-write*)")
	w("(allow file-write*")
	w(`    (subpath "/dev")`)
	w(`    (subpath "/private/tmp")`)
	w(`    (subpath "/private/var/folders")`)
	w("    (subpath %s)", q(real(dir)))
	for _, l := range []string{"Unity", "Logs/Unity", "Application Support/Unity", "Caches/Unity"} {
		w("    (subpath %s)", q(filepath.Join(lib, l)))
	}
	for _, l := range []string{"Caches", "Saved Application State", "Application Support"} {
		w("    (regex %s)", re(filepath.Join(lib, l)+"/", `[^/]*[Uu]nity3d`))
	}
	w(")")
	w(";; Reads: nothing of the home folder but its Library, where Unity keeps its licence, its")
	w(";; packages and its settings. Not the user's documents, projects and dot files.")
	w("(deny file-read* (subpath %s))", q(real(home)))
	w("(allow file-read-metadata (literal %s))", q(real(home)))
	w("(allow file-read* (subpath %s))", q(lib))
	w(";; Of the Library, not what other programs keep there about the user.")
	private := []string{"Accounts", "Application Support", "Caches", "Calendars", "CloudStorage", "Containers", "Cookies",
		"Group Containers", "HTTPStorages", "IdentityServices", "Mail", "Messages", "Mobile Documents", "Safari"}
	w("(deny file-read*")
	for _, l := range private {
		w("    (subpath %s)", q(filepath.Join(lib, l)))
	}
	w(")")
	w(";; Those folders can still be passed through, and Unity's own in them be read.")
	w("(allow file-read-metadata")
	for _, l := range private {
		w("    (literal %s)", q(filepath.Join(lib, l)))
	}
	w(")")
	w("(allow file-read*")
	for _, l := range []string{"Unity", "UnityHub"} {
		w("    (subpath %s)", q(filepath.Join(lib, "Application Support", l)))
	}
	// The Package Manager keeps its cache here and does not start without it.
	w("    (subpath %s)", q(filepath.Join(lib, "Caches", "Unity")))
	w("    (regex %s)", re(lib+"/", `[^/]+/[^/]*[Uu]nity3d`))
	for _, r := range read {
		w("    (subpath %s)", q(real(r)))
	}
	w(")")
	w(";; Not Maestro's keys, wherever they are.")
	if len(secret) > 0 {
		w("(deny file-read*")
		for _, s := range secret {
			w("    (subpath %s)", q(real(s)))
		}
		w(")")
	}
	w(";; The check's own folder, even when it lies below one of those.")
	w("(allow file-read* (subpath %s))", q(real(dir)))
	for _, o := range own {
		w(";; What the service keeps for the check: read, never written.")
		w("(allow file-read* (subpath %s))", q(real(o)))
		w("(deny file-write* (subpath %s))", q(real(o)))
	}
	// A path is looked up from the root, so the folders above the check's
	// own have to be passed through. Without this every full path in it is
	// refused: the shell's `cd x/..` says "Not a directory", and Unity cannot
	// open the project by its path.
	// The same holds for the folders that are read, such as an Editor below
	// the home folder: .NET looks up its own full path, and without its
	// parents Unity's compiler stops with "Failed to resolve full path of
	// the current executable".
	w(";; The folders above it and above what is read can be passed through, not listed or read.")
	w("(allow file-read-metadata")
	seen := map[string]bool{}
	for _, p := range append(append([]string{dir}, read...), own...) {
		for d := filepath.Dir(real(p)); !seen[d]; d = filepath.Dir(d) {
			seen[d] = true
			w("    (literal %s)", q(d))
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	w(")")
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
