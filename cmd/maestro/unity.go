package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/unity"
)

const unityUsage = `usage:
  maestro unity                    whether this machine has a Unity licence for the checks
  maestro unity activate          sign in to Unity in your browser and activate a Personal licence
                                   (two-step verification works)
  maestro unity activate --password [--serial]
                                   activate with the account's email and password instead (no
                                   two-step verification), with --serial for a Plus or Pro licence
  maestro unity activate --file Unity_lic.ulf
                                   import a licence file
  maestro unity remove             delete the licence from this machine

The licence is for the conventions pack unity. Unity needs one to open a
project and run its tests. The licence is given read-only to the steps that
run the project's checks, never to the agent's own container, and your
password is not kept. The checks run code the agent wrote, so that code can
read the licence: use a Unity account that holds nothing else.`

func unityStore(dir string) unity.Store {
	return unity.Store{Dir: filepath.Join(dir, "unity-licence")}
}

// runUnity is `maestro unity`.
func runUnity(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "where setup keeps its state")
	wf := fs.String("file-settings", "", "the settings file")
	file := fs.String("file", "", "a licence file activated elsewhere")
	serial := fs.Bool("serial", false, "ask for the serial of a Plus or Pro licence")
	password := fs.Bool("password", false, "activate with email and password instead of the browser")
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			fmt.Fprintln(stderr, unityUsage)
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
	setupDir := *dir
	if setupDir == "" {
		found, err := configDir()
		if err != nil {
			fmt.Fprintln(stderr, "no configuration directory was found for this user, pass --dir:", err)
			return 1
		}
		setupDir = found
	}
	setupDir, err := filepath.Abs(setupDir)
	if err != nil {
		return report(stderr, err)
	}
	store := unityStore(setupDir)
	switch {
	case len(words) == 0 || len(words) == 1 && words[0] == "status":
		return unityStatus(store, stdout)
	case len(words) == 1 && words[0] == "remove":
		if err := store.Remove(); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "The Unity licence is deleted from this machine. A Plus or Pro seat is still taken;")
		fmt.Fprintln(stdout, "return it in your Unity account (id.unity.com, Seats) if you want it back.")
		return 0
	case len(words) == 1 && words[0] == "activate" && *file != "":
		if err := store.Import(*file); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "The licence file is imported. The checks of a Unity project get it from the next run on.")
		return 0
	case len(words) == 1 && words[0] == "activate":
		mode := activateBrowser
		if *serial {
			mode = activateSerial
		} else if *password {
			mode = activatePassword
		}
		return unityActivate(setupDir, *wf, store, mode, stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, unityUsage)
	return 2
}

func unityStatus(store unity.Store, stdout io.Writer) int {
	files := store.Files()
	if len(files) == 0 {
		fmt.Fprintln(stdout, "This machine has no Unity licence. Get one with: maestro unity activate")
		return 0
	}
	fmt.Fprintf(stdout, "This machine has a Unity licence in %s:\n", store.Dir)
	for _, f := range files {
		fmt.Fprintln(stdout, "  "+f)
	}
	fmt.Fprintln(stdout, "The checks of a Unity project get it read-only. The agent's own container does not.")
	return 0
}

// How `maestro unity activate` gets the licence.
const (
	activateBrowser  = iota // Unity's sign-in in a browser
	activatePassword        // the account's email and password
	activateSerial          // the same, with a Plus or Pro serial
)

func unityActivate(dir, settings string, store unity.Store, mode int, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := interruptible()
	defer stop()
	in, err := open(dir, settings, nil)
	if err == nil {
		err = in.docker(ctx)
	}
	if err == nil {
		err = in.image(ctx)
	}
	if err != nil {
		return report(stderr, err)
	}
	if !slices.Contains(in.cfg.Conventions, "unity") {
		fmt.Fprintln(stderr, "The conventions pack unity is not switched on, so the sandbox image has no Unity Editor.")
		fmt.Fprintln(stderr, "First: maestro conventions enable unity, then maestro image.")
		return 1
	}
	if mode == activateBrowser {
		fmt.Fprintln(stdout, "Unity's own sign-in now runs once, in a container of the sandbox image that holds")
		fmt.Fprintln(stdout, "nothing of your repository. You sign in in your browser, so Maestro never sees your")
		fmt.Fprintln(stdout, "password; the licence Unity gives back is kept on this machine. The checks run code the")
		fmt.Fprintln(stdout, "agent wrote and can read the licence.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Starting the container and installing Unity's command-line tool in it. That takes a")
		fmt.Fprintln(stdout, "minute or two, longer on a Mac with Apple silicon; then a sign-in link appears here.")
		files, err := in.dbox.SignInUnity(ctx, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "If the sign-in does not work, `maestro unity activate --password` uses the email and")
			fmt.Fprintln(stderr, "password of an account without two-step verification instead.")
			return report(stderr, err)
		}
		return saveLicence(store, files, stdout, stderr)
	}
	fmt.Fprintln(stdout, "Unity's own activation now runs once, in a container of the sandbox image that holds")
	fmt.Fprintln(stdout, "nothing of your repository. Your password goes to Unity and is not kept; the licence it")
	fmt.Fprintln(stdout, "gives back is kept on this machine. The checks run code the agent wrote and can read the")
	fmt.Fprintln(stdout, "licence, so use a Unity account that holds nothing else. An account with two-step")
	fmt.Fprintln(stdout, "verification cannot be activated this way; use `maestro unity activate` instead.")
	fmt.Fprintln(stdout)
	r := bufio.NewReader(stdin)
	var acct dockerbox.Account
	if acct.Email, err = ask(r, stdout, "Email address of the Unity account: "); err == nil {
		acct.Password, err = askSecret(r, stdin, stdout, "Password (not shown): ")
	}
	if err == nil && mode == activateSerial {
		acct.Serial, err = askSecret(r, stdin, stdout, "Serial of the Plus or Pro licence (not shown): ")
	}
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout, "Activating. This takes a minute.")
	files, err := in.dbox.ActivateUnity(ctx, acct, stderr)
	if err != nil {
		return report(stderr, err)
	}
	return saveLicence(store, files, stdout, stderr)
}

func saveLicence(store unity.Store, files map[string][]byte, stdout, stderr io.Writer) int {
	if err := store.Save(files); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout, "Done. The checks of a Unity project get the licence from the next run on.")
	fmt.Fprintln(stdout, "A service that is running picks it up after: maestro service install, or a restart of maestro run.")
	return 0
}

func ask(r *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", explain("no answer was given")
	}
	return strings.TrimSpace(line), nil
}

// askSecret reads a line without echo when the input is a terminal.
func askSecret(r *bufio.Reader, in io.Reader, out io.Writer, prompt string) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) && r.Buffered() == 0 {
		fmt.Fprint(out, prompt)
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	return ask(r, out, prompt)
}
