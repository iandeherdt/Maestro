package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

const conventionsUsage = `usage:
  symphony conventions                      list the rule packs
  symphony conventions show PACK            the rules of a pack and what enforces each
  symphony conventions apply PACK           write the pack's files into the checkout you are in
        [--to DIR] [--replace]
  symphony conventions enable PACK          switch the pack on in this machine's settings file
        [--file WORKFLOW.md]
  symphony conventions disable PACK         switch it off again`

// runConventions is the rule packs' command (SPEC 23).
func runConventions(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "list" {
		fmt.Fprintln(stdout, "Rule packs. Every rule in a pack is enforced by a tool, in the service's verify step and in CI.")
		fmt.Fprintln(stdout)
		for _, n := range conventions.Names() {
			p, err := conventions.Get(n)
			if err != nil {
				return report(stderr, err)
			}
			fmt.Fprintf(stdout, "  %-12s %s, %d rules\n", n, p.Title, len(p.Rules))
		}
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "See the rules of one:    symphony conventions show PACK")
		fmt.Fprintln(stdout, "Add one to a project:    symphony conventions apply PACK    (run it inside a checkout of the project)")
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, conventionsUsage)
		return 2
	}
	p, err := conventions.Get(args[1])
	if err != nil {
		return report(stderr, explain("%v", err))
	}
	switch args[0] {
	case "show":
		return showConventions(p, stdout)
	case "apply":
		return applyConventions(p, args[2:], stdout, stderr)
	case "enable", "disable":
		return switchConventions(p, args[0] == "enable", args[2:], stdout, stderr)
	}
	fmt.Fprintln(stderr, conventionsUsage)
	return 2
}

func showConventions(p *conventions.Pack, stdout io.Writer) int {
	fmt.Fprintf(stdout, "%s: %s\n", p.Name, p.Title)
	fmt.Fprintf(stdout, "For: %s\n", p.Applies)
	where := map[string]string{"verify": "before the push", "ci": "in CI", "service": "by the service"}
	for _, r := range p.Rules {
		var places []string
		for _, w := range r.Where {
			places = append(places, where[w])
		}
		fmt.Fprintf(stdout, "\n  %s\n", r.Rule)
		fmt.Fprintf(stdout, "      enforced by: %s\n", strings.Join(r.EnforcedBy, "; "))
		fmt.Fprintf(stdout, "      checked:     %s\n", strings.Join(places, ", "))
	}
	fmt.Fprintf(stdout, "\nThe agent may not change: %s\n", strings.Join(p.Protect, ", "))
	return 0
}

func applyConventions(p *conventions.Pack, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conventions apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", ".", "the checkout of the project")
	replace := fs.Bool("replace", false, "overwrite files that exist with other content")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, conventionsUsage)
		return 2
	}
	dir, err := filepath.Abs(*to)
	if err != nil {
		return report(stderr, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return report(stderr, explain("%s is not a checkout of a Git repository.\n"+
			"Go into a checkout of the project the conventions are for, and run this again:\n"+
			"  git clone <the project's address>\n  cd <the project>\n  symphony conventions apply %s", dir, p.Name))
	}
	rep, err := p.Apply(dir, *replace)
	if err != nil {
		return report(stderr, err)
	}
	if len(rep.Conflicts) > 0 {
		fmt.Fprintln(stderr, "Nothing was written, because these files exist with other content:")
		for _, n := range rep.Conflicts {
			fmt.Fprintln(stderr, "  "+n)
		}
		fmt.Fprintln(stderr, "Look at what they hold. To let the pack's version take their place, run:")
		fmt.Fprintf(stderr, "  symphony conventions apply %s --replace\n", p.Name)
		fmt.Fprintln(stderr, "Git keeps the old version, so you can compare with `git diff` before you commit.")
		return 1
	}
	fmt.Fprintf(stdout, "The conventions pack %s is in %s.\n", p.Name, dir)
	for _, n := range rep.Written {
		fmt.Fprintln(stdout, "  wrote  "+n)
	}
	for _, n := range rep.Unchanged {
		fmt.Fprintln(stdout, "  same   "+n)
	}
	if len(rep.Ignored) > 0 {
		fmt.Fprintf(stdout, "  added to .gitignore: %s\n", strings.Join(rep.Ignored, ", "))
	}
	fmt.Fprintf(stdout, "The rules, each with the tool that enforces it, are in %s.\n", p.GuidePath())

	step := 1
	next := func(format string, a ...any) {
		fmt.Fprintf(stdout, "\n%d. "+format+"\n", append([]any{step}, a...)...)
		step++
	}
	fmt.Fprintln(stdout, "\nWhat is left, in this order:")
	if len(p.AfterApply) > 0 {
		next("Install the tools the rules use, in the project:")
		for _, c := range p.AfterApply {
			fmt.Fprintln(stdout, "     "+c)
		}
	}
	next("Check that the project passes its own rules, and fix what does not:\n     %s\n     %s", p.Prepare, p.Verify)
	next("Commit and push to the main branch yourself:\n     git add -A\n     git commit -m \"Add the %s conventions\"\n     git push\n"+
		"   The service cannot do this for you: its App may not change CI workflows, on purpose.", p.Name)
	next("On GitHub, make the check required: the repository's Settings, Branches (or Rules),\n"+
		"   protect the main branch, require status checks, and pick %s.\n"+
		"   From then on nothing can be merged that breaks a rule.", p.Check)
	next("Switch the pack on, on every machine that runs agents for this project:\n     symphony conventions enable %s", p.Name)
	return 0
}

var (
	inlineListRE = regexp.MustCompile(`(?m)^conventions:[ \t]*\[([^\]\n]*)\][ \t]*(#.*)?$`)
	anyListRE    = regexp.MustCompile(`(?m)^conventions:`)
)

// withPack returns the settings file with the pack switched on or off.
// Only the one line is touched; everything else stays as the owner wrote it.
func withPack(text, name string, on bool) (string, error) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", errors.New("the settings file does not start with a settings block (a line of three dashes)")
	}
	loc := inlineListRE.FindStringSubmatchIndex(text)
	if loc == nil {
		if anyListRE.MatchString(text) {
			return "", fmt.Errorf("the settings file has a `conventions:` entry in a form this command does not edit. "+
				"Write it as one line, for example `conventions: [%s]`", name)
		}
		if !on {
			return text, nil
		}
		nl := strings.Index(text, "\n") + 1
		return text[:nl] + "conventions: [" + name + "]\n" + text[nl:], nil
	}
	var names []string
	for _, n := range strings.Split(text[loc[2]:loc[3]], ",") {
		if n = strings.Trim(strings.TrimSpace(n), `"'`); n != "" && n != name {
			names = append(names, n)
		}
	}
	if on {
		names = append(names, name)
	}
	// loc[3] is where the list ends; what follows the bracket, a comment
	// perhaps, stays.
	return text[:loc[0]] + "conventions: [" + strings.Join(names, ", ") + "]" + text[loc[3]+1:], nil
}

func switchConventions(p *conventions.Pack, on bool, args []string, stdout, stderr io.Writer) int {
	_, explicit, ok := parse("conventions "+map[bool]string{true: "enable", false: "disable"}[on], args, stderr)
	if !ok {
		return 2
	}
	path := workflowPath(explicit)
	raw, err := os.ReadFile(path)
	if err != nil {
		return report(stderr, explain("%v\nThe settings file is %s. Run `symphony setup` if you have not yet.", err, path))
	}
	home, _ := os.UserHomeDir()
	load := func(file string) (*config.Config, error) {
		def, err := workflow.Load(file)
		if err != nil {
			return nil, err
		}
		return config.Build(def, config.Options{Env: os.Getenv, HomeDir: home})
	}
	before, err := load(path)
	if err != nil {
		return report(stderr, explain("%s is not valid as it is: %v\nFix that first.", path, err))
	}
	if slices.Contains(before.Conventions, p.Name) == on {
		fmt.Fprintf(stdout, "The conventions pack %s is already %s in %s.\n", p.Name, map[bool]string{true: "on", false: "off"}[on], path)
		return 0
	}
	text, err := withPack(string(raw), p.Name, on)
	if err != nil {
		return report(stderr, explain("%v", err))
	}
	// The new file is checked with the code the service starts with before
	// it takes the place of the old one.
	info, err := os.Stat(path)
	if err != nil {
		return report(stderr, err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(text), info.Mode().Perm()); err != nil {
		return report(stderr, err)
	}
	defer os.Remove(tmp)
	after, err := load(tmp)
	if err != nil || slices.Contains(after.Conventions, p.Name) != on {
		if err == nil {
			err = errors.New("the change did not take effect")
		}
		return report(stderr, explain("the settings file was left as it was, because the changed one is not valid: %v", err))
	}
	if err := os.Rename(tmp, path); err != nil {
		return report(stderr, err)
	}
	if !on {
		fmt.Fprintf(stdout, "The conventions pack %s is off in %s.\n", p.Name, path)
		fmt.Fprintln(stdout, "The service no longer runs its checks and no longer protects its files. A running service picks this up by itself.")
		return 0
	}
	fmt.Fprintf(stdout, "The conventions pack %s is on in %s.\n", p.Name, path)
	fmt.Fprintln(stdout, "From now on the service, for every ticket:")
	fmt.Fprintf(stdout, "  - installs the project's dependencies before the agent starts (%s)\n", p.Prepare)
	fmt.Fprintf(stdout, "  - runs the checks itself before it pushes (%s), and hands failures back to the agent\n", p.Verify)
	fmt.Fprintln(stdout, "  - refuses any change to the rule files")
	fmt.Fprintln(stdout, "A running service picks this up by itself.")
	if len(p.Image) > 0 {
		fmt.Fprintf(stdout, "\nOne step is left: the sandbox image needs %s. Build it again:\n  symphony image\n", p.ImageNote)
	}
	return 0
}
