package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/skills"
)

const skillsUsage = `usage:
  maestro skills                          the skills and agent files the agents have
  maestro skills add SOURCE [--ref REF] [--path DIR] [--yes]
                                          show what SOURCE holds, ask, then add it
  maestro skills remove NAME              take one away again

SOURCE is one of:
  owner/repo            a repository on GitHub, optionally owner/repo/some/dir
  https://...           another git repository
  ./dir, /dir, ~/dir    a directory on this machine

A skill is a directory with a SKILL.md. An agent file is a Markdown file in a
directory called agents. Both are the formats Claude Code uses.`

// skillFlags reads flags that may stand before or after the words.
func skillFlags(args []string, stderr io.Writer) (dir, ref, sub string, yes bool, words []string, ok bool) {
	fs := flag.NewFlagSet("skills", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dir, "dir", "", "where setup keeps its state")
	fs.StringVar(&ref, "ref", "", "a branch, tag or commit")
	fs.StringVar(&sub, "path", "", "a directory inside the source")
	fs.BoolVar(&yes, "yes", false, "do not ask")
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			fmt.Fprintln(stderr, skillsUsage)
			return "", "", "", false, nil, false
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return dir, ref, sub, yes, words, true
}

// runSkills is `maestro skills`.
func runSkills(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, ref, sub, yes, words, ok := skillFlags(args, stderr)
	if !ok {
		return 2
	}
	if dir == "" {
		found, err := configDir()
		if err != nil {
			fmt.Fprintln(stderr, "no configuration directory was found for this user, pass --dir:", err)
			return 1
		}
		dir = found
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return report(stderr, err)
	}
	store := skills.Store{Dir: filepath.Join(dir, "agent-files")}
	switch {
	case len(words) == 0 || len(words) == 1 && words[0] == "list":
		return listSkills(store, stdout, stderr)
	case words[0] == "add" && len(words) == 2:
		return addSkills(store, words[1], ref, sub, yes, stdin, stdout, stderr)
	case words[0] == "remove" && len(words) == 2:
		n, err := store.Remove(words[1])
		if err != nil {
			return report(stderr, err)
		}
		if n == 0 {
			fmt.Fprintf(stderr, "Nothing called %q is installed. See what is: maestro skills\n", plain(words[1], 80))
			return 1
		}
		fmt.Fprintf(stdout, "Removed %s. Agents that start from now on do not have it.\n", words[1])
		return 0
	}
	fmt.Fprintln(stderr, skillsUsage)
	return 2
}

func listSkills(store skills.Store, stdout, stderr io.Writer) int {
	es, err := store.List()
	if err != nil {
		return report(stderr, err)
	}
	if len(es) == 0 {
		fmt.Fprintln(stdout, "The agents have no extra skills or agent files.")
		fmt.Fprintln(stdout, "Add some with: maestro skills add owner/repo    (or a directory: maestro skills add ./my-skills)")
		return 0
	}
	fmt.Fprintln(stdout, "What the agents have, next to the repository they work in:")
	for _, e := range es {
		from := e.Source
		if e.Commit != "" {
			from += " at " + e.Commit[:12]
		}
		fmt.Fprintf(stdout, "  %-6s %s\n         %s\n         from %s, added %s\n", e.Kind, e.Name, plain(e.Description, 160), plain(from, 200), e.Added[:10])
	}
	fmt.Fprintln(stdout, "They never change by themselves. To get a newer version, add the source again.")
	return 0
}

func addSkills(store skills.Store, arg, ref, sub string, yes bool, stdin io.Reader, stdout, stderr io.Writer) int {
	home, _ := os.UserHomeDir()
	src, err := skills.Parse(arg, ref, sub, home)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	if src.URL != "" {
		fmt.Fprintf(stdout, "Fetching %s ...\n", src.URL)
	}
	root, commit, cleanup, err := skills.Fetch(ctx, src)
	defer cleanup()
	if err != nil {
		return report(stderr, err)
	}
	found, problems := skills.Discover(root)
	for _, p := range problems {
		fmt.Fprintf(stderr, "skipped %s\n", plain(p, 300))
	}
	if len(found) == 0 {
		fmt.Fprintln(stderr, "No skill (a directory with a SKILL.md) and no agent file (a Markdown file in a directory called agents) was found there.")
		return 1
	}
	have := map[string]skills.Entry{}
	if es, err := store.List(); err == nil {
		for _, e := range es {
			have[string(e.Kind)+"/"+e.Name] = e
		}
	}
	where := arg
	if commit != "" {
		where += " at commit " + commit[:12]
	}
	fmt.Fprintf(stdout, "\nFound in %s:\n", plain(where, 200))
	for i := range found {
		f := &found[i]
		f.Source, f.Commit = arg, commit
		note := ""
		if old, ok := have[string(f.Kind)+"/"+f.Name]; ok {
			note = "   (replaces the one from " + plain(old.Source, 100) + ")"
		}
		fmt.Fprintf(stdout, "  %-6s %s%s\n         %s\n", f.Kind, f.Name, note, plain(f.Description, 200))
		if f.Kind == skills.Skill {
			fmt.Fprintf(stdout, "         %d file(s), %d KB\n", len(f.Files), (f.Bytes+1023)/1024)
		}
		if len(f.Scripts) > 0 {
			shown := f.Scripts
			if len(shown) > 8 {
				shown = append(append([]string{}, shown[:8]...), fmt.Sprintf("and %d more", len(f.Scripts)-8))
			}
			fmt.Fprintf(stdout, "         programs the agent may run: %s\n", plain(strings.Join(shown, ", "), 400))
		}
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Every agent will follow these like it follows your own instructions, so add only what you have read or trust.")
	fmt.Fprintln(stdout, "Their programs run inside the agent's container, never on this machine. To read them first:")
	fmt.Fprintf(stdout, "  %s\n", plain(readHint(src), 300))
	if !yes {
		ok, err := setup.Terminal(stdin, stdout).Confirm(fmt.Sprintf("Add %s?", count(len(found))))
		if err != nil || !ok {
			fmt.Fprintln(stdout, "Nothing was added.")
			return 3
		}
	}
	for _, f := range found {
		if err := store.Install(f, time.Now()); err != nil {
			return report(stderr, fmt.Errorf("%s: %w", f.Name, err))
		}
	}
	fmt.Fprintf(stdout, "Added %s. Agents that start from now on have them; the service does not need a restart.\n", count(len(found)))
	fmt.Fprintln(stdout, "See them with `maestro skills`, take one away with `maestro skills remove NAME`.")
	return 0
}

func count(n int) string {
	if n == 1 {
		return "this one"
	}
	return fmt.Sprintf("these %d", n)
}

// readHint is where the person can read what they are about to add.
func readHint(src skills.Source) string {
	if src.Local != "" {
		return filepath.Join(src.Local, filepath.FromSlash(src.Sub))
	}
	u := strings.TrimSuffix(src.URL, ".git")
	if strings.HasPrefix(u, "https://github.com/") && (src.Sub != "" || src.Ref != "") {
		ref := src.Ref
		if ref == "" {
			ref = "HEAD"
		}
		return u + "/tree/" + ref + "/" + src.Sub
	}
	return u
}
