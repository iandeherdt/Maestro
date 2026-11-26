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

// CheckDir is the folder next to the repository where the agent finds the
// pictures of a check it asked for (SPEC 13.6).
const CheckDir = "check-screenshots"

// writePictureScript empties a folder next to the repository ("clear") or
// writes one picture into it from standard input. The folder and the names
// are the service's own.
const writePictureScript = `set -u
d="$(dirname "$PWD")/$1"
if [ "$2" = clear ]; then rm -rf "$d" && mkdir -p "$d"; exit $?; fi
cat > "$d/$2"
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
	if !g.writePicture(ctx, ws.Key, CurrentDir, "clear", nil) {
		return worker.CurrentState{Note: "The pictures of the current state could not be given to the agent."}
	}
	out := worker.CurrentState{Note: note}
	for i, s := range shots {
		name := strconv.Itoa(i+1) + ".png"
		if !g.writePicture(ctx, ws.Key, CurrentDir, name, s.PNG) {
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

// GiveCheckPictures puts the pictures of a check the agent asked for where
// it can open them, and returns their paths from the repository.
func (g *Gates) GiveCheckPictures(ctx context.Context, ws workspace.Workspace, shots []worker.Screenshot) ([]worker.CurrentFile, string) {
	if !g.writePicture(ctx, ws.Key, CheckDir, "clear", nil) {
		return nil, "The pictures could not be given to you."
	}
	var out []worker.CurrentFile
	for i, s := range shots {
		name := strconv.Itoa(i+1) + ".png"
		if !g.writePicture(ctx, ws.Key, CheckDir, name, s.PNG) {
			return out, "Some pictures could not be given to you."
		}
		out = append(out, worker.CurrentFile{Caption: s.Caption, Path: "../" + CheckDir + "/" + name})
	}
	return out, ""
}

var _ worker.CheckPictures = (*Gates)(nil)

func (g *Gates) writePicture(ctx context.Context, key, dir, name string, png []byte) bool {
	sctx, cancel := context.WithTimeout(ctx, collectTime)
	defer cancel()
	res, err := g.Launcher.Run(sctx, sandbox.Request{
		Step: sandbox.StepVerify, WorkspaceKey: key, Argv: []string{"sh", "-c", writePictureScript, "sh", dir, name},
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
