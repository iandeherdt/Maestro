package gate

import (
	"context"
	"strconv"

	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// Pictures of the current state (SPEC 27). Before a design agent starts,
// the service takes the project's own screenshots of the code as it is, so
// that the agent designs against what is really there, and the design's
// pull request can show the current state next to the design.

// CurrentDir is the folder next to the repository where the agent finds
// the pictures of the current state. It is outside the repository, so
// nothing in it is ever committed.
const CurrentDir = "current-screenshots"

// writeCurrentScript empties the folder ("clear") or writes one picture
// into it from standard input. The names are the service's own.
const writeCurrentScript = `set -u
d="$(dirname "$PWD")/` + CurrentDir + `"
if [ "$1" = clear ]; then rm -rf "$d" && mkdir -p "$d"; exit $?; fi
cat > "$d/$1"
`

// Current takes the pictures of the current state with the project's
// screenshots setting and puts them where the design agent can look at
// them. Without the setting there are none. Like the screenshots of a
// ticket, it never fails the work: what went wrong is in the note.
func (g *Gates) Current(ctx context.Context, ws workspace.Workspace) worker.CurrentState {
	g.mu.Lock()
	delete(g.current, ws.Key)
	g.mu.Unlock()
	sc := g.Config().Screenshots
	if !sc.On() {
		return worker.CurrentState{}
	}
	shots, note := g.shoot(ctx, ws.Key, sc)
	if len(shots) == 0 {
		return worker.CurrentState{Note: note}
	}
	if !g.writeCurrent(ctx, ws.Key, "clear", nil) {
		return worker.CurrentState{Note: "The pictures of the current state could not be given to the agent."}
	}
	out := worker.CurrentState{Note: note}
	for i, s := range shots {
		name := strconv.Itoa(i+1) + ".png"
		if !g.writeCurrent(ctx, ws.Key, name, s.PNG) {
			out.Note = "Some pictures of the current state could not be given to the agent."
			break
		}
		out.Shots = append(out.Shots, s)
		out.Files = append(out.Files, worker.CurrentFile{Caption: s.Caption, Path: "../" + CurrentDir + "/" + name})
	}
	g.mu.Lock()
	if g.current == nil {
		g.current = map[string][]worker.Screenshot{}
	}
	g.current[ws.Key] = out.Shots
	g.mu.Unlock()
	return out
}

func (g *Gates) writeCurrent(ctx context.Context, key, name string, png []byte) bool {
	sctx, cancel := context.WithTimeout(ctx, collectTime)
	defer cancel()
	res, err := g.Launcher.Run(sctx, sandbox.Request{
		Step: sandbox.StepVerify, WorkspaceKey: key, Argv: []string{"sh", "-c", writeCurrentScript, "sh", name},
		NetworkProfile: sandbox.NetNone, Limits: sandbox.Limits{TimeoutMS: collectTime.Milliseconds()},
	}, png)
	return err == nil && res.ExitStatus == 0
}

// currentOf is what Current gave the design attempt in this workspace.
func (g *Gates) currentOf(key string) []worker.Screenshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current[key]
}

var _ worker.CurrentPictures = (*Gates)(nil)
