# Symphony-beads

Symphony-beads turns tickets into pull requests. You approve a ticket on GitHub, Claude Code works
on it inside a sandbox on your own machine, and you get a pull request to review. Nothing is merged
without you.

It is built for one person running agents on their own hardware with their own Claude
subscription, where the agent must not be able to do damage even if a ticket, a web page or a
dependency tries to trick it.

> **Status: under construction.** The building blocks are written and tested, but the service
> does not run an agent yet. `symphony setup` and `symphony check` work today. See
> [Project status](#project-status) for exactly what exists.

## How it works

1. You write an issue and approve it by adding two labels: `status:todo` and `run:linux`.
2. The service sees the ticket, checks that **you** wrote it and **you** applied the labels, and
   marks it `status:in progress`.
3. It creates a fresh copy of the repository in a sandbox and starts Claude Code there with the
   ticket as its task. The agent has no GitHub token, no access to your home network and no way to
   reach the service's own files.
4. When the agent says it is done, the service does not take its word. It runs your verify
   command (tests, lint) itself, then inspects the diff: no changes to CI configuration, no
   credentials, no oversized or binary files, no tampering with paths you protect.
5. If everything passes, the service pushes a branch and opens a pull request under its own
   GitHub identity. The pull request lists anything a reviewer should look at twice, such as
   removed tests or changed dependencies.
6. Once CI is green the ticket moves to `status:human review`. You review and merge, or comment
   and send it back with `status:rework`.

If the agent fails, the ticket is retried a limited number of times and then parked as
`status:blocked` with an explanation.

## What keeps it safe

| Rule | How it is enforced |
|---|---|
| The agent holds nothing worth stealing | It runs as a separate user in a sandbox with no tokens. Pushing and all GitHub calls are done by the service, outside the sandbox. |
| The agent's word is not trusted | The service runs the verify command, checks the diff and waits for CI before a ticket reaches you. |
| Nothing reaches the main branch without you | The service cannot merge or approve. It only opens pull requests, and only on branches under its own prefix. |
| Only you can start work | A ticket is picked up only if an allowlisted account wrote it and applied its labels. Comments from anyone else never reach the agent. |
| Cost is bounded | Per-ticket retry limit, daily run limits, and `pause`. |

The full threat model is in [docs/SPEC.md](docs/SPEC.md), section 18.

## Requirements

- Linux (RHEL 9 or compatible, in a VM) or macOS.
- Git 2.31 or newer.
- The [Claude Code CLI](https://code.claude.com/docs), logged in with your subscription.
- A GitHub repository you own. Private repositories work.

## Install

Install the binary on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/iandeherdt/Symphony-beads/main/install.sh | sh
```

The script downloads the latest release for your machine, checks it against the release's
checksum file, and puts `symphony` in `~/.local/bin`. It does not use `sudo`. To read it before
running it, open [install.sh](install.sh). To choose a version or a directory, set
`SYMPHONY_VERSION=v0.1.0` or `SYMPHONY_INSTALL_DIR=/some/dir` before `sh`.

Or build it yourself with Go 1.24 or newer:

```sh
git clone https://github.com/iandeherdt/Symphony-beads.git
cd Symphony-beads
go build -o ~/.local/bin/symphony ./cmd/symphony
```

Then run the guided setup. It explains each step, waits for you before it changes anything, and
can be stopped and run again at any time:

```sh
symphony setup
```

What the setup does today:

1. **Checks the machine** for Git, the Claude Code CLI and, on Linux, what the sandbox needs. For
   anything missing it tells you what to install.
2. **Creates a GitHub App for the service.** You open a local page, press one button on
   github.com, and the App's key is saved on your machine in a file only you can read. The App can
   open pull requests and read check results. It cannot change settings, workflows or secrets,
   and it cannot merge.
3. **Installs the App on one repository** of your choice and confirms that it can obtain a token
   for it.

4. **Creates the labels** in the repository, after showing you which ones are missing.
5. **Writes the workflow file** from your verify command, shows it to you first, and validates it.

Still to come in the setup: installing the service (systemd or launchd) and a final self-test.

If the machine has no browser, the setup shows an `ssh -L` command to run on your own computer so
that the page opens there.

## Use

Not available yet: this section describes how the finished service is used.

Approve a ticket by labelling an issue you wrote:

- `status:todo` approves it.
- `run:linux` or `run:mac` chooses the machine.

Then watch the labels change:

| Label | Meaning |
|---|---|
| `status:todo` | Approved, waiting for a free slot |
| `status:in progress` | An agent is working on it |
| `status:human review` | A pull request is open and CI is green. Your turn. |
| `status:rework` | You asked for changes. Your comments on the issue and the pull request go to the agent. |
| `status:blocked` | The service gave up after repeated failures. The status comment says why. |

Day to day:

```sh
symphony status            # what is running, what is queued, today's usage
symphony pause "on holiday"
symphony resume
symphony unblock GH-12     # give a blocked ticket another chance
symphony doctor            # check the installation and the repository's protection
```

## Configure

One file, `WORKFLOW.md`, holds the settings and the instructions for the agent. It lives on the
machine that runs the service, not in the repository the agent can change. The setup will write
it for you; this is what it looks like:

```markdown
---
tracker:
  kind: github
  provider:
    repo: your-account/your-repo
    state_source: labels
    bot_login: your-app[bot]
  allowed_authors: [your-account]
workspace:
  root: /var/lib/symphony-agent/workspaces
  repo: your-account/your-repo
verify:
  command: |
    npm run lint
    npm test
worker:
  name: linux
---

Implement the issue below in this repository.

Follow the conventions in CLAUDE.md. Add or update tests for every behaviour you change.
```

Check a workflow file without starting anything:

```sh
symphony check examples/WORKFLOW.example.md
```

Every setting, with its default, is listed in [docs/SPEC.md](docs/SPEC.md), section 5. Commonly
changed ones:

| Setting | Default | What it does |
|---|---|---|
| `verify.command` | none | What the service runs to decide whether the work is acceptable |
| `agent.max_concurrent_agents` | 1 | How many tickets are worked on at once |
| `budget.max_failure_attempts` | 3 | Failed attempts before a ticket is blocked |
| `budget.max_runs_per_day` | 20 | Daily limit on agent runs |
| `policy.deny_paths` | built-in list | Extra paths the agent may never change |
| `policy.flag_paths` | none | Paths whose changes are pointed out in the pull request |

## Project status

Last updated 2026-11-21.

| Part | State |
|---|---|
| Workflow file, configuration, live reload | Done |
| Scheduling: polling, claims, retries, budgets, pause | Done, tested against a fake board and a fake agent |
| Prompt construction | Done |
| Attempt loop: turns, verify, publish, hand-off | Done, tested against fakes |
| Workspace manager | Done, tested against a fake sandbox |
| GitHub board with labels | Done, run once against a real repository |
| Diff policy and secret scan | Done |
| Import of the agent's commits, push, pull request | Done, tested against real Git and a fake GitHub API |
| GitHub App tokens | Done, not yet tried against github.com |
| Install script and release downloads (macOS and Linux, Intel and ARM) | Done. The binaries are not signed or notarized. |
| Guided setup | Five of seven steps done (machine, App, installation, labels, workflow file), not yet tried against github.com |
| Checks gate (waiting for CI) | Done, tested against a fake GitHub API |
| Hand-off watch (merge closes the ticket, requested changes send it back) | Done, tested against a fake GitHub API |
| Running the service: `symphony run`, `status`, `pause`, `doctor` | Not started |
| Claude Code runner | Written from the CLI documentation and tested against a scripted agent. Needs one real run on a machine with the CLI logged in. |
| Linux sandbox | Not started. Needs the server. |
| Mac worker | Not started. Needs the Mac. |
| Linear board | Not started |

## Documentation

- [docs/SPEC.md](docs/SPEC.md): the full specification, including the security model.
- [docs/PLAN.md](docs/PLAN.md): the roadmap and the decisions taken so far.
- [docs/phase0.md](docs/phase0.md): which assumptions were checked, and how.

## Development

```sh
go test -race ./...
```

The tests need Git on the PATH. A live test of the GitHub adapter exists behind a build tag and
is described at the top of `internal/tracker/github/live_test.go`.

## Background

The design follows the specification of OpenAI's Symphony, adapted for Claude Code and hardened
for a setup where the board is reachable from the internet and the worker sits on a home network.
[docs/SPEC.md](docs/SPEC.md), Appendix D, lists the differences.
