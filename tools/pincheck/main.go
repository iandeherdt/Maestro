// Command pincheck reports the tool versions that the sandbox image and the
// convention packs pin, and whether a newer release of each exists. The
// scheduled workflow .github/workflows/pin-updates.yml turns its report
// into an issue.
//
//	go run ./tools/pincheck [-root .] [-report report.md] [-list]
//
// The exit status is 0 when every lookup got an answer, whether or not a
// pin is behind, and 1 when a lookup failed. When GITHUB_OUTPUT is set, the
// counts outdated and failed are written to it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/iandeherdt/Maestro/internal/pins"
)

func main() {
	root := flag.String("root", ".", "the repository to scan")
	report := flag.String("report", "", "write the Markdown report to this file")
	list := flag.Bool("list", false, "only list the pins found, without looking anything up")
	flag.Parse()

	found, err := pins.Scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pincheck:", err)
		os.Exit(2)
	}
	if *list {
		for _, p := range found {
			fmt.Printf("%s %s %s", p.Kind, p.Name, p.Version)
			if p.Commit != "" {
				fmt.Printf(" (commit %s)", p.Commit)
			}
			fmt.Println()
			for _, pl := range p.Places {
				fmt.Printf("    %s:%d\n", pl.File, pl.Line)
			}
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	checker := pins.NewChecker()
	checker.Log = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "pincheck: "+format+"\n", args...)
	}
	results := checker.Check(ctx, found)

	repoURL := ""
	if s, r := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"); s != "" && r != "" {
		repoURL = s + "/" + r
	}
	text := pins.Report(results, repoURL, os.Getenv("GITHUB_SHA"))
	if *report != "" {
		if err := os.WriteFile(*report, []byte(text), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "pincheck:", err)
			os.Exit(2)
		}
	} else {
		fmt.Print(text)
	}
	outdated, failed := pins.Outdated(results), pins.Failed(results)
	fmt.Fprintf(os.Stderr, "pincheck: %d pins, %d with a newer release, %d not checked\n", len(results), outdated, failed)
	if out := os.Getenv("GITHUB_OUTPUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err == nil {
			fmt.Fprintf(f, "outdated=%d\nfailed=%d\n", outdated, failed)
			f.Close()
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}
