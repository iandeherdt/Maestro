package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/workflow"
)

// minimal is the smallest valid workflow front matter.
const minimal = `
tracker:
  kind: github
  allowed_authors: [owner]
workspace:
  root: /srv/agent/workspaces
  repo: owner/repo
sandbox:
  backend: systemd
  network:
    proxy: 127.0.0.1:3128
verify:
  command: make test
budget:
  state_dir: /srv/maestro/state
worker:
  name: linux
`

func build(t *testing.T, front string, env map[string]string) (*Config, error) {
	t.Helper()
	def, err := workflow.Parse([]byte("---\n" + front + "\n---\nprompt"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	def.Path = "/etc/maestro/WORKFLOW.md"
	return Build(def, Options{Env: func(k string) string { return env[k] }, HomeDir: "/home/op"})
}

func mustBuild(t *testing.T, front string) *Config {
	t.Helper()
	c, err := build(t, front, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return c
}

// with returns the minimal config with one line replaced or a block appended.
func with(replaceOld, replaceNew, appendBlock string) string {
	s := minimal
	if replaceOld != "" {
		if !strings.Contains(s, replaceOld) {
			panic("fixture does not contain " + replaceOld)
		}
		s = strings.Replace(s, replaceOld, replaceNew, 1)
	}
	return s + appendBlock
}

// keys returns the configuration keys named by the errors in err.
func keys(err error) map[string]string {
	out := map[string]string{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range j.Unwrap() {
				walk(c)
			}
			return
		}
		var we *workflow.Error
		if errors.As(e, &we) {
			out[we.Key] = we.Class
		}
	}
	walk(err)
	return out
}

func TestDefaults(t *testing.T) {
	c := mustBuild(t, minimal)

	eq := func(name string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("polling.interval_ms", c.Polling.IntervalMS, int64(30000))
	eq("hooks.timeout_ms", c.Hooks.TimeoutMS, int64(60000))
	eq("agent.max_concurrent_agents", c.Agent.MaxConcurrentAgents, int64(1))
	eq("agent.max_turns", c.Agent.MaxTurns, int64(8))
	eq("agent.max_retry_backoff_ms", c.Agent.MaxRetryBackoffMS, int64(300000))
	eq("claude.command", c.Claude.Command, "claude")
	eq("claude.model", c.Claude.Model, "opus")
	eq("claude.planning_model", c.Claude.PlanningModel, "")
	eq("claude.max_steps_per_turn", c.Claude.MaxStepsPerTurn, int64(150))
	eq("claude.permission_mode", c.Claude.PermissionMode, "bypassPermissions")
	eq("claude.turn_timeout_ms", c.Claude.TurnTimeoutMS, int64(3600000))
	eq("claude.turn_total_timeout_ms", c.Claude.TurnTotalTimeoutMS, int64(5400000))
	eq("claude.read_timeout_ms", c.Claude.ReadTimeoutMS, int64(30000))
	eq("claude.stall_timeout_ms", c.Claude.StallTimeoutMS, int64(300000))
	eq("claude.inner_sandbox", c.Claude.InnerSandbox, false)
	eq("sandbox.memory_max", c.Sandbox.MemoryMax, "6G")
	eq("sandbox.cpu_quota", c.Sandbox.CPUQuota, "300%")
	eq("sandbox.tasks_max", c.Sandbox.TasksMax, int64(2048))
	eq("verify.timeout_ms", c.Verify.TimeoutMS, int64(1800000))
	eq("verify.network", c.Verify.Network, false)
	eq("publish.branch_prefix", c.Publish.BranchPrefix, "maestro/")
	eq("publish.draft_until_green", c.Publish.DraftUntilGreen, true)
	eq("publish.required_checks from protection", c.Publish.RequiredChecksFromBranchProtection, true)
	eq("publish.rework_on_changes_requested", c.Publish.ReworkOnChangesRequested, true)
	eq("publish.rework_on_comments", c.Publish.ReworkOnComments, true)
	eq("publish.review_quiet_ms", c.Publish.ReviewQuietMS, int64(120000))
	eq("policy.max_changed_files", c.Policy.MaxChangedFiles, int64(200))
	eq("policy.secret_scan", c.Policy.SecretScan, true)
	eq("policy.allow_binary", c.Policy.AllowBinary, false)
	eq("policy.allow_symlinks", c.Policy.AllowSymlinks, false)
	eq("budget.max_failure_attempts", c.Budget.MaxFailureAttempts, int64(3))
	eq("budget.max_runs_per_day", c.Budget.MaxRunsPerDay, int64(20))
	eq("budget.quota_backoff_ms", c.Budget.QuotaBackoffMS, int64(3600000))
	eq("tracker.handoff_state", c.Tracker.HandoffState, "Human Review")
	eq("tracker.in_progress_state", c.Tracker.InProgressState, "In Progress")
	eq("tracker.blocked_state", c.Tracker.BlockedState, "Blocked")
	eq("tracker.done_state", c.Tracker.DoneState, "Done")
	eq("worker.require_routing_label", c.Worker.RequireRoutingLabel, true)

	if c.Budget.MaxTokensPerDay != nil {
		t.Errorf("budget.max_tokens_per_day = %d, want nil", *c.Budget.MaxTokensPerDay)
	}
	if c.Budget.MaxAgentSecondsPerDay == nil || *c.Budget.MaxAgentSecondsPerDay != 28800 {
		t.Errorf("budget.max_agent_seconds_per_day = %v, want 28800", c.Budget.MaxAgentSecondsPerDay)
	}
	if c.Server.Port != nil {
		t.Errorf("server.port = %d, want nil", *c.Server.Port)
	}
	if got := strings.Join(c.Tracker.ActiveStates, ","); got != "Todo,In Progress,Rework" {
		t.Errorf("active states = %q", got)
	}
	if got := strings.Join(c.Claude.DisallowedTools, ","); got != "WebFetch,WebSearch" {
		t.Errorf("disallowed tools = %q", got)
	}
	if got := strings.Join(c.Tracker.AllowedCommenters, ","); got != "owner" {
		t.Errorf("allowed_commenters = %q, want the allowed authors", got)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", c.Warnings)
	}
}

func TestRoutingLabelAppended(t *testing.T) {
	c := mustBuild(t, with("allowed_authors: [owner]",
		"allowed_authors: [owner]\n  required_labels: [' Maestro ', maestro, 'RUN:linux']", ""))
	if got := strings.Join(c.Tracker.RequiredLabels, ","); got != "maestro,run:linux" {
		t.Errorf("required labels = %q, want maestro,run:linux", got)
	}

	c = mustBuild(t, with("name: linux", "name: linux\n  require_routing_label: false", ""))
	if len(c.Tracker.RequiredLabels) != 0 {
		t.Errorf("required labels = %v, want none", c.Tracker.RequiredLabels)
	}
}

func TestBlankRequiredLabelIsKept(t *testing.T) {
	// A blank configured label must match no issue, so it must not be dropped.
	c := mustBuild(t, with("allowed_authors: [owner]", "allowed_authors: [owner]\n  required_labels: ['  ']", ""))
	if len(c.Tracker.RequiredLabels) != 2 || c.Tracker.RequiredLabels[0] != "" {
		t.Errorf("required labels = %q, want a blank entry kept", c.Tracker.RequiredLabels)
	}
}

func TestPathResolution(t *testing.T) {
	c, err := build(t, with("root: /srv/agent/workspaces", "root: $WS_ROOT", ""),
		map[string]string{"WS_ROOT": "/data/ws/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace.Root != "/data/ws" {
		t.Errorf("root = %q, want /data/ws", c.Workspace.Root)
	}

	c = mustBuild(t, with("state_dir: /srv/maestro/state", "state_dir: ~/state", ""))
	if c.Budget.StateDir != "/home/op/state" {
		t.Errorf("state_dir = %q, want /home/op/state", c.Budget.StateDir)
	}

	_, err = build(t, with("root: /srv/agent/workspaces", "root: $WS_ROOT", ""), nil)
	if _, ok := keys(err)["workspace.root"]; !ok {
		t.Errorf("unset variable accepted: %v", err)
	}
	_, err = build(t, with("root: /srv/agent/workspaces", "root: relative/dir", ""), nil)
	if _, ok := keys(err)["workspace.root"]; !ok {
		t.Errorf("relative path accepted: %v", err)
	}
}

func TestVarNotResolvedOutsideDocumentedFields(t *testing.T) {
	// sandbox.env values are literals, so a host secret cannot be passed into
	// the sandbox by configuration.
	c, err := build(t, with("backend: systemd", "backend: systemd\n  env: {FOO: $SECRET}", ""),
		map[string]string{"SECRET": "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.Env["FOO"] != "$SECRET" {
		t.Errorf("sandbox.env.FOO = %q, want the literal $SECRET", c.Sandbox.Env["FOO"])
	}
	// claude.command is never expanded either, and a "$" makes it invalid.
	_, err = build(t, minimal+"claude:\n  command: $CLAUDE_BIN\n", map[string]string{"CLAUDE_BIN": "/bin/sh"})
	if _, ok := keys(err)["claude.command"]; !ok {
		t.Errorf("claude.command with $ accepted: %v", err)
	}
}

func TestUnknownKeys(t *testing.T) {
	c := mustBuild(t, minimal+"future_extension:\n  a: 1\n")
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "future_extension") {
		t.Errorf("warnings = %v, want one about future_extension", c.Warnings)
	}
	c = mustBuild(t, minimal+"agent:\n  max_turn: 3\n")
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "agent.max_turn") {
		t.Errorf("warnings = %v, want one about agent.max_turn", c.Warnings)
	}

	// A typo in a control section must fail, not be ignored.
	strict := map[string]string{
		"sandbox.memory_maxx":         with("backend: systemd", "backend: systemd\n  memory_maxx: 1G", ""),
		"sandbox.network.allow_agnet": with("proxy: 127.0.0.1:3128", "proxy: 127.0.0.1:3128\n    allow_agnet: [a.example]", ""),
		"publish.branch_prefx":        minimal + "publish:\n  branch_prefx: x/\n",
		"policy.secret_scann":         minimal + "policy:\n  secret_scann: false\n",
	}
	for key, front := range strict {
		_, err := build(t, front, nil)
		if cls := keys(err)[key]; cls != workflow.ClassInvalidConfig {
			t.Errorf("%s: class = %q, want %q (err %v)", key, cls, workflow.ClassInvalidConfig, err)
		}
	}
}

func TestInvalid(t *testing.T) {
	tests := []struct {
		name    string
		front   string
		wantKey string
	}{
		{"missing tracker kind", with("kind: github\n", "", ""), "tracker.kind"},
		{"unsupported tracker kind", with("kind: github", "kind: jira", ""), "tracker.kind"},
		{"empty allowed authors", with("allowed_authors: [owner]", "allowed_authors: []", ""), "tracker.allowed_authors"},
		{"blank allowed authors", with("allowed_authors: [owner]", "allowed_authors: ['  ']", ""), "tracker.allowed_authors"},
		{"handoff state is active", with("kind: github", "kind: github\n  handoff_state: Todo", ""), "tracker.handoff_state"},
		{"handoff state is terminal", with("kind: github", "kind: github\n  handoff_state: done", ""), "tracker.handoff_state"},
		{"in-progress state not active", with("kind: github", "kind: github\n  in_progress_state: Doing", ""), "tracker.in_progress_state"},
		{"blocked state is active", with("kind: github", "kind: github\n  blocked_state: ' rework '", ""), "tracker.blocked_state"},
		{"done state not terminal", with("kind: github", "kind: github\n  done_state: Shipped", ""), "tracker.done_state"},
		{"state both active and terminal", with("kind: github", "kind: github\n  active_states: [Todo, In Progress, Done]", ""), "tracker.active_states"},
		{"poll interval too small", minimal + "polling:\n  interval_ms: 1000\n", "polling.interval_ms"},
		{"missing workspace root", with("  root: /srv/agent/workspaces\n", "", ""), "workspace.root"},
		{"missing repo", with("  repo: owner/repo\n", "", ""), "workspace.repo"},
		{"bad repo", with("repo: owner/repo", "repo: https://github.com/owner/repo", ""), "workspace.repo"},
		{"bad base branch", with("repo: owner/repo", "repo: owner/repo\n  base_branch: 'a..b'", ""), "workspace.base_branch"},
		{"zero hook timeout", minimal + "hooks:\n  timeout_ms: 0\n", "hooks.timeout_ms"},
		{"zero max turns", minimal + "agent:\n  max_turns: 0\n", "agent.max_turns"},
		{"non-integer max turns", minimal + "agent:\n  max_turns: many\n", "agent.max_turns"},
		{"shell in claude command", minimal + "claude:\n  command: 'claude; rm -rf /'\n", "claude.command"},
		{"claude command with args", minimal + "claude:\n  command: claude --bare\n", "claude.command"},
		{"bad permission mode", minimal + "claude:\n  permission_mode: yolo\n", "claude.permission_mode"},
		{"missing sandbox backend", with("  backend: systemd\n", "", ""), "sandbox.backend"},
		{"no sandbox backend 'none'", with("backend: systemd", "backend: none", ""), "sandbox.backend"},
		{"missing proxy", with("    proxy: 127.0.0.1:3128\n", "    allow_agent: []\n", ""), "sandbox.network.proxy"},
		{"proxy without port", with("proxy: 127.0.0.1:3128", "proxy: localhost", ""), "sandbox.network.proxy"},
		{"ip address in allowlist", with("proxy: 127.0.0.1:3128", "proxy: 127.0.0.1:3128\n    allow_hooks: [10.0.0.5]", ""), "sandbox.network.allow_hooks"},
		{"url in allowlist", with("proxy: 127.0.0.1:3128", "proxy: 127.0.0.1:3128\n    allow_agent: ['https://a.example/x']", ""), "sandbox.network.allow_agent"},
		{"bad memory size", with("backend: systemd", "backend: systemd\n  memory_max: lots", ""), "sandbox.memory_max"},
		{"missing verify command", with("command: make test", "command: '  '", ""), "verify.command"},
		{"branch prefix without slash", minimal + "publish:\n  branch_prefix: maestro\n", "publish.branch_prefix"},
		{"branch prefix matches base branch", with("repo: owner/repo", "repo: owner/repo\n  base_branch: main", "publish:\n  branch_prefix: main/\n"), "publish.branch_prefix"},
		{"bad required checks string", minimal + "publish:\n  required_checks: all\n", "publish.required_checks"},
		{"zero failure attempts", with("state_dir: /srv/maestro/state", "state_dir: /srv/maestro/state\n  max_failure_attempts: 0", ""), "budget.max_failure_attempts"},
		{"missing state dir", with("  state_dir: /srv/maestro/state\n", "  max_runs_per_day: 5\n", ""), "budget.state_dir"},
		{"state dir inside workspace root", with("state_dir: /srv/maestro/state", "state_dir: /srv/agent/workspaces/state", ""), "budget.state_dir"},
		{"workspace root inside state dir", with("root: /srv/agent/workspaces", "root: /srv/maestro/state/ws", ""), "budget.state_dir"},
		{"missing worker name", with("  name: linux\n", "  require_routing_label: true\n", ""), "worker.name"},
		{"bad worker name", with("name: linux", "name: 'Linux Box'", ""), "worker.name"},
		{"port out of range", minimal + "server:\n  port: 70000\n", "server.port"},
		{"section not a mapping", minimal + "agent: 5\n", "agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := build(t, tc.front, nil)
			if err == nil {
				t.Fatalf("accepted, want an error on %s", tc.wantKey)
			}
			if cls, ok := keys(err)[tc.wantKey]; !ok || cls != workflow.ClassInvalidConfig {
				t.Errorf("no %s error on key %q; got: %v", workflow.ClassInvalidConfig, tc.wantKey, err)
			}
		})
	}
}

func TestCredentialLikeSandboxEnvRejected(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "anthropic_auth_token", "CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_API_KEY", "GITHUB_TOKEN", "GH_TOKEN", "LINEAR_API_KEY"} {
		_, err := build(t, with("backend: systemd", "backend: systemd\n  env: {"+name+": x}", ""), nil)
		if _, ok := keys(err)["sandbox.env."+name]; !ok {
			t.Errorf("%s accepted in sandbox.env: %v", name, err)
		}
	}
	c := mustBuild(t, with("backend: systemd", "backend: systemd\n  env: {NODE_ENV: test, CLAUDE_CODE_USE_X: '1'}", ""))
	if c.Sandbox.Env["NODE_ENV"] != "test" {
		t.Errorf("harmless variable dropped: %v", c.Sandbox.Env)
	}
}

func TestWorkflowPathMustNotBeInsideWorkspaceRoot(t *testing.T) {
	def, err := workflow.Parse([]byte("---\n" + minimal + "\n---\nprompt"))
	if err != nil {
		t.Fatal(err)
	}
	for path, wantErr := range map[string]bool{
		"/srv/agent/workspaces/GH-1/WORKFLOW.md":    true,
		"/srv/agent/workspaces/WORKFLOW.md":         true,
		"/srv/agent/workspaces-other/WORKFLOW.md":   false, // string prefix, different directory
		"/etc/maestro/WORKFLOW.md":                  false,
		"/srv/agent/workspaces/../conf/WORKFLOW.md": false,
	} {
		def.Path = path
		_, err := Build(def, Options{})
		got := false
		var we *workflow.Error
		for _, e := range flatten(err) {
			if errors.As(e, &we) && we.Class == workflow.ClassPathUntrusted {
				got = true
			}
		}
		if got != wantErr {
			t.Errorf("%s: untrusted = %v, want %v (err %v)", path, got, wantErr, err)
		}
	}
}

func flatten(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

func TestPerStateConcurrency(t *testing.T) {
	c := mustBuild(t, minimal+"agent:\n  max_concurrent_agents_by_state:\n    ' In Progress ': 2\n    Rework: 0\n    Todo: many\n")
	if got := c.Agent.MaxConcurrentAgentsByState; len(got) != 1 || got["in progress"] != 2 {
		t.Errorf("by-state map = %v, want {in progress: 2}", got)
	}
	if len(c.Warnings) != 2 {
		t.Errorf("warnings = %v, want two about ignored entries", c.Warnings)
	}
}

func TestRequiredChecks(t *testing.T) {
	c := mustBuild(t, minimal+"publish:\n  required_checks: [build, ' test ']\n")
	if c.Publish.RequiredChecksFromBranchProtection || strings.Join(c.Publish.RequiredChecks, ",") != "build,test" {
		t.Errorf("required checks = %+v", c.Publish)
	}
	c = mustBuild(t, minimal+"publish:\n  required_checks: []\n")
	if c.Publish.RequiredChecksFromBranchProtection || len(c.Warnings) != 1 {
		t.Errorf("explicit empty list: fromProtection=%v warnings=%v", c.Publish.RequiredChecksFromBranchProtection, c.Warnings)
	}
	c = mustBuild(t, minimal+"publish:\n  required_checks: branch-protection\n")
	if !c.Publish.RequiredChecksFromBranchProtection {
		t.Error("branch-protection string not honoured")
	}
}

func TestNullableBudgets(t *testing.T) {
	c := mustBuild(t, with("state_dir: /srv/maestro/state",
		"state_dir: /srv/maestro/state\n  max_agent_seconds_per_day: null\n  max_tokens_per_day: 5000000", ""))
	if c.Budget.MaxAgentSecondsPerDay != nil {
		t.Errorf("explicit null not honoured: %d", *c.Budget.MaxAgentSecondsPerDay)
	}
	if c.Budget.MaxTokensPerDay == nil || *c.Budget.MaxTokensPerDay != 5000000 {
		t.Errorf("max_tokens_per_day = %v", c.Budget.MaxTokensPerDay)
	}
}

func TestAllErrorsReportedTogether(t *testing.T) {
	_, err := build(t, "tracker:\n  kind: github\n", nil)
	got := keys(err)
	for _, want := range []string{"tracker.allowed_authors", "workspace.root", "workspace.repo",
		"sandbox.backend", "sandbox.network.proxy", "verify.command", "budget.state_dir", "worker.name"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing error for %s; got %v", want, got)
		}
	}
}

func TestStallTimeoutMayBeDisabled(t *testing.T) {
	c := mustBuild(t, minimal+"claude:\n  stall_timeout_ms: 0\n")
	if c.Claude.StallTimeoutMS != 0 {
		t.Errorf("stall_timeout_ms = %d, want 0", c.Claude.StallTimeoutMS)
	}
}

func TestDockerBackend(t *testing.T) {
	docker := strings.Replace(with("backend: systemd", "backend: docker", ""), "  network:\n    proxy: 127.0.0.1:3128\n", "", 1)
	c := mustBuild(t, docker)
	if c.Sandbox.Backend != "docker" || c.Sandbox.Image != "maestro-sandbox:latest" || c.Sandbox.Proxy != "" {
		t.Errorf("%+v", c.Sandbox)
	}
	c = mustBuild(t, strings.Replace(docker, "backend: docker", "backend: docker\n  image: ghcr.io/me/box:1.2", 1))
	if c.Sandbox.Image != "ghcr.io/me/box:1.2" {
		t.Errorf("image = %q", c.Sandbox.Image)
	}
	for _, bad := range []string{"'--privileged'", "'a b'", "'box:'", "'Box'", "'x;y'"} {
		_, err := build(t, strings.Replace(docker, "backend: docker", "backend: docker\n  image: "+bad, 1), nil)
		if _, ok := keys(err)["sandbox.image"]; !ok {
			t.Errorf("image %s was accepted: %v", bad, err)
		}
	}
}

func TestRequiredChecksNone(t *testing.T) {
	c := mustBuild(t, minimal+"publish:\n  required_checks: none\n")
	if !c.Publish.NoChecks || c.Publish.RequiredChecksFromBranchProtection || len(c.Warnings) == 0 {
		t.Errorf("%+v, warnings %v", c.Publish, c.Warnings)
	}
	if c := mustBuild(t, minimal); c.Publish.NoChecks {
		t.Error("checks are not required by default")
	}
	if c := mustBuild(t, minimal+"publish:\n  required_checks: [test]\n"); c.Publish.NoChecks {
		t.Error("a list of checks switched the gate off")
	}
}

func TestPodmanBackend(t *testing.T) {
	c := mustBuild(t, strings.Replace(with("backend: systemd", "backend: podman", ""), "  network:\n    proxy: 127.0.0.1:3128\n", "", 1))
	if c.Sandbox.Backend != "podman" || c.Sandbox.Proxy != "" {
		t.Errorf("%+v", c.Sandbox)
	}
}

// A rule pack adds its checks, its hosts and its protected paths to what
// the file says. It never replaces or removes any of them (SPEC 23).
func TestConventionsAddToTheSettings(t *testing.T) {
	c := mustBuild(t, minimal)
	if len(c.Conventions) != 0 || c.Verify.Command != "make test" || c.Hooks.BeforeRun != "" || c.Hooks.TimeoutMS != 60000 {
		t.Fatalf("without packs: %+v %+v", c.Verify, c.Hooks)
	}

	c = mustBuild(t, with("", "", "conventions: [dotnet, typescript]\npolicy:\n  deny_paths: [\"infra/**\"]\nhooks:\n  before_run: echo mine\n"))
	if !slices.Equal(c.Conventions, []string{"dotnet", "typescript"}) {
		t.Fatalf("conventions %v", c.Conventions)
	}
	v := c.Verify.Command
	if !strings.Contains(v, "sh conventions/dotnet/verify.sh || exit $?") ||
		strings.Index(v, "conventions/dotnet/verify.sh") > strings.Index(v, "conventions/typescript/verify.sh") ||
		!strings.HasSuffix(v, "\nmake test") {
		t.Errorf("verify:\n%s", v)
	}
	if h := c.Hooks.BeforeRun; !strings.Contains(h, "sh conventions/typescript/prepare.sh || exit $?") || !strings.HasSuffix(h, "\necho mine") {
		t.Errorf("before_run:\n%s", h)
	}
	if c.Hooks.TimeoutMS != 900000 {
		t.Errorf("hook timeout %d", c.Hooks.TimeoutMS)
	}
	for _, want := range []string{"infra/**", "conventions/**", "Directory.Build.props", "eslint.config.*"} {
		if !slices.Contains(c.Policy.DenyPaths, want) {
			t.Errorf("deny_paths lacks %s: %v", want, c.Policy.DenyPaths)
		}
	}
	if !slices.Contains(c.Sandbox.AllowHooks, "api.nuget.org") || !slices.Contains(c.Sandbox.AllowHooks, "registry.npmjs.org") {
		t.Errorf("allow_hooks %v", c.Sandbox.AllowHooks)
	}

	// The placeholder setup writes for "no tests" gives way to the pack,
	// and a pack is enough: no command of one's own is needed.
	c = mustBuild(t, with("command: make test", "command: \"true\"", "conventions: [typescript]\n"))
	if strings.Contains(c.Verify.Command, "true") || !strings.Contains(c.Verify.Command, "conventions/typescript/verify.sh") {
		t.Errorf("verify:\n%s", c.Verify.Command)
	}
	// An explicit hook timeout is kept, and a host is not listed twice.
	c = mustBuild(t, with("    proxy: 127.0.0.1:3128", "    proxy: 127.0.0.1:3128\n    allow_hooks: [registry.npmjs.org]",
		"conventions: [typescript]\nhooks:\n  timeout_ms: 5000\n"))
	if c.Hooks.TimeoutMS != 5000 || len(c.Sandbox.AllowHooks) != 1 {
		t.Errorf("timeout %d, hosts %v", c.Hooks.TimeoutMS, c.Sandbox.AllowHooks)
	}

	_, err := build(t, with("", "", "conventions: [cobol]\n"), nil)
	if err == nil || !strings.Contains(err.Error(), `conventions: there is no conventions pack named "cobol"`) {
		t.Errorf("an unknown pack: %v", err)
	}
	if _, err := build(t, with("", "", "conventions: dotnet\n"), nil); err == nil {
		t.Error("a pack name that is not a list was accepted")
	}
}

func TestScreenshots(t *testing.T) {
	if c := mustBuild(t, minimal); c.Screenshots.On() {
		t.Fatalf("on without being asked for: %+v", c.Screenshots)
	}
	c := mustBuild(t, with("", "", "screenshots:\n  pages: [index.html, \"docs/a b.html\", \"http://localhost:3000/\", \"http://127.0.0.1:8080/x?y=1\"]\n  start: npm run start\n"))
	if s := c.Screenshots; !s.On() || len(s.Pages) != 4 || s.Start != "npm run start" || s.Width != 1280 || s.Height != 800 || s.TimeoutMS != 300000 {
		t.Fatalf("%+v", s)
	}
	if c := mustBuild(t, with("", "", "screenshots:\n  command: node shots.mjs\n  timeout_ms: 1000\n")); !c.Screenshots.On() || c.Screenshots.Command != "node shots.mjs" || c.Screenshots.TimeoutMS != 1000 {
		t.Fatalf("%+v", c.Screenshots)
	}
	for name, block := range map[string]string{
		"another machine":     "pages: [\"https://example.com/\"]",
		"a name on the lan":   "pages: [\"http://nas.local/\"]",
		"localhost lookalike": "pages: [\"http://localhost.example.com/\"]",
		"a file address":      "pages: [\"file:///etc/passwd\"]",
		"outside":             "pages: [\"../secret.html\"]",
		"absolute":            "pages: [\"/etc/passwd\"]",
		"an option":           "pages: [\"--remote-debugging-port=9222\"]",
		"not clean":           "pages: [\"a/../../b.html\"]",
		"too many":            "pages: [a, b, c, d, e, f, g]",
		"both":                "pages: [a.html]\n  command: x",
		"start alone":         "start: npm start",
		"a typo":              "page: [a.html]",
		"too wide":            "pages: [a.html]\n  width: 9000",
	} {
		if _, err := build(t, with("", "", "screenshots:\n  "+block+"\n"), nil); err == nil || !strings.Contains(err.Error(), "screenshots") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestModelFor(t *testing.T) {
	c := Claude{Model: "sonnet", PlanningModel: "opus"}
	for _, tc := range []struct {
		labels   []string
		planning bool
		model    string
		refused  string
	}{
		{nil, false, "sonnet", ""},
		{nil, true, "opus", ""},
		{[]string{"bug", "model:haiku"}, false, "haiku", ""},
		{[]string{"model:haiku"}, true, "haiku", ""},
		{[]string{"model:claude-opus-4-5[1m]"}, false, "claude-opus-4-5[1m]", ""},
		{[]string{"model:--dangerously-skip-permissions"}, false, "sonnet", "--dangerously-skip-permissions"},
		{[]string{"model:opus 4"}, false, "sonnet", "opus 4"},
		{[]string{"model:"}, false, "sonnet", ""},
	} {
		if m, r := c.ModelFor(tc.labels, tc.planning); m != tc.model || r != tc.refused {
			t.Errorf("%v planning=%v: %q, %q", tc.labels, tc.planning, m, r)
		}
	}
	// With a list, only what is on it.
	c.CardModels = []string{"sonnet", "haiku"}
	if m, r := c.ModelFor([]string{"model:opus"}, false); m != "sonnet" || r != "opus" {
		t.Errorf("not on the list: %q %q", m, r)
	}
	if m, r := c.ModelFor([]string{"model:haiku"}, false); m != "haiku" || r != "" {
		t.Errorf("on the list: %q %q", m, r)
	}
	// Nothing configured and nothing asked: Claude Code decides.
	if m, r := (Claude{}).ModelFor(nil, true); m != "" || r != "" {
		t.Errorf("no model anywhere: %q %q", m, r)
	}
}

func TestSandboxToolsAndBinaryPaths(t *testing.T) {
	c := mustBuild(t, with("  backend: systemd\n", "  backend: systemd\n  tools: [blender]\n",
		"policy:\n  binary_paths: [\"Assets/Art/Kit/**\"]\n  binary_max_file_bytes: 1000\n"))
	if !slices.Equal(c.Sandbox.Tools, []string{"blender"}) {
		t.Errorf("tools %v", c.Sandbox.Tools)
	}
	if !slices.Equal(c.Policy.BinaryPaths, []string{"Assets/Art/Kit/**"}) || c.Policy.BinaryMaxFileBytes != 1000 {
		t.Errorf("policy %+v", c.Policy)
	}
	c = mustBuild(t, minimal)
	if len(c.Policy.BinaryPaths) != 0 || c.Policy.BinaryMaxFileBytes != 52428800 {
		t.Errorf("defaults %+v", c.Policy)
	}
	_, err := build(t, with("  backend: systemd\n", "  backend: systemd\n  tools: [maya]\n", ""), nil)
	if _, ok := keys(err)["sandbox.tools"]; !ok {
		t.Errorf("an unknown tool: %v", err)
	}
}
