# Symphony-beads

A hardened orchestrator that runs Claude Code on tickets from a GitHub or Linear board and opens
pull requests for a human to review.

- [docs/SPEC.md](docs/SPEC.md): the specification.
- [docs/PLAN.md](docs/PLAN.md): the roadmap and decisions.
- [docs/phase0.md](docs/phase0.md): what has been verified so far.

## Status

Early. What exists: the workflow file loader, configuration validation, the scheduling core
(polling, claims, retries, reconciliation, budgets) tested against a fake board and a fake agent,
prompt construction with a strict template engine, and the workspace manager, which talks to
the sandbox launcher through a narrow request format.
The service does not run anything yet: there is no sandbox, agent runner, board adapter or
publisher.

```sh
go build ./cmd/symphony
./symphony check examples/WORKFLOW.example.md
go test -race ./...
```
