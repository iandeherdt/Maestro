package publish

import (
	"context"

	"github.com/iandeherdt/Maestro/internal/worker"
)

// Screenshots go where the code host lets a change request show them
// (SPEC 24). On GitHub that is a branch of the repository that holds
// nothing else (see gitHub.storeShots).

// shotLink is one picture in the pull request description.
type shotLink struct{ caption, url string }

// shotBranch is the branch the pictures are kept on.
func shotBranch(prefix string) string { return prefix + "screenshots" }

// ScreenshotBranch is the branch the pictures are kept on. The mirror
// leaves it out, so that a workspace is not cloned with every picture ever
// taken.
func ScreenshotBranch(prefix string) string { return shotBranch(prefix) }

// storeFor stores pictures on the host of the repository in the settings.
func (p *Publisher) storeFor(ctx context.Context, key, head string, shots []worker.Screenshot) ([]shotLink, error) {
	h, err := p.host()
	if err != nil {
		return nil, err
	}
	return h.storeShots(ctx, key, head, shots)
}
