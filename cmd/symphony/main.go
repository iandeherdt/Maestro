// Command symphony runs the orchestrator described in docs/SPEC.md.
//
// Only configuration checking is implemented so far.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/prompt"
	"github.com/iandeherdt/Symphony-beads/internal/workflow"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" || len(args) > 2 {
		fmt.Fprintln(stderr, "usage: symphony check [path-to-WORKFLOW.md]")
		fmt.Fprintln(stderr, "The service itself is not implemented yet.")
		return 2
	}
	explicit := ""
	if len(args) == 2 {
		explicit = args[1]
	}
	def, err := workflow.Load(workflow.ResolvePath(explicit))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	home, _ := os.UserHomeDir()
	cfg, err := config.Build(def, config.Options{Env: os.Getenv, HomeDir: home})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Parse the prompt template and render it once with sample data, so that
	// an unknown variable, filter or tag is reported here and not at the
	// first dispatch.
	pb, err := prompt.New(def.PromptTemplate)
	if err == nil {
		_, err = pb.First(prompt.Input{
			Issue:      issue.Issue{ID: "sample", Identifier: "SAMPLE-1", Title: "Sample", State: "Todo"},
			WorkerName: cfg.Worker.Name, RepoName: cfg.Workspace.Repo,
			BaseBranch: cfg.Workspace.BaseBranch, Branch: cfg.Publish.BranchPrefix + "SAMPLE-1",
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prompt.RenderError, err)
		return 1
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	fmt.Fprintf(stdout, "workflow ok: tracker=%s repo=%s worker=%s required_labels=%v\n",
		cfg.Tracker.Kind, cfg.Workspace.Repo, cfg.Worker.Name, cfg.Tracker.RequiredLabels)
	return 0
}
