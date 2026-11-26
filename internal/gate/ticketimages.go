package gate

import (
	"context"
	"strconv"

	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/ticketimage"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// Pictures on the ticket (SPEC 13.5). The tracker adapter downloads them
// with the service's credentials; here they are put where the agent can
// look at them.

// TicketImagesDir is the folder next to the repository where the agent
// finds the ticket's pictures. It is outside the repository, so nothing in
// it is ever committed.
const TicketImagesDir = "ticket-images"

// writeTicketImageScript empties the folder ("clear") or writes one
// picture into it from standard input. The names are the service's own.
const writeTicketImageScript = `set -u
d="$(dirname "$PWD")/` + TicketImagesDir + `"
if [ "$1" = clear ]; then rm -rf "$d" && mkdir -p "$d"; exit $?; fi
cat > "$d/$1"
`

// TicketImages empties the folder and writes the pictures into it, in
// order. It returns the paths the agent finds them at, from the repository,
// and false when not every picture could be written.
func (g *Gates) TicketImages(ctx context.Context, ws workspace.Workspace, imgs []ticketimage.Image) ([]string, bool) {
	if !g.writeTicketImage(ctx, ws.Key, "clear", nil) {
		return nil, false
	}
	var paths []string
	for i, img := range imgs {
		name := strconv.Itoa(i+1) + ".png"
		if !g.writeTicketImage(ctx, ws.Key, name, img.PNG) {
			return paths, false
		}
		paths = append(paths, "../"+TicketImagesDir+"/"+name)
	}
	return paths, true
}

func (g *Gates) writeTicketImage(ctx context.Context, key, name string, png []byte) bool {
	sctx, cancel := context.WithTimeout(ctx, collectTime)
	defer cancel()
	res, err := g.Launcher.Run(sctx, sandbox.Request{
		Step: sandbox.StepVerify, WorkspaceKey: key, Argv: []string{"sh", "-c", writeTicketImageScript, "sh", name},
		NetworkProfile: sandbox.NetNone, Limits: sandbox.Limits{TimeoutMS: collectTime.Milliseconds()},
	}, png)
	return err == nil && res.ExitStatus == 0
}

var _ worker.TicketPictures = (*Gates)(nil)
