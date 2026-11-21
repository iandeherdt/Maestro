// Package config turns a workflow definition into typed, validated runtime
// settings (SPEC sections 5.3 and 6).
package config

import (
	"errors"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

// Config is the typed view of the workflow front matter.
type Config struct {
	// Conventions are the names of the rule packs that are switched on.
	// What they add is already merged into Hooks, Sandbox, Verify and
	// Policy below.
	Conventions []string
	Tracker     Tracker
	Polling     Polling
	Workspace   Workspace
	Hooks       Hooks
	Agent       Agent
	Claude      Claude
	Sandbox     Sandbox
	Verify      Verify
	Screenshots Screenshots
	Publish     Publish
	Policy      Policy
	Budget      Budget
	Worker      Worker
	Server      Server

	// Warnings are non-fatal findings, such as ignored unknown keys.
	Warnings []string
}

type Tracker struct {
	Kind string
	// Provider is adapter-owned configuration, passed through unchanged.
	Provider        map[string]any
	RequiredLabels  []string // effective: normalized, plus the routing label
	ActiveStates    []string
	TerminalStates  []string
	HandoffState    string
	InProgressState string
	BlockedState    string
	DoneState       string
	// PlanningState is the state in which an issue is an idea to be planned
	// rather than work to be done (SPEC 22). Empty switches planning off.
	PlanningState string
	// PlannedState is where the owner moves an idea to approve its plan.
	PlannedState      string
	AllowedAuthors    []string
	AllowedCommenters []string
}

type Polling struct{ IntervalMS int64 }

type Workspace struct {
	Root       string
	Repo       string
	BaseBranch string // empty means the repository's default branch
	CloneDepth int64
}

type Hooks struct {
	AfterCreate, BeforeRun, AfterRun, BeforeRemove string
	TimeoutMS                                      int64
}

type Agent struct {
	MaxConcurrentAgents        int64
	MaxTurns                   int64
	MaxRetryBackoffMS          int64
	MaxConcurrentAgentsByState map[string]int64 // keys normalized
}

type Claude struct {
	Command            string
	Model              string
	MaxStepsPerTurn    int64
	PermissionMode     string
	DisallowedTools    []string
	InnerSandbox       bool
	TurnTimeoutMS      int64
	TurnTotalTimeoutMS int64
	ReadTimeoutMS      int64
	StallTimeoutMS     int64
}

type Sandbox struct {
	Backend    string
	Image      string
	MemoryMax  string
	CPUQuota   string
	TasksMax   int64
	Proxy      string
	AllowAgent []string
	AllowHooks []string
	Env        map[string]string
}

type Verify struct {
	Command         string
	TimeoutMS       int64
	Network         bool
	OutputTailBytes int64
}

// Screenshots is how the service takes pictures of the work for the pull
// request (SPEC 24). Either Pages or Command is set, or neither.
type Screenshots struct {
	// Pages are files in the repository, or addresses on localhost.
	Pages []string
	// Start is a command that is started first and left running, such as
	// a development server.
	Start string
	// Command is the operator's own command. It writes PNG files into
	// $SCREENSHOT_DIR.
	Command       string
	TimeoutMS     int64
	Width, Height int64
}

// On reports whether screenshots are taken.
func (s Screenshots) On() bool { return len(s.Pages) > 0 || s.Command != "" }

// MaxScreenshots is how many pictures one pull request shows.
const MaxScreenshots = 6

type Publish struct {
	BranchPrefix string
	// RequiredChecksFromBranchProtection is true when required checks are
	// taken from the base branch's protection settings.
	RequiredChecksFromBranchProtection bool
	RequiredChecks                     []string
	// NoChecks is the operator saying the repository has no checks: a
	// pull request is handed to review as soon as it is open.
	NoChecks                 bool
	DraftUntilGreen          bool
	ChecksTimeoutMS          int64
	ChecksPollIntervalMS     int64
	ReworkOnChangesRequested bool
	// ReworkOnComments sends a ticket back to work when an allowlisted
	// person comments on its pull request. ReviewQuietMS is how long that
	// person has to be quiet first, so that several comments start one run.
	ReworkOnComments bool
	ReviewQuietMS    int64
	MaxBundleBytes   int64
}

type Policy struct {
	DenyPaths       []string // additions to the built-in list
	FlagPaths       []string
	MaxChangedFiles int64
	MaxDiffBytes    int64
	MaxFileBytes    int64
	AllowBinary     bool
	AllowSymlinks   bool
	TestPaths       []string
	SecretScan      bool
}

type Budget struct {
	MaxFailureAttempts    int64
	MaxRunsPerDay         int64
	MaxTokensPerDay       *int64
	MaxAgentSecondsPerDay *int64
	QuotaBackoffMS        int64
	StateDir              string
}

type Worker struct {
	Name                string
	RequireRoutingLabel bool
}

type Server struct {
	// Port is nil when the HTTP extension is disabled.
	Port *int64
}

// Env resolves environment variables. It returns "" for unset names.
type Env func(name string) string

// Options are the inputs to Build that do not come from the workflow file.
type Options struct {
	Env Env
	// HomeDir is used for "~" expansion.
	HomeDir string
}

var (
	repoRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	workerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	varRefRE     = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostNameRE   = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)
	credEnvRE    = regexp.MustCompile(`^(ANTHROPIC_.*|CLAUDE_.*TOKEN.*|.*_API_KEY|GITHUB_.*|GH_.*|LINEAR_.*)$`)
	sizeRE       = regexp.MustCompile(`^[1-9][0-9]*[KMGT]?$`)
	cpuQuotaRE   = regexp.MustCompile(`^[1-9][0-9]*%$`)
	imageRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$`)
	branchRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	permModes    = set("acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan")
	backends     = set("systemd", "docker", "podman", "macos-vm", "macos-user")
	trackerKinds = set("github", "linear")
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// Normalize trims and lowercases a state or label for comparison.
func Normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Build validates def and returns the typed configuration. All problems found
// are returned together, joined into one error.
func Build(def *workflow.Definition, opt Options) (*Config, error) {
	if opt.Env == nil {
		opt.Env = func(string) string { return "" }
	}
	var errs []error
	c := &Config{}
	root := newSection("", def.Config, &errs)

	b := &builder{opt: opt, errs: &errs, cfg: c}
	b.conventions(root)
	b.tracker(root.sub("tracker"))
	b.polling(root.sub("polling"))
	b.workspace(root.sub("workspace"))
	b.hooks(root.sub("hooks"))
	b.agent(root.sub("agent"))
	b.claude(root.sub("claude"))
	b.sandbox(root.sub("sandbox"))
	b.verify(root.sub("verify"))
	b.screenshots(root.sub("screenshots"))
	b.publish(root.sub("publish"))
	b.policy(root.sub("policy"))
	b.budget(root.sub("budget"))
	b.worker(root.sub("worker"))
	b.server(root.sub("server"))

	for _, k := range root.unknown() {
		c.Warnings = append(c.Warnings, fmt.Sprintf("unknown top-level key %q ignored", k))
	}
	b.crossChecks(def.Path)

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

type builder struct {
	opt  Options
	errs *[]error
	cfg  *Config
	// packs is what the rule packs add (SPEC 23).
	packs conventions.Settings
}

// conventions reads the list of rule packs. Their settings are merged by
// the sections they belong to, always in addition to what the file says:
// a pack can add a protected path or a check, never take one away.
func (b *builder) conventions(root *section) {
	names := trimAll(root.strList("conventions", nil))
	list, err := conventions.Resolve(names)
	if err != nil {
		b.fail("conventions", "%v", err)
		return
	}
	b.cfg.Conventions = names
	b.packs = conventions.Derive(list)
}

// both joins two shell scripts so that the second runs only when the
// first succeeded.
func both(first, second string) string {
	switch {
	case strings.TrimSpace(first) == "":
		return second
	case strings.TrimSpace(second) == "":
		return first
	}
	return first + "\n" + second
}

func (b *builder) fail(key, format string, args ...any) {
	*b.errs = append(*b.errs, &workflow.Error{
		Class: workflow.ClassInvalidConfig, Key: key, Msg: fmt.Sprintf(format, args...),
	})
}

// lenient reports unknown keys in a section as warnings.
func (b *builder) lenient(s *section) {
	for _, k := range s.unknown() {
		b.cfg.Warnings = append(b.cfg.Warnings, fmt.Sprintf("unknown key %q ignored", k))
	}
}

// strict reports unknown keys in a section as errors. A typo in these
// sections could silently disable a control.
func (b *builder) strict(s *section) {
	for _, k := range s.unknown() {
		b.fail(k, "unknown key")
	}
}

// path resolves "$VAR" indirection and "~" expansion and requires an absolute
// result.
func (b *builder) path(key, v string) string {
	if v == "" {
		b.fail(key, "is required")
		return ""
	}
	if m := varRefRE.FindStringSubmatch(v); m != nil {
		v = b.opt.Env(m[1])
		if v == "" {
			b.fail(key, "environment variable %s is empty or unset", m[1])
			return ""
		}
	}
	if v == "~" || strings.HasPrefix(v, "~/") {
		if b.opt.HomeDir == "" {
			b.fail(key, "cannot expand ~: home directory unknown")
			return ""
		}
		v = filepath.Join(b.opt.HomeDir, strings.TrimPrefix(v, "~"))
	}
	if !filepath.IsAbs(v) {
		b.fail(key, "must be an absolute path")
		return ""
	}
	return filepath.Clean(v)
}

func (b *builder) positive(key string, v int64) int64 {
	if v <= 0 {
		b.fail(key, "must be a positive integer")
	}
	return v
}

func (b *builder) nonNegative(key string, v int64) int64 {
	if v < 0 {
		b.fail(key, "must not be negative")
	}
	return v
}

func (b *builder) tracker(s *section) {
	t := &b.cfg.Tracker
	t.Kind = s.str("kind", "")
	switch {
	case t.Kind == "":
		b.fail("tracker.kind", "is required")
	case !trackerKinds[t.Kind]:
		b.fail("tracker.kind", "unsupported tracker kind %q", t.Kind)
	}
	if v, ok := s.raw("provider"); ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			b.fail("tracker.provider", "must be a mapping")
		}
		t.Provider = m
	}
	if t.Provider == nil {
		t.Provider = map[string]any{}
	}
	t.RequiredLabels = normalizeLabels(s.strList("required_labels", nil))
	t.ActiveStates = s.strList("active_states", []string{"Todo", "In Progress", "Rework"})
	t.TerminalStates = s.strList("terminal_states",
		[]string{"Done", "Closed", "Cancelled", "Canceled", "Duplicate"})
	t.HandoffState = s.str("handoff_state", "Human Review")
	t.InProgressState = s.str("in_progress_state", "In Progress")
	t.BlockedState = s.str("blocked_state", "Blocked")
	t.DoneState = s.str("done_state", "Done")
	// Planning needs a board, so it is on by default only there.
	planDefault := ""
	if t.Provider["state_source"] == "project" {
		planDefault = "Idea"
	}
	t.PlanningState = strings.TrimSpace(s.str("planning_state", planDefault))
	t.PlannedState = strings.TrimSpace(s.str("planned_state", "Planned"))
	t.AllowedAuthors = trimAll(s.strList("allowed_authors", nil))
	t.AllowedCommenters = trimAll(s.strList("allowed_commenters", t.AllowedAuthors))
	b.lenient(s)

	if len(t.AllowedAuthors) == 0 {
		b.fail("tracker.allowed_authors", "is required and must not be empty")
	}
	if len(t.ActiveStates) == 0 {
		b.fail("tracker.active_states", "must not be empty")
	}
	active, terminal := stateSet(t.ActiveStates), stateSet(t.TerminalStates)
	for st := range active {
		if terminal[st] {
			b.fail("tracker.active_states", "state %q is also terminal", st)
		}
	}
	check := func(key, st string, wantActive, wantTerminal bool) {
		n := Normalize(st)
		if n == "" {
			b.fail(key, "must not be empty")
			return
		}
		if active[n] != wantActive {
			b.fail(key, "state %q must%s be an active state", st, notIf(!wantActive))
		}
		if terminal[n] != wantTerminal {
			b.fail(key, "state %q must%s be a terminal state", st, notIf(!wantTerminal))
		}
	}
	check("tracker.handoff_state", t.HandoffState, false, false)
	check("tracker.in_progress_state", t.InProgressState, true, false)
	check("tracker.blocked_state", t.BlockedState, false, false)
	check("tracker.done_state", t.DoneState, false, true)

	if t.PlanningState != "" {
		// An idea is polled like any active state, but it must be told
		// apart from every state that means something else.
		plan, planned := Normalize(t.PlanningState), Normalize(t.PlannedState)
		taken := map[string]bool{Normalize(t.HandoffState): true, Normalize(t.BlockedState): true}
		for st := range active {
			taken[st] = true
		}
		for st := range terminal {
			taken[st] = true
		}
		switch {
		case taken[plan]:
			b.fail("tracker.planning_state", "state %q is already used for something else", t.PlanningState)
		case planned == "" || planned == plan || taken[planned]:
			b.fail("tracker.planned_state", "state %q must be a state of its own", t.PlannedState)
		default:
			t.ActiveStates = append(t.ActiveStates, t.PlanningState)
		}
	}
}

func notIf(b bool) string {
	if b {
		return " not"
	}
	return ""
}

func stateSet(states []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range states {
		if n := Normalize(s); n != "" {
			m[n] = true
		}
	}
	return m
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// normalizeLabels lowercases and trims labels and removes duplicates. A blank
// entry is kept as "", which matches no issue (SPEC 5.3.1).
func normalizeLabels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, l := range in {
		n := Normalize(l)
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func (b *builder) polling(s *section) {
	b.cfg.Polling.IntervalMS = s.integer("interval_ms", 30000)
	b.lenient(s)
	if b.cfg.Polling.IntervalMS < 5000 {
		b.fail("polling.interval_ms", "must be at least 5000")
	}
}

func (b *builder) workspace(s *section) {
	w := &b.cfg.Workspace
	w.Root = b.path("workspace.root", s.str("root", ""))
	w.Repo = s.str("repo", "")
	w.BaseBranch = s.str("base_branch", "")
	w.CloneDepth = b.nonNegative("workspace.clone_depth", s.integer("clone_depth", 0))
	b.lenient(s)
	if w.Repo == "" {
		b.fail("workspace.repo", "is required")
	} else if !repoRE.MatchString(w.Repo) {
		b.fail("workspace.repo", "must have the form owner/name")
	}
	if w.BaseBranch != "" && !validBranch(w.BaseBranch) {
		b.fail("workspace.base_branch", "is not a valid branch name")
	}
}

func validBranch(s string) bool {
	return branchRE.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, ".lock")
}

func (b *builder) hooks(s *section) {
	h := &b.cfg.Hooks
	h.AfterCreate = s.str("after_create", "")
	// The packs install the project's dependencies before every run.
	h.BeforeRun = both(b.packs.Prepare, s.str("before_run", ""))
	h.AfterRun = s.str("after_run", "")
	h.BeforeRemove = s.str("before_remove", "")
	timeout := int64(60000)
	if b.packs.Prepare != "" {
		timeout = 900000 // installing dependencies takes longer than a minute
	}
	h.TimeoutMS = b.positive("hooks.timeout_ms", s.integer("timeout_ms", timeout))
	b.lenient(s)
}

func (b *builder) agent(s *section) {
	a := &b.cfg.Agent
	a.MaxConcurrentAgents = b.positive("agent.max_concurrent_agents", s.integer("max_concurrent_agents", 1))
	a.MaxTurns = b.positive("agent.max_turns", s.integer("max_turns", 8))
	a.MaxRetryBackoffMS = b.positive("agent.max_retry_backoff_ms", s.integer("max_retry_backoff_ms", 300000))
	a.MaxConcurrentAgentsByState = map[string]int64{}
	if v, ok := s.raw("max_concurrent_agents_by_state"); ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			b.fail("agent.max_concurrent_agents_by_state", "must be a mapping")
		}
		for k, raw := range m {
			// Invalid entries are ignored, as upstream specifies.
			n, isInt := raw.(int)
			if !isInt || n <= 0 || Normalize(k) == "" {
				b.cfg.Warnings = append(b.cfg.Warnings,
					fmt.Sprintf("agent.max_concurrent_agents_by_state.%s ignored: not a positive integer", k))
				continue
			}
			a.MaxConcurrentAgentsByState[Normalize(k)] = int64(n)
		}
	}
	b.lenient(s)
}

func (b *builder) claude(s *section) {
	c := &b.cfg.Claude
	c.Command = strings.TrimSpace(s.str("command", "claude"))
	c.Model = s.str("model", "")
	c.MaxStepsPerTurn = b.positive("claude.max_steps_per_turn", s.integer("max_steps_per_turn", 150))
	c.PermissionMode = s.str("permission_mode", "bypassPermissions")
	c.DisallowedTools = trimAll(s.strList("disallowed_tools", []string{"WebFetch", "WebSearch"}))
	c.InnerSandbox = s.boolean("inner_sandbox", false)
	c.TurnTimeoutMS = b.positive("claude.turn_timeout_ms", s.integer("turn_timeout_ms", 3600000))
	c.TurnTotalTimeoutMS = b.positive("claude.turn_total_timeout_ms", s.integer("turn_total_timeout_ms", 5400000))
	c.ReadTimeoutMS = b.positive("claude.read_timeout_ms", s.integer("read_timeout_ms", 30000))
	c.StallTimeoutMS = s.integer("stall_timeout_ms", 300000) // <= 0 disables stall detection
	b.lenient(s)

	if c.Command == "" {
		b.fail("claude.command", "must not be empty")
	} else if strings.ContainsAny(c.Command, " \t\n;|&$`'\"<>") {
		// The service builds the argument vector itself. No shell is involved.
		b.fail("claude.command", "must be an executable name or path, not a shell command")
	}
	if !permModes[c.PermissionMode] {
		b.fail("claude.permission_mode", "unsupported value %q", c.PermissionMode)
	}
	for _, t := range c.DisallowedTools {
		if strings.ContainsAny(t, " \t\n,") {
			b.fail("claude.disallowed_tools", "tool name %q must not contain spaces or commas", t)
		}
	}
}

func (b *builder) sandbox(s *section) {
	sb := &b.cfg.Sandbox
	sb.Backend = s.str("backend", "")
	sb.Image = s.str("image", "symphony-sandbox:latest")
	sb.MemoryMax = s.str("memory_max", "6G")
	sb.CPUQuota = s.str("cpu_quota", "300%")
	sb.TasksMax = b.positive("sandbox.tasks_max", s.integer("tasks_max", 2048))
	n := s.sub("network")
	sb.Proxy = n.str("proxy", "")
	sb.AllowAgent = b.hosts("sandbox.network.allow_agent", n.strList("allow_agent", nil))
	sb.AllowHooks = b.hosts("sandbox.network.allow_hooks", n.strList("allow_hooks", nil))
	for _, h := range b.packs.Hosts {
		if !slices.Contains(sb.AllowHooks, h) {
			sb.AllowHooks = append(sb.AllowHooks, h)
		}
	}
	sb.Env = s.strMap("env")
	b.strict(n)
	b.strict(s)

	switch {
	case sb.Backend == "":
		b.fail("sandbox.backend", "is required")
	case !backends[sb.Backend]:
		b.fail("sandbox.backend", "unsupported backend %q", sb.Backend)
	}
	if !sizeRE.MatchString(sb.MemoryMax) {
		b.fail("sandbox.memory_max", "must be a size such as 6G")
	}
	if !cpuQuotaRE.MatchString(sb.CPUQuota) {
		b.fail("sandbox.cpu_quota", "must be a percentage such as 300%%")
	}
	if !imageRE.MatchString(sb.Image) {
		b.fail("sandbox.image", "must be an image name such as symphony-sandbox:latest")
	}
	// The docker backend runs its own proxy next to the containers.
	if sb.Proxy == "" && (sb.Backend == "docker" || sb.Backend == "podman") {
	} else if sb.Proxy == "" {
		b.fail("sandbox.network.proxy", "is required")
	} else if host, port, err := net.SplitHostPort(sb.Proxy); err != nil || host == "" || !validPort(port) {
		b.fail("sandbox.network.proxy", "must have the form host:port")
	}
	for k := range sb.Env {
		switch {
		case !envNameRE.MatchString(k):
			b.fail("sandbox.env."+k, "is not a valid environment variable name")
		case credEnvRE.MatchString(strings.ToUpper(k)):
			b.fail("sandbox.env."+k, "credential-like variables must not be passed into the sandbox")
		}
	}
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

func (b *builder) hosts(key string, in []string) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		if !hostNameRE.MatchString(h) || net.ParseIP(h) != nil {
			b.fail(key, "%q is not a host name", h)
			continue
		}
		out = append(out, h)
	}
	return out
}

func (b *builder) verify(s *section) {
	v := &b.cfg.Verify
	// The packs' checks run first; the file's own command, if any, after.
	own := s.str("command", "")
	if b.packs.Verify != "" && strings.TrimSpace(own) == "true" {
		own = "" // the placeholder setup writes when there are no tests
	}
	v.Command = both(b.packs.Verify, own)
	v.TimeoutMS = b.positive("verify.timeout_ms", s.integer("timeout_ms", 1800000))
	v.Network = s.boolean("network", false)
	v.OutputTailBytes = b.positive("verify.output_tail_bytes", s.integer("output_tail_bytes", 16384))
	b.lenient(s)
	if strings.TrimSpace(v.Command) == "" {
		b.fail("verify.command", "is required")
	}
	if v.Network {
		b.cfg.Warnings = append(b.cfg.Warnings, "verify.network is true: the verify step can reach the hook allowlist")
	}
}

var localPageRE = regexp.MustCompile(`^http://(localhost|127\.0\.0\.1)(:[0-9]{1,5})?(/[^\s]*)?$`)

func (b *builder) screenshots(s *section) {
	sh := &b.cfg.Screenshots
	sh.Pages = s.strList("pages", nil)
	sh.Start = strings.TrimSpace(s.str("start", ""))
	sh.Command = strings.TrimSpace(s.str("command", ""))
	sh.TimeoutMS = b.positive("screenshots.timeout_ms", s.integer("timeout_ms", 300000))
	sh.Width = b.positive("screenshots.width", s.integer("width", 1280))
	sh.Height = b.positive("screenshots.height", s.integer("height", 800))
	b.strict(s)
	if len(sh.Pages) > 0 && sh.Command != "" {
		b.fail("screenshots.command", "cannot be set together with screenshots.pages")
	}
	if len(sh.Pages) == 0 && sh.Start != "" {
		b.fail("screenshots.start", "needs screenshots.pages")
	}
	if len(sh.Pages) > MaxScreenshots {
		b.fail("screenshots.pages", "has more than %d entries", MaxScreenshots)
	}
	if sh.Width > 4000 || sh.Height > 4000 {
		b.fail("screenshots.width", "and screenshots.height are at most 4000")
	}
	for _, page := range sh.Pages {
		switch {
		case localPageRE.MatchString(page):
		case strings.Contains(page, "://"):
			b.fail("screenshots.pages", "%q is not on this machine: only http://localhost addresses and files in the repository can be shown", page)
		case page == "" || strings.HasPrefix(page, "/") || strings.HasPrefix(page, "-") || strings.ContainsAny(page, "\n\\") ||
			page != path.Clean(page) || strings.HasPrefix(page, ".."):
			b.fail("screenshots.pages", "%q is not a file in the repository, written like docs/index.html", page)
		}
	}
}

func (b *builder) publish(s *section) {
	p := &b.cfg.Publish
	p.BranchPrefix = s.str("branch_prefix", "symphony/")
	p.DraftUntilGreen = s.boolean("draft_until_green", true)
	p.ChecksTimeoutMS = b.positive("publish.checks_timeout_ms", s.integer("checks_timeout_ms", 3600000))
	p.ChecksPollIntervalMS = b.positive("publish.checks_poll_interval_ms", s.integer("checks_poll_interval_ms", 60000))
	p.ReworkOnChangesRequested = s.boolean("rework_on_changes_requested", true)
	p.ReworkOnComments = s.boolean("rework_on_comments", true)
	p.ReviewQuietMS = b.positive("publish.review_quiet_ms", s.integer("review_quiet_ms", 120000))
	p.MaxBundleBytes = b.positive("publish.max_bundle_bytes", s.integer("max_bundle_bytes", 104857600))

	switch v, ok := s.raw("required_checks"); {
	case !ok:
		p.RequiredChecksFromBranchProtection = true
	default:
		if str, isStr := v.(string); isStr {
			switch str {
			case "none":
				p.NoChecks = true
				b.cfg.Warnings = append(b.cfg.Warnings,
					"publish.required_checks is none: pull requests are handed to review without waiting for CI")
			case "branch-protection":
				p.RequiredChecksFromBranchProtection = true
			default:
				b.fail("publish.required_checks", `must be "branch-protection", "none" or a list of check names`)
			}
			break
		}
		p.RequiredChecks = trimAll(s.strList("required_checks", nil))
		if len(p.RequiredChecks) == 0 {
			b.cfg.Warnings = append(b.cfg.Warnings,
				"publish.required_checks is empty: no CI check gates the hand-off to review")
		}
	}
	b.strict(s)

	if !strings.HasSuffix(p.BranchPrefix, "/") || !validBranch(strings.TrimSuffix(p.BranchPrefix, "/")) {
		b.fail("publish.branch_prefix", `must be a valid branch prefix ending with "/"`)
	}
}

func (b *builder) policy(s *section) {
	p := &b.cfg.Policy
	p.DenyPaths = append(trimAll(s.strList("deny_paths", nil)), b.packs.Protect...)
	p.FlagPaths = trimAll(s.strList("flag_paths", nil))
	p.MaxChangedFiles = b.positive("policy.max_changed_files", s.integer("max_changed_files", 200))
	p.MaxDiffBytes = b.positive("policy.max_diff_bytes", s.integer("max_diff_bytes", 2097152))
	p.MaxFileBytes = b.positive("policy.max_file_bytes", s.integer("max_file_bytes", 1048576))
	p.AllowBinary = s.boolean("allow_binary", false)
	p.AllowSymlinks = s.boolean("allow_symlinks", false)
	p.TestPaths = trimAll(s.strList("test_paths", []string{
		"**/test/**", "**/tests/**", "**/*_test.*", "**/*.test.*", "**/*.spec.*", "**/Tests/**"}))
	p.SecretScan = s.boolean("secret_scan", true)
	b.strict(s)
}

func (b *builder) budget(s *section) {
	bg := &b.cfg.Budget
	bg.MaxFailureAttempts = b.positive("budget.max_failure_attempts", s.integer("max_failure_attempts", 3))
	bg.MaxRunsPerDay = b.positive("budget.max_runs_per_day", s.integer("max_runs_per_day", 20))
	bg.MaxTokensPerDay = s.optInteger("max_tokens_per_day", nil)
	defSeconds := int64(28800)
	bg.MaxAgentSecondsPerDay = s.optInteger("max_agent_seconds_per_day", &defSeconds)
	bg.QuotaBackoffMS = b.positive("budget.quota_backoff_ms", s.integer("quota_backoff_ms", 3600000))
	bg.StateDir = b.path("budget.state_dir", s.str("state_dir", ""))
	b.lenient(s)
	if bg.MaxTokensPerDay != nil {
		b.positive("budget.max_tokens_per_day", *bg.MaxTokensPerDay)
	}
	if bg.MaxAgentSecondsPerDay != nil {
		b.positive("budget.max_agent_seconds_per_day", *bg.MaxAgentSecondsPerDay)
	}
}

func (b *builder) worker(s *section) {
	w := &b.cfg.Worker
	w.Name = s.str("name", "")
	w.RequireRoutingLabel = s.boolean("require_routing_label", true)
	b.lenient(s)
	switch {
	case w.Name == "":
		b.fail("worker.name", "is required")
	case !workerNameRE.MatchString(w.Name):
		b.fail("worker.name", "must be lowercase letters, digits and hyphens")
	}
}

func (b *builder) server(s *section) {
	b.cfg.Server.Port = s.optInteger("port", nil)
	b.lenient(s)
	if p := b.cfg.Server.Port; p != nil && (*p < 0 || *p > 65535) {
		b.fail("server.port", "must be between 0 and 65535")
	}
}

// crossChecks validates rules that span sections.
func (b *builder) crossChecks(workflowPath string) {
	c := b.cfg

	// The routing label is appended to the required labels (SPEC 5.3.12).
	if c.Worker.RequireRoutingLabel && c.Worker.Name != "" {
		c.Tracker.RequiredLabels = normalizeLabels(append(c.Tracker.RequiredLabels, "run:"+c.Worker.Name))
	}

	// A permissive permission mode is acceptable only inside a real sandbox.
	// Every supported backend is one, so this guards against a future
	// "none"-like backend slipping in.
	if c.Claude.PermissionMode != "plan" && !backends[c.Sandbox.Backend] {
		b.fail("claude.permission_mode", "requires a sandbox backend")
	}

	// The branch prefix must not be able to match the base branch.
	if c.Workspace.BaseBranch != "" && c.Publish.BranchPrefix != "" &&
		strings.HasPrefix(c.Workspace.BaseBranch+"/", c.Publish.BranchPrefix) {
		b.fail("publish.branch_prefix", "must not match the base branch %q", c.Workspace.BaseBranch)
	}

	// The workspace root and the state directory must be separate trees.
	if c.Workspace.Root != "" && c.Budget.StateDir != "" {
		if within(c.Workspace.Root, c.Budget.StateDir) || within(c.Budget.StateDir, c.Workspace.Root) {
			b.fail("budget.state_dir", "must not contain or be inside workspace.root")
		}
	}

	// The workflow file is trusted configuration and must not live where the
	// agent can write (SPEC 5.1). Ownership is checked at startup. The
	// location is checked here.
	if workflowPath != "" && c.Workspace.Root != "" {
		abs, err := filepath.Abs(workflowPath)
		if err == nil && within(abs, c.Workspace.Root) {
			*b.errs = append(*b.errs, &workflow.Error{
				Class: workflow.ClassPathUntrusted,
				Msg:   "the workflow file must not be inside workspace.root",
			})
		}
	}
}

// within reports whether path is dir or inside it, comparing path components.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
