# Maestro

**Maestro turns a GitHub board into a team of Claude Code agents that you can leave alone.** You
write an idea on a card. Maestro plans it with you in the comments, splits it into tasks, does the
work in a locked box on your own machines, and hands you a pull request that already passed its
checks. Nothing is merged without you.

What sets it apart:

- **The agent is not trusted.** It works in a sandbox with no token and no way to push. Maestro
  itself runs the tests, checks the diff against a policy, scans for secrets and opens the pull
  request. A ticket, a web page or a dependency that tries to trick the agent gets nowhere.
- **Your Claude subscription, not an API bill.** It drives the official `claude` command that you
  logged in to yourself. No key is passed around.
- **Only GitHub.** The board, the tickets, the comments and the pull requests all live there. No
  other tracker, no extra server, no database.
- **From idea to done, not only from ticket to code.** Planning, your answers, rework when you ask
  for changes, and closing the card after the merge are all part of it.
- **Rules that tools enforce.** Rule packs for .NET and TypeScript are checked by linters,
  analyzers and coverage, in places the agent cannot edit.
- **Your machines.** A Mac and a home server can share one board, and each card says which one
  does the work.
- **One guided setup.** `symphony setup` walks you through every step.

It follows the [Symphony specification](https://github.com/openai/symphony) published by OpenAI,
whose reference setup pairs Linear with Codex. Maestro is a separate implementation for GitHub and
Claude Code, built for one person running agents on their own hardware.

> **Status: early.** The service has taken a real ticket to a merged pull request on a Mac, with
> labels. The board and the planning stage described below have had one real run on a Mac: setup,
> an idea planned, the plan changed after a comment, and its tasks created in Todo. See [Project status](#project-status) for exactly what exists.

## How it works

1. You write an issue, put it on your board, say which machine should take it, and approve it by
   moving its card to the **Todo** column.
2. The service sees the card, checks that **you** wrote the issue, and moves the card to
   **In Progress**.
3. It creates a fresh copy of the repository in a sandbox and starts Claude Code there with the
   ticket as its task. The agent has no GitHub token, no access to your home network and no way to
   reach the service's own files.
4. When the agent says it is done, the service does not take its word. It runs your verify
   command (tests, lint) itself, then inspects the diff: no changes to CI configuration, no
   credentials, no oversized or binary files, no tampering with paths you protect.
5. If everything passes, the service pushes a branch and opens a pull request under its own
   GitHub identity. The pull request lists anything a reviewer should look at twice, such as
   removed tests or changed dependencies.
6. Once CI is green the card moves to **Human Review**. You review and merge, or
   move the card to **Rework** to have the agent continue.

If the agent fails, the ticket is retried a limited number of times and then parked in
**Blocked** with an explanation.

The board is a GitHub Project. GitHub only lets a service use a board that belongs to an
organization, so the repository has to live in one (a free organization is enough, see
[Install](#install)). For a repository on your personal account the service uses labels
instead: `status:todo`, `status:in progress` and so on. Everything else is the same.

## What keeps it safe

| Rule | How it is enforced |
|---|---|
| The agent holds nothing worth stealing | It runs as a separate user in a sandbox with no tokens. Pushing and all GitHub calls are done by the service, outside the sandbox. |
| The agent's word is not trusted | The service runs the verify command, checks the diff and waits for CI before a ticket reaches you. |
| Nothing reaches the main branch without you | The service cannot merge or approve. It only opens pull requests, and only on branches under its own prefix. |
| Only you can start work | A ticket is picked up only if an allowlisted account wrote it and it was approved: its card stands in Todo on a board only you can change, or you applied its labels. Comments from anyone else never reach the agent. |
| Cost is bounded | Per-ticket retry limit, daily run limits, and `pause`. |

The full threat model is in [docs/SPEC.md](docs/SPEC.md), section 18.

## Requirements

- macOS or Linux.
- Git 2.31 or newer.
- A container engine. The agent works inside containers. On a Mac:
  [Docker Desktop](https://www.docker.com/products/docker-desktop/). On Linux: Podman
  (`sudo dnf install podman` on RHEL and Fedora; it needs no root daemon) or Docker Engine.
- A Claude subscription. You sign in once, inside the container. Claude Code does not have to be
  installed on the machine itself.
- A GitHub repository you own. Private repositories work. For the board, the repository has to
  be in a GitHub organization that only you are a member of. Creating one is free: on github.com
  press **+** at the top right, **New organization**, and pick the Free plan. Then create the
  repository there, or move an existing one with Settings, **Transfer ownership**.

## Install

Install the binary on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/iandeherdt/Maestro/main/install.sh | sh
```

The script downloads the latest release for your machine, checks it against the release's
checksum file, and puts `symphony` in `~/.local/bin`. It does not use `sudo`. To read it before
running it, open [install.sh](install.sh). To choose a version or a directory, set
`SYMPHONY_VERSION=v0.1.0` or `SYMPHONY_INSTALL_DIR=/some/dir` before `sh`.

Or build it yourself with Go 1.24 or newer:

```sh
git clone https://github.com/iandeherdt/Maestro.git
cd Maestro
go build -o ~/.local/bin/symphony ./cmd/symphony
```

Then run the guided setup. It explains each step, waits for you before it changes anything, and
can be stopped and run again at any time:

```sh
symphony setup
```

What the setup does:

1. **Checks the machine** for Git and Docker or Podman. For anything missing it tells you what to install.
2. **Creates a GitHub App for the service.** You say which repository the agents work on, open a
   local page and press one button on github.com. The App's key is saved on your machine in a
   file only you can read. The App can open pull requests and read check results, and in an
   organization it can use the boards. It cannot change settings, workflows or secrets, and it
   cannot merge.
3. **Installs the App on one repository** of your choice and confirms that it can obtain a token
   for it.
4. **Sets up the board**, for a repository in an organization: it asks what this machine is
   called and what your GitHub user name is. GitHub lets only a person create a board, so setup
   asks you to make an empty one (open the organization's Projects page, press New project, pick
   Board, name it Symphony) and waits. Then it gives the board the columns
   Backlog, Idea, Planned, Todo, In Progress, Human Review, Rework, Blocked and Done, and a field named Worker
   that says which machine takes a card. If the organization already has a board you can pick
   that one. For a repository on a personal account this step creates the labels instead.
5. **Writes the settings file** from two questions (how you test the repository, and whether it
   runs checks on pull requests), shows it to you first, and validates it.

If the machine has no browser, the setup shows an `ssh -L` command to run on your own computer so
that the page opens there.

Three commands are left, in this order, with Docker running:

```sh
symphony image    # builds the container the agent works in. A few minutes, once.
symphony login    # signs Claude Code in inside that container. Once.
symphony run      # starts the service. Stop it with Ctrl-C.
```

`symphony login` shows a link. Open it in your browser, approve, and paste the code it gives you
back into the terminal. The login is kept in a Docker volume that only the agent's containers
get. It is not written to your files or your keychain, and no API key is involved.

## Use

While `symphony run` is running, it looks at the board every 30 seconds.

If the board opens as a table, click the small arrow next to the name of the view and choose
**Board** under Layout. You do that once. The board has nine columns, so some are off the right
edge of the screen: scroll sideways to see them. A card that seems to have vanished has usually
moved to a column further right, such as Human Review.

To give the service a ticket:

1. On the board, press **+ Add item** at the bottom of the Backlog column and create an issue in
   your repository. Write what you want done. Only issues that you wrote yourself are worked on.
2. Open the card and set its **Worker** field to the machine that should do it, for example
   `mac`.
3. Drag the card to **Todo**. That is the approval.

Then watch the card move:

| Column | Meaning |
|---|---|
| Backlog | Not approved. Nothing happens here. |
| Idea | An agent is planning it, see below |
| Planned | You approved the plan and its tasks were created |
| Todo | Approved, waiting for a free slot |
| In Progress | An agent is working on it |
| Human Review | A pull request is open and its checks passed (or you told setup there are none). Your turn. |
| Rework | You asked for changes. Write what should change in a comment on the issue, then move the card here. The agent continues on the same branch and is given your comments on the issue. (Review comments on the pull request itself are not passed on yet.) |
| Blocked | The service gave up after repeated failures. The status comment on the issue says why. |
| Done | The pull request was merged. |

### Something bigger: let it plan first

For an idea that is too big or too vague for one pull request, move the card to **Idea** instead
of Todo (set its Worker first).

1. An agent reads the repository and thinks about the idea. It changes nothing.
2. If it needs to know something, it asks. The questions appear as a comment on the issue and the
   card moves to **Human Review**. Answer in a comment and move the card back to **Idea**.
3. When it knows enough, it posts a plan: a list of small tasks, each with a full description,
   the machine that should do it, and what it depends on. The card moves to **Human Review**.
4. Not happy with the plan? Say so in a comment and move the card back to **Idea**.
5. Happy? Move the card to **Planned**. The tasks are created as issues exactly as shown, linked
   to the idea as sub-issues, and placed in **Todo**. The agents then work through them in the
   order the plan gives: a task waits until the tasks it depends on are done.

Moving the card to Planned is your approval of every task in the plan, so read them. Each task
then goes through the same steps as a ticket you wrote yourself and ends as a pull request for
you to review.

Anyone who can change the board can approve work, because GitHub does not record who moved a
card. Keep the organization to yourself, or give other members read access to the board only.
Do not switch on a board automation that puts new cards in Todo by itself.

With labels instead of a board (a repository on a personal account), approve an issue by adding
`status:todo` and `run:<machine>`, and the service changes the `status:` label as the card
would move.

What happens to a ticket, step by step: the service clones the repository into a fresh
container, runs Claude Code there on a branch named `symphony/GH-<number>`, runs your test
command itself, checks the changes against its rules, pushes the branch and opens a pull request.
The agent's container has no GitHub token and can reach only the Claude service. Merging is
always yours: merge the pull request and the ticket is closed, or request changes and the agent
continues.

If something goes wrong, the service writes why in a comment on the issue and in the terminal.

Day to day, from any terminal:

```sh
symphony status              # what is being worked on, what is waiting, today's usage
symphony pause on holiday    # finish what is running, start nothing new
symphony resume
```

To stop needing a terminal window:

```sh
symphony service install     # run in the background, start when you log in
symphony service uninstall   # back to running it by hand
```

It shows the file it will write and asks before it does. The background service runs as you, and
the agent still works in Docker, so Docker has to be running.

Not built yet: `symphony unblock` and `symphony doctor`. To give a blocked ticket another
chance, drag its card back to Todo (or put `status:todo` back on it).

## Configure

One file, `WORKFLOW.md`, holds the settings and the instructions for the agent. It lives on the
machine that runs the service, not in the repository the agent can change. The setup will write
it for you; this is what it looks like:

```markdown
---
tracker:
  kind: github
  provider:
    repo: your-organization/your-repo
    state_source: project          # the board; "labels" for a personal account
    project:
      number: 1                    # the number in the board's address
    bot_login: your-app[bot]
  allowed_authors: [your-user-name]
workspace:
  root: "/home/you/.config/symphony/workspaces"
  repo: your-organization/your-repo
sandbox:
  backend: docker
verify:
  command: |
    npm run lint
    npm test
budget:
  state_dir: "/home/you/.config/symphony/state"
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
| `publish.required_checks` | the repository's checks | `none` hands a pull request to you without waiting for CI. A list of names waits for exactly those. |
| `sandbox.network.allow_hooks` | none | Hosts the `after_create` and `before_run` hooks may reach, for installing dependencies |
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
| Tickets approved with labels | Done, run once against a real repository |
| Tickets approved on a board (GitHub Project of an organization) | Done. Run once against github.com on a Mac: a card was picked up from the board and moved by the service. |
| Export and verify steps, mirror sync | Done, tested against a fake sandbox and local Git |
| Egress proxy (allowlist of hosts the agent may reach) | Done, tested locally |
| Diff policy and secret scan | Done |
| Import of the agent's commits, push, pull request | Done, tested against real Git and a fake GitHub API |
| GitHub App tokens | Done, used once against github.com by the guided setup |
| Install script and release downloads (macOS and Linux, Intel and ARM) | Done. The binaries are not signed or notarized. |
| Guided setup | Five of seven steps done (machine, App, installation, board or labels, settings file). Run once on a Mac against github.com with labels. Also run once on an organization, with the board. |
| Checks gate (waiting for CI) | Done, tested against a fake GitHub API |
| Hand-off watch (merge closes the ticket, requested changes send it back) | Done, tested against a fake GitHub API |
| Container sandbox (isolated network, proxies, image) | Done, tested with real containers on Docker and on rootless Podman. The image with the real Claude Code is built in CI. Not yet run on RHEL with SELinux. |
| `symphony image`, `symphony login`, `symphony run` | Done. Tested in real containers against a stand-in for GitHub and for Claude Code, and run once on a Mac with the real Claude Code against github.com, from a labelled issue to a merged pull request. |
| `symphony status`, `pause`, `resume` | Done, tested against the running service |
| Planning stage (idea, questions, plan, tasks) | Done. Run once on a Mac with the real Claude Code against github.com: a plan was posted, changed after a comment, approved, and its tasks were created in Todo. Questions from the agent and tasks that depend on each other have only been tested against stand-ins. Needs the board. |
| Running in the background (`symphony service install`) | Written and tested as far as the file and the commands go. Not yet run on a real Mac or a real systemd. |
| `symphony unblock`, `doctor`, start-up self-test | Not started |
| Claude Code runner | Done, tested against a scripted agent and run once for real. |
| Native Linux sandbox (systemd) | Not started |
| Native Mac sandbox, for work that needs macOS itself (Xcode, Unity) | Not started |
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

The project was first called Symphony-beads. The command is still `symphony`.

## License

Apache License 2.0, see [LICENSE](LICENSE). [NOTICE](NOTICE) says where the design comes from.
