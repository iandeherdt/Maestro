# Symphony-beads

A hardened orchestrator that runs Claude Code on tickets from a GitHub or Linear board and opens
pull requests for a human to review.

- [docs/SPEC.md](docs/SPEC.md): the specification.
- [docs/PLAN.md](docs/PLAN.md): the roadmap and decisions.
- [docs/phase0.md](docs/phase0.md): what has been verified so far.

## Status

Not usable yet. The parts are being built and tested one by one, and nothing runs an agent so far.
Last updated 2026-11-21.

Built and tested:

| Part | State |
|---|---|
| Workflow file, configuration, live reload | Done |
| Scheduling: polling, claims, retries, budgets, pause | Done, against a fake board and a fake agent |
| Prompt construction with a strict template engine | Done |
| Attempt loop: turns, verify, publish, hand-off | Done, against fakes |
| Workspace manager and the request format for the sandbox | Done, against a fake launcher |
| GitHub board, state kept in labels | Done, run once against a real repository |
| Diff policy and secret scan | Done |
| Import of the agent's commits and diff computation | Done, against real Git |
| GitHub App tokens | Done, not yet tried against github.com |
| Log format and loopback status API | Done |

Not built yet:

| Part | Needs |
|---|---|
| Push and pull request | In progress |
| Checks gate and hand-off watch | |
| Wiring into `symphony run`, and the `status`, `pause`, `doctor` commands | |
| Claude Code runner | One real run on a machine with the CLI logged in |
| Sandbox and launcher for Linux | The RHEL server |
| Mac worker | The Mac |
| Linear board | |

The order of work and the decisions taken are in [docs/PLAN.md](docs/PLAN.md).

## Try what exists

```sh
go build ./cmd/symphony
./symphony check examples/WORKFLOW.example.md
go test -race ./...
```
