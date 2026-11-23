// Package sandbox defines how the control zone asks for work to be done in
// the sandbox (SPEC section 11.2). The control zone never touches a
// workspace itself: it names a step and a workspace key, and the launcher,
// which is the only privileged code in the system, derives every path, user
// and unit property on its own.
package sandbox

import (
	"context"
	"fmt"
	"io"
)

// Step selects what the launcher runs. The launcher, not the caller,
// decides the unit properties for each step.
type Step string

const (
	StepPopulate Step = "populate"
	StepHook     Step = "hook"
	StepAgent    Step = "agent"
	StepVerify   Step = "verify"
	StepExport   Step = "export"
	// StepLoginStatus asks Claude Code in the sandbox whether it is
	// signed in, with the agent's state and network. It takes no
	// workspace and no command: the launcher runs `claude auth status`.
	StepLoginStatus Step = "login-status"
	// StepSelfTest runs the probe (SPEC 11.6) with one network profile in
	// a throwaway workspace. It takes no workspace and no command.
	StepSelfTest Step = "selftest"
)

// Network profiles. The launcher maps them to the proxy allowlists of the
// workflow. A step that does not name one gets no network.
const (
	NetNone  = "none"
	NetAgent = "agent"
	NetHooks = "hooks"
)

// Populate operations. Each is implemented inside the launcher with a fixed
// command line; the caller supplies only names.
const (
	// OpProbe reports whether the workspace exists. Standard output is
	// "missing", or "exists <base_sha>".
	OpProbe = "probe"
	// OpClone creates the workspace from the mirror on a new branch at the
	// tip of the base branch. Standard output is the base commit.
	OpClone = "clone"
	// OpFastForward moves the workspace branch forward to the published
	// branch in the mirror when that is strictly ahead. It never resets.
	OpFastForward = "fast-forward"
	// OpRemove deletes the workspace.
	OpRemove = "remove"
)

// Populate carries the arguments of a populate step.
type Populate struct {
	Op         string `json:"op"`
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// Limits bound a step. Zero values mean the launcher's defaults.
type Limits struct {
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// Request is the JSON document sent to the launcher. It carries no path,
// no user name and no unit property.
type Request struct {
	Step           Step              `json:"step"`
	WorkspaceKey   string            `json:"workspace_key"`
	Argv           []string          `json:"argv,omitempty"`
	Populate       *Populate         `json:"populate,omitempty"`
	NetworkProfile string            `json:"network_profile"`
	Limits         Limits            `json:"limits"`
	Env            map[string]string `json:"env,omitempty"`
}

// Result is what a finished step reports. Everything in it was produced by
// code running in the sandbox and is untrusted.
type Result struct {
	ExitStatus int
	Stdout     []byte
	Stderr     []byte
}

// Launcher runs one step and waits for it. An error means the step could
// not be run at all, or ctx ended; a step that ran and failed is reported
// through Result.ExitStatus. When ctx ends the whole unit must be killed.
type Launcher interface {
	Run(ctx context.Context, req Request, stdin []byte) (Result, error)
}

// Streamer runs one step like Launcher.Run, but delivers standard output to
// stdout while the step runs. Result.Stdout is empty. Writes to stdout may
// block; when one fails the step is killed.
type Streamer interface {
	Stream(ctx context.Context, req Request, stdin []byte, stdout io.Writer) (Result, error)
}

// LaunchFailed is the error category for a step that could not be started.
const LaunchFailed = "sandbox_launch_failed"

// Error is a launcher failure.
type Error struct{ Msg string }

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", LaunchFailed, e.Msg) }
