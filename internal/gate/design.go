package gate

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// DesignPublisher is the part of the publish gate a design needs.
type DesignPublisher interface {
	PublishDesign(ctx context.Context, is issue.Issue, ws workspace.Workspace, folder string, shots []worker.Screenshot, note string) (*plan.Design, *worker.Rejection, error)
}

// pagesOfScript lists the pages of a design: index.html first, then the
// other HTML files in the order of their path. Only names that are safe to
// pass on are printed; the service checks them again.
const pagesOfScript = `set -u
[ -d "$1" ] || exit 0
{
  [ -f "$1/index.html" ] && echo "$1/index.html"
  find "$1" -type f -name '*.html' ! -path "$1/index.html" 2>/dev/null | LC_ALL=C sort
} | head -n "$2"
`

var designPageRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+\.html$`)

// designPages finds the pages of a design to take pictures of.
func (g *Gates) designPages(ctx context.Context, key, folder string) ([]string, bool) {
	res, err := g.step(ctx, key, collectTime, "sh", "-c", pagesOfScript, "sh", folder, strconv.Itoa(config.MaxScreenshots))
	if err != nil || res.ExitStatus != 0 {
		return nil, false
	}
	var pages []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !designPageRE.MatchString(line) || !strings.HasPrefix(line, folder+"/") || strings.Contains(line, "..") {
			continue
		}
		pages = append(pages, line)
		if len(pages) == config.MaxScreenshots {
			break
		}
	}
	return pages, true
}

// Design takes pictures of a design and hands it to the publish gate
// (SPEC 27). The pictures are taken from exactly the exported commits,
// without network, like the screenshots of a ticket.
func (g *Gates) Design(ctx context.Context, is issue.Issue, ws workspace.Workspace, folder string) (*plan.Design, *worker.Rejection, error) {
	dp, ok := g.Publisher.(DesignPublisher)
	if !ok {
		return nil, nil, errDesignUnsupported
	}
	var shots []worker.Screenshot
	note := ""
	pages, ok := g.designPages(ctx, ws.Key, folder)
	switch {
	case !ok:
		note = "No pictures: the pages of the design could not be listed in the sandbox."
	case len(pages) == 0:
		note = "No pictures: the design has no HTML page."
	default:
		sc := g.Config().Screenshots
		cfg := config.Screenshots{Pages: pages, TimeoutMS: sc.TimeoutMS, Width: sc.Width, Height: sc.Height}
		shots, note = g.shoot(ctx, ws.Key, cfg)
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return dp.PublishDesign(ctx, is, ws, folder, shots, note)
}

var errDesignUnsupported = errors.New("the publish gate cannot publish designs")
