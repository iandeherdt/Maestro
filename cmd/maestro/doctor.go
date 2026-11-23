package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/ghapp"
)

// doctor collects what `maestro doctor` finds and prints it as it goes.
type doctor struct {
	out          io.Writer
	fails, warns int
}

func (d *doctor) ok(name string, detail ...string) {
	fmt.Fprintf(d.out, "  ok    %s\n", name)
	d.more(detail)
}

func (d *doctor) warn(name string, detail ...string) {
	d.warns++
	fmt.Fprintf(d.out, "  WARN  %s\n", name)
	d.more(detail)
}

func (d *doctor) fail(name string, detail ...string) {
	d.fails++
	fmt.Fprintf(d.out, "  FAIL  %s\n", name)
	d.more(detail)
}

func (d *doctor) more(detail []string) {
	for _, block := range detail {
		for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
			if line != "" {
				fmt.Fprintf(d.out, "        %s\n", line)
			}
		}
	}
}

func (d *doctor) section(title string) { fmt.Fprintf(d.out, "\n%s\n", title) }

// errText is the text of an error meant for the terminal.
func errText(err error) string {
	if p, ok := err.(*problem); ok {
		return p.text
	}
	return err.Error()
}

// runDoctor is `maestro doctor`: everything the service needs, checked
// without starting it, and the sandbox's self-test (SPEC 11.6, 16.7). It
// changes nothing, except that it starts the proxies for the self-test
// when no service runs, and stops them again.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	dir, explicit, ok := parse("doctor", args, stderr)
	if !ok {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	d := &doctor{out: stdout}
	code := d.run(ctx, dir, explicit)
	fmt.Fprintln(stdout)
	switch {
	case d.fails > 0:
		fmt.Fprintf(stdout, "%d problem(s) stop the service; each says above what to do.\n", d.fails)
	case d.warns > 0:
		fmt.Fprintf(stdout, "Nothing stops the service. %d warning(s) above are worth a look.\n", d.warns)
	default:
		fmt.Fprintln(stdout, "Everything checks out.")
	}
	return code
}

func (d *doctor) run(ctx context.Context, dir, explicit string) int {
	d.section("Settings")
	in, err := open(dir, explicit, nil)
	if err != nil {
		d.fail("the settings and the setup can be read", errText(err))
		return 1
	}
	d.ok("the settings file " + in.path)
	for _, w := range in.cfg.Warnings {
		d.warn(w)
	}
	if p := in.wslReady(); len(p) > 0 {
		d.fail("this WSL distribution can run the service", strings.Join(p, "\n"))
	}

	d.section("Containers")
	if err := in.docker(ctx); err != nil {
		d.fail("the container engine answers", errText(err))
		return 1
	}
	d.ok("the container engine answers (" + engineName(in) + ")")
	if in.box.Podman {
		d.podman(ctx)
	}
	if err := in.image(ctx); err != nil {
		d.fail("the sandbox image is there", errText(err))
		return 1
	}
	d.ok("the sandbox image " + in.cfg.Sandbox.Image + " is there")
	if why := imageOutdated(ctx, in); why != "" {
		d.warn("the sandbox image is current", why+" Build it again with: "+mention("maestro image"))
	}

	// The self-test and the login need the proxies. A running service has
	// them up, and starting them again would stop its work.
	if !in.box.ProxyRunning(ctx, "agent") {
		if err := in.up(ctx); err != nil {
			d.fail("the proxies start", errText(err))
			return 1
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			in.box.Down(c)
		}()
	}
	d.ok("the proxies run")
	switch yes, err := in.box.LoggedIn(ctx, in.cfg.Claude.Command); {
	case err != nil:
		d.fail("Claude Code is signed in inside the sandbox", errText(err))
	case !yes:
		d.fail("Claude Code is signed in inside the sandbox", "Sign in with: "+mention("maestro login"))
	default:
		d.ok("Claude Code is signed in inside the sandbox")
	}

	d.section("Sandbox self-test")
	rep, err := in.box.SelfTest(ctx)
	if err != nil {
		d.fail("the self-test runs", errText(err))
	} else {
		d.selfTest(rep)
	}

	d.section("GitHub")
	d.github(ctx, in)

	if slices.Contains(in.cfg.Conventions, "unity") {
		d.section("Unity")
		d.unityPackages(ctx, in)
	}

	d.section("Service")
	d.service(ctx, in)
	if d.fails > 0 {
		return 1
	}
	return 0
}

func engineName(in *install) string {
	if in.box.Podman {
		return "podman"
	}
	return "docker"
}

// selfTest prints the self-test's checks, grouped by network profile.
func (d *doctor) selfTest(rep dockerbox.SelfTestReport) {
	if rep.OldImage {
		d.fail("the sandbox image can run the self-test",
			"It was built by an older Maestro. Build it again with: "+mention("maestro image"))
		return
	}
	labels := map[string]string{"none": "a step without network", "hooks": "a hook or a check", "agent": "the agent"}
	for _, c := range rep.Checks {
		name := labels[c.Profile] + ": " + c.Name
		switch {
		case c.OK:
			d.ok(name)
		case c.Advisory:
			d.warn(name, c.Detail+selfTestAdvice(c))
		default:
			d.fail(name, c.Detail+"\nThe service does not start while this fails.")
		}
	}
}

// selfTestAdvice says what to do about an advisory check.
func selfTestAdvice(c dockerbox.ProbeCheck) string {
	switch {
	case strings.Contains(c.Name, "limit"):
		return "\nA runaway step can then use all of the machine. See the cgroups notes under Containers."
	case strings.Contains(c.Name, "look up names"):
		return "\nThe container engine answers names on the sandbox's network. Nothing can connect out directly,\n" +
			"but a name lookup can carry a little data out."
	case strings.Contains(c.Name, "reach Claude"):
		return "\nThe agent cannot work like this. Check this machine's internet connection, and that the agent proxy runs."
	}
	return ""
}

// podman looks at what made Podman setups go wrong: the CNI network
// backend (checked by docker()), cgroups v1 and missing controllers.
func (d *doctor) podman(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "podman", "info", "--format",
		"{{.Host.Security.Rootless}} {{.Host.CgroupsVersion}} {{.Host.NetworkBackend}}").Output()
	if err != nil {
		d.warn("Podman's facts can be read", "podman info failed: "+err.Error())
		return
	}
	f := strings.Fields(string(out))
	if len(f) < 3 {
		d.warn("Podman's facts can be read", "podman info said: "+strings.TrimSpace(string(out)))
		return
	}
	d.ok("Podman uses the " + f[2] + " network backend")
	controllers := ""
	if raw, err := os.ReadFile(delegatedControllers(os.Getuid())); err == nil {
		controllers = string(raw)
	}
	if advice := cgroupAdvice(f[0] == "true", f[1], controllers); advice != "" {
		d.warn("the sandbox's memory, CPU and process limits can be applied", advice)
	} else {
		d.ok("cgroups " + f[1] + " can apply the sandbox's limits")
	}
}

// delegatedControllers is the file that lists the cgroup controllers
// systemd gives the user's own services, which rootless Podman uses.
func delegatedControllers(uid int) string {
	u := strconv.Itoa(uid)
	return "/sys/fs/cgroup/user.slice/user-" + u + ".slice/user@" + u + ".service/cgroup.controllers"
}

// cgroupAdvice says how to make the limits work, or "" when they do.
func cgroupAdvice(rootless bool, version, controllers string) string {
	if !rootless {
		return ""
	}
	if version == "v1" {
		return "This machine uses cgroups v1. Rootless Podman cannot set limits there and ignores them,\n" +
			"so a runaway step can use all memory and processes. Switch to cgroups v2 (RHEL 8):\n" +
			"  sudo grubby --update-kernel=ALL --args=systemd.unified_cgroup_hierarchy=1\n" +
			"  sudo reboot\n" +
			"Then `podman info --format '{{.Host.CgroupsVersion}}'` says v2. Images and volumes are kept."
	}
	if controllers == "" {
		return ""
	}
	have := strings.Fields(controllers)
	var missing []string
	for _, c := range []string{"memory", "pids", "cpu"} {
		if !slices.Contains(have, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "systemd does not give your user the cgroup controllers " + strings.Join(missing, ", ") + ", so rootless Podman\n" +
		"cannot apply those limits. Give them, once:\n" +
		"  sudo mkdir -p /etc/systemd/system/user@.service.d\n" +
		"  printf '[Service]\\nDelegate=memory pids cpu cpuset io\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf\n" +
		"  sudo systemctl daemon-reload\n" +
		"Then log out and in again (or reboot)."
}

// imageOutdated says why the image does not fit the settings, or "".
func imageOutdated(ctx context.Context, in *install) string {
	have, ok := in.box.ImageLabel(ctx, imagePacksLabel)
	if !ok {
		return "The image carries no list of its packs; it was built by an older Maestro."
	}
	got := strings.Split(have, ",")
	for _, n := range in.cfg.Conventions {
		if !slices.Contains(got, n) {
			return "The settings turn on the pack " + n + ", whose tools the image does not have."
		}
	}
	packs, err := conventions.Resolve(in.cfg.Conventions)
	if err != nil {
		return ""
	}
	extra := imageExtra(packs, len(in.cfg.Screenshots.Pages) > 0 || in.cfg.Tracker.DesignState != "")
	if lines, _ := in.box.ImageLabel(ctx, imageLinesLabel); lines != linesHash(extra) {
		return "This version of Maestro puts more in the image."
	}
	return ""
}

// github checks the App's token and the rule on the base branch.
func (d *doctor) github(ctx context.Context, in *install) {
	app, err := ghapp.New(ghapp.Config{AppID: in.state.AppID, InstallationID: in.state.InstallationID,
		KeyFile: in.state.AppKeyFile, APIURL: gitHub.api})
	if err != nil {
		d.fail("the GitHub App's key can be read", err.Error())
		return
	}
	_, name, _ := strings.Cut(in.cfg.Workspace.Repo, "/")
	src, err := app.Source(ghapp.Scope{Repositories: []string{name}, Permissions: map[string]string{"contents": "read"}})
	if err != nil {
		d.fail("the GitHub App can be used", err.Error())
		return
	}
	token, err := src.Token(ctx)
	if err != nil {
		d.fail("GitHub gives the App a token for "+in.cfg.Workspace.Repo, err.Error()+"\nRun `maestro setup` to check the App and its installation.")
		return
	}
	d.ok("GitHub gives the App a token for " + in.cfg.Workspace.Repo)
	base := in.cfg.Workspace.BaseBranch
	if base == "" {
		if base, err = defaultBranch(ctx, gitHub.api, in.cfg.Workspace.Repo, token); err != nil {
			d.fail("the repository can be read", err.Error())
			return
		}
	}
	switch guarded, err := baseGuarded(ctx, gitHub.api, in.cfg.Workspace.Repo, base, token); {
	case err != nil:
		d.warn("GitHub requires a pull request for "+base, "This could not be read: "+err.Error())
	case !guarded:
		d.warn("GitHub requires a pull request for "+base, strings.TrimPrefix(unguardedNote(gitHub.git, in.cfg.Workspace.Repo, base), "\n"))
	default:
		d.ok("GitHub requires a pull request for " + base)
	}
}

// service says whether the service runs, and how it is kept running.
func (d *doctor) service(ctx context.Context, in *install) {
	if alive(ctx, in.cfg.Budget.StateDir) {
		d.ok("the service is running")
	} else {
		d.warn("the service is running", "It is not. Start it with "+mention("maestro run")+" or "+mention("maestro service install")+".")
	}
	u, err := unitFor(in)
	if err != nil || !u.Installed() {
		return
	}
	d.ok("the background service is installed: " + u.Path())
	if runtime.GOOS != "linux" || inWSL() {
		return
	}
	user := os.Getenv("USER")
	if user == "" {
		return
	}
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", user, "--property=Linger").Output()
	if err == nil && strings.TrimSpace(string(out)) == "Linger=no" {
		d.warn("the service keeps running when you log out",
			"systemd stops your services when you log out. Keep them running with:\n  sudo loginctl enable-linger "+user)
	}
}

// gitPackages lists the packages of a Unity manifest that Unity fetches
// with git, and the host of each.
func gitPackages(manifest []byte) (map[string]string, error) {
	var m struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, v := range m.Dependencies {
		if host := gitHost(v); host != "" {
			out[name] = host
		}
	}
	return out, nil
}

// gitHost is the host of a git package reference, such as
// "https://github.com/o/r.git?path=/p#v1" or "git@github.com:o/r.git".
func gitHost(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, "git@"); ok {
		host, _, _ := strings.Cut(rest, ":")
		return strings.ToLower(host)
	}
	for _, p := range []string{"git+https://", "git+ssh://", "https://", "http://", "ssh://", "git://"} {
		if rest, ok := strings.CutPrefix(v, p); ok {
			host, _, _ := strings.Cut(rest, "/")
			if i := strings.LastIndex(host, "@"); i >= 0 {
				host = host[i+1:]
			}
			host, _, _ = strings.Cut(host, ":")
			return strings.ToLower(host)
		}
	}
	return ""
}

// unityPackages warns about packages the checks cannot fetch: Unity
// clones a git package itself, and the checks reach only the hosts on
// their list.
func (d *doctor) unityPackages(ctx context.Context, in *install) {
	ref := "HEAD"
	if in.cfg.Workspace.BaseBranch != "" {
		ref = in.cfg.Workspace.BaseBranch
	}
	raw, err := exec.CommandContext(ctx, "git", "--git-dir", in.mirrorDir, "show", ref+":Packages/manifest.json").Output()
	if err != nil {
		return
	}
	pkgs, err := gitPackages(raw)
	if err != nil {
		d.warn("Packages/manifest.json can be read", err.Error())
		return
	}
	names := make([]string, 0, len(pkgs))
	for n := range pkgs {
		names = append(names, n)
	}
	slices.Sort(names)
	blocked := false
	for _, n := range names {
		host := pkgs[n]
		if proxyAllows(in.cfg.Sandbox.AllowHooks, host) {
			d.ok("Unity can fetch the package " + n + " from " + host)
			continue
		}
		blocked = true
		d.warn("Unity can fetch the package "+n,
			"It comes from "+host+" with git, and the checks may not reach "+host+", so Unity's checks fail.\n"+
				"The safer way is to embed it: copy the package's folder into Packages/ and commit it, then\n"+
				"remove its line from Packages/manifest.json. The other way is to add "+host+" to\n"+
				"sandbox.network.allow_hooks; code the agent wrote can then reach "+host+" during the checks,\n"+
				"which have the Unity licence.")
	}
	if !blocked && len(names) == 0 {
		d.ok("Unity's packages come from Unity's own registry")
	}
}

// proxyAllows is the proxy's rule: an exact name, or "*.domain" for the
// names below it.
func proxyAllows(list []string, host string) bool {
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == host {
			return true
		}
		if rest, ok := strings.CutPrefix(e, "*."); ok && strings.HasSuffix(host, "."+rest) {
			return true
		}
	}
	return false
}
