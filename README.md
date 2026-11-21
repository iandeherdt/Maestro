# Symphony-beads

A hardened orchestrator that runs Claude Code on tickets from a GitHub or Linear board and opens
pull requests for a human to review.

- [docs/SPEC.md](docs/SPEC.md): the specification.
- [docs/PLAN.md](docs/PLAN.md): the roadmap and decisions.
- [docs/phase0.md](docs/phase0.md): what has been verified so far.

## Status

Early. Only the workflow file loader and configuration validation exist. The service does not run
anything yet.

```sh
go build ./cmd/symphony
./symphony check examples/WORKFLOW.example.md
go test -race ./...
```
