package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

const settingsUsage = `usage:
  maestro settings                    show the everyday settings and their values
  maestro settings set NAME VALUE     change one of them

The settings shown here are the ones an owner changes most: the daily limits,
how many tickets run at once, and the Claude models. ` + "`set`" + ` writes the one line
in the settings file, checks the result the way the service will read it, and
leaves the file as it was when the new value is not valid. A running service
picks the change up by itself, within a few seconds.

For a limit that can be switched off, the value none removes the limit:
  maestro settings set budget.max_tokens_per_day none

Every other setting is in the settings file itself, WORKFLOW.md; ` + "`maestro settings`" + `
prints where it is.`

type settingKind int

const (
	whole    settingKind = iota // a positive whole number
	optional                    // a positive whole number, or none
	word                        // a word, such as a model name
)

type setting struct {
	name string
	kind settingKind
	what string
	get  func(*config.Config) string
}

func optString(v *int64) string {
	if v == nil {
		return "none"
	}
	return strconv.FormatInt(*v, 10)
}

var everydaySettings = []setting{
	{"budget.max_runs_per_day", whole, "agent runs a day; at the limit no new ticket starts until midnight",
		func(c *config.Config) string { return strconv.FormatInt(c.Budget.MaxRunsPerDay, 10) }},
	{"budget.max_tokens_per_day", optional, "tokens a day",
		func(c *config.Config) string { return optString(c.Budget.MaxTokensPerDay) }},
	{"budget.max_agent_seconds_per_day", optional, "seconds of agent time a day",
		func(c *config.Config) string { return optString(c.Budget.MaxAgentSecondsPerDay) }},
	{"budget.max_failure_attempts", whole, "failed attempts on one ticket before it is blocked",
		func(c *config.Config) string { return strconv.FormatInt(c.Budget.MaxFailureAttempts, 10) }},
	{"agent.max_concurrent_agents", whole, "tickets worked on at the same time",
		func(c *config.Config) string { return strconv.FormatInt(c.Agent.MaxConcurrentAgents, 10) }},
	{"agent.max_turns", whole, "turns the agent gets for one attempt",
		func(c *config.Config) string { return strconv.FormatInt(c.Agent.MaxTurns, 10) }},
	{"claude.model", word, "the Claude model the agent uses",
		func(c *config.Config) string { return c.Claude.Model }},
	{"claude.planning_model", word, "the model for planning an idea; empty means claude.model",
		func(c *config.Config) string { return c.Claude.PlanningModel }},
	{"publish.checks_timeout_ms", whole, "milliseconds to wait for a pull request's checks before the ticket is blocked",
		func(c *config.Config) string { return strconv.FormatInt(c.Publish.ChecksTimeoutMS, 10) }},
	{"polling.interval_ms", whole, "milliseconds between two looks at the board",
		func(c *config.Config) string { return strconv.FormatInt(c.Polling.IntervalMS, 10) }},
}

func findSetting(name string) *setting {
	for i := range everydaySettings {
		if everydaySettings[i].name == name {
			return &everydaySettings[i]
		}
	}
	return nil
}

func runSettings(args []string, stdout, stderr io.Writer) int {
	// The words come first, the flags after them or before them.
	var rest, flags []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			rest = append(rest, a)
		}
	}
	_, explicit, more, ok := parseWords("settings", flags, stderr, true)
	if !ok || len(more) > 0 {
		fmt.Fprintln(stderr, settingsUsage)
		return 2
	}
	switch {
	case len(rest) == 0 || (len(rest) == 1 && rest[0] == "show"):
		return showSettings(workflowPath(explicit), stdout, stderr)
	case rest[0] == "set" && len(rest) == 3:
		return setSetting(workflowPath(explicit), rest[1], rest[2], stdout, stderr)
	}
	fmt.Fprintln(stderr, settingsUsage)
	return 2
}

func loadSettings(file string) (*config.Config, error) {
	def, err := workflow.Load(file)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	return config.Build(def, config.Options{Env: os.Getenv, HomeDir: home})
}

func showSettings(path string, stdout, stderr io.Writer) int {
	cfg, err := loadSettings(path)
	if err != nil {
		return report(stderr, explain("%v\nThe settings file is %s. Run `maestro setup` if you have not yet.", err, path))
	}
	fmt.Fprintf(stdout, "Settings in %s\n\n", path)
	for _, s := range everydaySettings {
		v := s.get(cfg)
		if v == "" {
			v = `""`
		}
		fmt.Fprintf(stdout, "  %-34s %-10s %s\n", s.name, v, s.what)
	}
	fmt.Fprintln(stdout, "\nChange one with:  maestro settings set NAME VALUE")
	fmt.Fprintln(stdout, "For example:      maestro settings set budget.max_runs_per_day 40")
	fmt.Fprintln(stdout, "A running service picks a change up by itself.")
	return 0
}

func setSetting(path, name, value string, stdout, stderr io.Writer) int {
	s := findSetting(name)
	if s == nil {
		var names []string
		for _, e := range everydaySettings {
			names = append(names, "  "+e.name)
		}
		return report(stderr, explain("maestro settings cannot change %q. It changes these:\n%s\nAny other setting is changed in %s itself.",
			name, strings.Join(names, "\n"), path))
	}
	value = strings.TrimSpace(value)
	literal := value
	switch s.kind {
	case whole, optional:
		if s.kind == optional && (value == "none" || value == "off") {
			literal, value = "null", "none"
			break
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			hint := "a whole number above 0"
			if s.kind == optional {
				hint += ", or none for no limit"
			}
			return report(stderr, explain("%s takes %s, not %q.", name, hint, value))
		}
		literal, value = strconv.FormatInt(n, 10), strconv.FormatInt(n, 10)
	case word:
		if strings.ContainsAny(value, "\n\r") {
			return report(stderr, explain("%s takes one line.", name))
		}
		literal = strconv.Quote(value)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return report(stderr, explain("%v\nThe settings file is %s. Run `maestro setup` if you have not yet.", err, path))
	}
	before, err := loadSettings(path)
	if err != nil {
		return report(stderr, explain("%s is not valid as it is: %v\nFix that first.", path, err))
	}
	old := s.get(before)
	if old == value {
		fmt.Fprintf(stdout, "%s is already %s in %s.\n", name, value, path)
		return 0
	}
	section, key, _ := strings.Cut(name, ".")
	changed, err := withSetting(string(raw), section, key, literal)
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
	if err := os.WriteFile(tmp, []byte(changed), info.Mode().Perm()); err != nil {
		return report(stderr, err)
	}
	defer os.Remove(tmp)
	after, err := loadSettings(tmp)
	if err == nil && s.get(after) != value {
		err = errors.New("the change did not take effect")
	}
	if err != nil {
		return report(stderr, explain("The settings file was left as it was, because the changed one is not valid: %v", err))
	}
	if err := os.Rename(tmp, path); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "%s is now %s (it was %s) in %s.\n", name, value, old, path)
	fmt.Fprintln(stdout, "A running service picks this up by itself; `maestro status` shows it within a few seconds.")
	return 0
}

var sectionRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*):[ \t]*(.*)$`)

// withSetting returns the settings file with section.key set to literal,
// a YAML scalar. Only that one line is written or added; everything else
// stays as the owner wrote it.
func withSetting(text, section, key, literal string) (string, error) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", errors.New("the settings file does not start with a settings block (a line of three dashes)")
	}
	lines := strings.SplitAfter(text, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", errors.New("the settings block has no closing line of three dashes")
	}
	nl := "\n"
	if strings.HasSuffix(lines[0], "\r\n") {
		nl = "\r\n"
	}
	join := func(ls ...[]string) string {
		var b strings.Builder
		for _, l := range ls {
			b.WriteString(strings.Join(l, ""))
		}
		return b.String()
	}
	head := -1
	for i := 1; i < end; i++ {
		m := sectionRE.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
		if m == nil || m[1] != section {
			continue
		}
		if v := strings.TrimSpace(m[2]); v != "" && !strings.HasPrefix(v, "#") {
			return "", fmt.Errorf("the settings file has `%s:` in a form this command does not edit (%s). "+
				"Write it as a block, one setting per line, or change %s.%s there", section, v, section, key)
		}
		head = i
		break
	}
	if head < 0 {
		add := []string{section + ":" + nl, "  " + key + ": " + literal + nl}
		return join(lines[:end], add, lines[end:]), nil
	}
	keyRE := regexp.MustCompile(`^([ \t]+)` + regexp.QuoteMeta(key) + `:[ \t]*(.*)$`)
	indent := ""
	for i := head + 1; i < end; i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == line {
			break // the next top-level key
		}
		lead := line[:len(line)-len(trimmed)]
		if indent == "" {
			indent = lead
		}
		if lead != indent {
			continue // deeper: a value of another key
		}
		if m := keyRE.FindStringSubmatch(line); m != nil {
			comment := ""
			if c := strings.Index(m[2], " #"); c >= 0 {
				comment = m[2][c:]
			}
			out := append([]string{}, lines...)
			out[i] = indent + key + ": " + literal + comment + nl
			return strings.Join(out, ""), nil
		}
	}
	if indent == "" {
		indent = "  "
	}
	add := []string{indent + key + ": " + literal + nl}
	return join(lines[:head+1], add, lines[head+1:]), nil
}
