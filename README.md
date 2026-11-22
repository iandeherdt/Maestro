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
- **From idea to done, not only from ticket to code.** Planning, your answers, a design to look
  at before anything is built, rework when you ask for changes, and closing the card after the
  merge are all part of it.
- **Rules that tools enforce.** Rule packs for .NET and TypeScript are checked by linters,
  analyzers and coverage, in places the agent cannot edit.
- **Your machines.** A Mac and a home server can share one board, and each card says which one
  does the work.
- **One guided setup.** `maestro setup` walks you through every step.

It follows the [Symphony specification](https://github.com/openai/symphony) published by OpenAI,
whose reference setup pairs Linear with Codex. Maestro is a separate implementation for GitHub and
Claude Code, built for one person or a small team that trusts each other, running agents on their
own hardware. Each machine works under the Claude subscription of the person who signed it in.

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
6. Once CI is green the card moves to **Human Review**. You review and merge. To have something
   changed, write a comment on the pull request: the card goes back to work by itself.

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
| What you run yourself was pointed out first | Once you merge and run the code, it runs with your rights, and the sandbox no longer helps. So the pull request starts with what deserves a look: added code that opens a network connection, starts another program, runs code built at run time or holds a long encoded block; changed dependencies and build scripts; weakened tests. Compiled files are refused outright. This helps you review. It does not replace reading the diff. |
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
  be in a GitHub organization. Everyone who can edit its board can approve work, so keep that to
  people you trust with it. Creating an organization is free: on github.com
  press **+** at the top right, **New organization**, and pick the Free plan. Then create the
  repository there, or move an existing one with Settings, **Transfer ownership**.

## Install

Install the binary on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/iandeherdt/Maestro/main/install.sh | sh
```

The script downloads the latest release for your machine, checks it against the release's
checksum file, and puts `maestro` in `~/.local/bin`. It does not use `sudo`. To read it before
running it, open [install.sh](install.sh). To choose a version or a directory, set
`MAESTRO_VERSION=v0.1.0` or `MAESTRO_INSTALL_DIR=/some/dir` before `sh`.

Or build it yourself with Go 1.26 or newer:

```sh
git clone https://github.com/iandeherdt/Maestro.git
cd Maestro
go build -o ~/.local/bin/maestro ./cmd/maestro
```

Then run the guided setup. It explains each step, waits for you before it changes anything, and
can be stopped and run again at any time:

```sh
maestro setup
```

In a terminal it shows each step with its progress, answers yes and no with one key, and lets you
pick from a list with the arrow keys. When its output goes to a file or another program, or when
the `NO_COLOR` environment variable is set, it prints plain lines instead.

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
   Board, name it Maestro) and waits. Then it gives the board the columns
   Backlog, Idea, Planned, Todo, In Progress, Human Review, Rework, Blocked and Done, and a field named Worker
   that says which machine takes a card. If the organization already has a board you can pick
   that one. For a repository on a personal account this step creates the labels instead.
5. **Writes the settings file** from two questions (how you test the repository, and whether it
   runs checks on pull requests), shows it to you first, and validates it.

If the machine has no browser, the setup shows an `ssh -L` command to run on your own computer so
that the page opens there.

Three commands are left, in this order, with Docker running:

```sh
maestro image    # builds the container the agent works in. A few minutes, once.
maestro login    # signs Claude Code in inside that container. Once.
maestro run      # starts the service. Stop it with Ctrl-C.
```

`maestro login` shows a link. Open it in your browser, approve, and paste the code it gives you
back into the terminal. The login is kept in a Docker volume that only the agent's containers
get. It is not written to your files or your keychain, and no API key is involved.

## Use

While `maestro run` is running, it looks at the board every 30 seconds.

If the board opens as a table, click the small arrow next to the name of the view and choose
**Board** under Layout. You do that once. The board has ten columns, so some are off the right
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
| Design | Optional. An agent makes a design of the plan for you to look at, see [See it before it is built](#see-it-before-it-is-built-a-design) |
| Planned | You approved the plan and its tasks were created |
| Todo | Approved, waiting for a free slot |
| In Progress | An agent is working on it |
| Human Review | A pull request is open and its checks passed (or you told setup there are none). Your turn. |
| Rework | You asked for changes. The service moves the card here by itself when you comment on the pull request, then on to In Progress. You can also write a comment on the issue and move the card here yourself. The agent continues on the same branch and is given your comments. See [Asking for changes](#asking-for-changes). |
| Blocked | The service gave up after repeated failures. The status comment on the issue says why. |
| Done | The pull request was merged. |

### Asking for changes

When a card is in **Human Review**, you do not have to touch the board to get something changed.
Write on the pull request, in any of the three places GitHub offers:

- a comment in the conversation,
- a comment on a line of the diff,
- a review with a summary.

About two minutes after your last comment, the service moves the card back to work and writes
"Picking up the new comments" on the pull request, so you can see it was noticed. The wait is
there so that you can write several comments in a row and get one run for all of them. The agent
is given your comments, with the file and line for comments on the diff, and its changes arrive
as a new commit on the same pull request. The card then returns to **Human Review**.

Only comments by you and the other people in `tracker.allowed_commenters` count. A comment by
anyone else is ignored. Set `publish.rework_on_comments: false` to switch this off, and
`publish.review_quiet_ms` to change the wait.

### Screenshots on the pull request

For anything you look at, a picture tells you more than the diff. Add this to the settings at
the top of your `WORKFLOW.md`:

```yaml
screenshots:
  pages: [index.html]
```

and build the sandbox image again, because it needs a browser now:

```sh
maestro image
```

From then on every pull request has a **Screenshots** section with a picture of each page, taken
by the service itself after the checks passed. The agent has no part in it.

A page is a file in the repository, or an address on `localhost` when the site needs a server.
In that case say how to start it:

```yaml
screenshots:
  pages: ["http://localhost:3000/", "http://localhost:3000/about"]
  start: npm run start
```

Things to know:

- At most 6 pages. Each picture is 1280 by 800 pixels unless you set `width` and `height`.
- The pictures are taken without network, like the checks. A font or script that the page
  loads from another site is missing in the picture.
- The pictures are stored in your repository on a branch called `maestro/screenshots`, because
  GitHub gives a service no other place for them. That branch holds nothing else and you can
  delete it whenever you like.
- When a picture cannot be taken, the pull request is opened anyway and says why.
- If you have your own way to take pictures, for example a Playwright script, use
  `command: node shots.mjs` instead of `pages`. The command writes PNG files into the folder
  named by `$SCREENSHOT_DIR`.

### Choosing the model

Without any setting, every ticket is worked on with `opus`. You can decide it in three places,
and the more specific one wins:

1. **On the card.** The board has a field **Model** with the choices `fable`, `opus`, `sonnet`
   and `haiku`. Set it on a card and that ticket is planned and worked on with that model. Leave it
   empty for the default. With labels instead of a board, put a label such as `model:opus` on the
   issue.
2. **For planning**, in the settings file: `claude: {planning_model: opus}`. Planning is where a
   stronger model pays off most.
3. **The default**, in the settings file: `claude: {model: sonnet}`. It is `opus` when you set
   nothing.

`fable`, `opus`, `sonnet` and `haiku` are Claude Code's own names for its newest model of each
kind, so
they stay right when new models come out. A full model name works too: add it as a choice to the
Model field on the board (the three dots at the top right, Settings, Model).

If your board was made before this existed, run `maestro setup` again: it adds the field and
changes nothing else. To limit what a card may ask for, set `claude: {card_models: [sonnet,
haiku]}`; a card that asks for something else gets the default.

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

Before step 5 you can also ask for a design first, see the next section.

Moving the card to Planned is your approval of every task in the plan, so read them. Each task
then goes through the same steps as a ticket you wrote yourself and ends as a pull request for
you to review.

Anyone who can change the board can approve work, because GitHub does not record who moved a
card. Give edit access to the board only to people who may approve work, and read access to
everyone else.
Do not switch on a board automation that puts new cards in Todo by itself.

With labels instead of a board (a repository on a personal account), approve an issue by adding
`status:todo` and `run:<machine>`, and the service changes the `status:` label as the card
would move.

### See it before it is built: a design

Once a plan is posted, you can ask to see what it will look like before any of it is built. This
is optional: skip it and move the card to **Planned** as before.

1. Move the card with the plan to **Design**.
2. An agent makes a design for the plan, in the folder `design/GH-<number>/` of the repository:
   - for a website or app, mock-ups of the pages and screens the plan adds, in plain HTML and CSS;
   - for a game, a game design (the core loop, controls, rules, levels, art direction and colours)
     with mock screens of the main moments and, where it helps, a tiny playable sketch.

   Everything works offline from the files, with an `index.html` that links to every page and a
   `README.md` for the agents that build the tasks. It decides between the two from the
   repository; set `design: {kind: web}` or `design: {kind: game}` to tell it.
3. The service opens a pull request with the design and pictures of its pages, taken by the
   service itself without network. The card moves to **Human Review**.
4. Then:
   - **Like it?** Merge the pull request and move the card to **Planned**. The tasks are created
     and each one is told to follow the design, which is now in the repository.
   - **Want changes?** Comment on the pull request, on a line or in a review. A couple of
     minutes later the card goes back to **Design** by itself and the design is reworked on the
     same pull request. A comment on the issue works too: move the card back to **Design** yourself.
   - **Rather not?** Close the pull request and move the card to **Planned**. The tasks are
     created without a design.

While the design's pull request is open, a card in Planned waits for it and says so. The design
agent is held to the same rules as any other agent, and may only add files inside its folder.
It needs the browser in the sandbox image for the pictures: run `maestro image` once after you
upgrade. A board made before this existed has no **Design** column; setup tells you how to add
it, and nothing else changes.

What happens to a ticket, step by step: the service clones the repository into a fresh
container, runs Claude Code there on a branch named `maestro/GH-<number>`, runs your test
command itself, checks the changes against its rules, pushes the branch and opens a pull request.
The agent's container has no GitHub token and can reach only the Claude service. Merging is
always yours: merge the pull request and the ticket is closed, or request changes and the agent
continues.

If something goes wrong, the service writes why in a comment on the issue and in the terminal.

Day to day, from any terminal:

```sh
maestro status              # what is being worked on, what is waiting, today's usage
maestro pause on holiday    # finish what is running, start nothing new
maestro resume
```

To stop needing a terminal window:

```sh
maestro service install     # run in the background, start when you log in
maestro service uninstall   # back to running it by hand
```

It shows the file it will write and asks before it does. The background service runs as you, and
the agent still works in Docker, so Docker has to be running.

Not built yet: `maestro unblock` and `maestro doctor`. To give a blocked ticket another
chance, drag its card back to Todo (or put `status:todo` back on it).

## Let your own Claude Code talk to it

Your own Claude Code on the same machine (the one you chat with, not the agents in the
container) can ask Maestro what it is doing and pause it. Run:

```sh
maestro assistant
```

It does two things and says what is left:

1. It writes a skill to `~/.claude/skills/maestro/SKILL.md`. The skill tells Claude Code what
   Maestro is, how to write a ticket an agent can finish, what to look at when a ticket is stuck,
   and that approving work stays with you.
2. It prints one command for you to run, `claude mcp add --scope user maestro -- … mcp`, which
   gives Claude Code the tools. It also prints the same server as JSON, for another assistant
   that speaks MCP.

After that you can ask things like "what is Maestro doing?", "why is GH-12 stuck?", "pause
Maestro while I rebase" or "write a ticket for the login bug".

| Tool | What it does |
|---|---|
| `maestro_info` | The repository, the name of this machine, how tickets are approved, the column names |
| `maestro_status` | The same as `maestro status` |
| `maestro_ticket` | More about one ticket that is being worked on |
| `maestro_pause`, `maestro_resume` | The same as the commands |
| `maestro_refresh` | Look at the board now |
| `maestro_check` | Validate the settings file |

The tools talk to the running service on this machine only. They hold no GitHub token and there
is no tool that approves a ticket, merges a pull request or changes the settings. Tickets are
written with `gh` under your own account, in Backlog, and the skill tells the assistant to leave
moving a card to Todo to you. Text that tickets and working agents wrote is marked in every
answer as information, not instructions. To undo: `maestro assistant remove` and
`claude mcp remove maestro`.

## Give the agents skills

The agents read the repository they work in, including its `CLAUDE.md`. Knowledge that is not
in the repository, such as how your team reviews code, cuts a release or uses an internal tool,
can be given to them as **skills** and **agent files**. These are Claude Code's own formats: a
skill is a directory with a `SKILL.md`, an agent file is a Markdown file in a directory called
`agents`. Many are published as git repositories, and you can write your own.

```sh
maestro skills add owner/repo            # a repository on GitHub
maestro skills add owner/repo/skills/pdf # only one directory of it
maestro skills add owner/repo --ref v1.2 # a branch, a tag or a commit
maestro skills add https://git.example.com/team/skills.git
maestro skills add ./my-skills           # a directory on this machine
```

The command downloads the source, lists every skill and agent file it found with its description
and the programs it contains, and waits for your yes. Then:

- Every agent that starts from then on has them. The service does not need a restart.
- `maestro skills` lists what is installed, from where and at which commit.
- `maestro skills remove NAME` takes one away.
- Nothing updates by itself. To get a newer version, add the source again; you are shown what
  you get before it replaces the old one.

A skill is followed like your own instructions, so this is the one place where text from
outside gets that weight. That is why it works this way:

- Only you add skills, with a command on the machine. A ticket, a comment or an agent cannot.
- What you approved is copied and kept at that commit. A later change to the repository it came
  from does not reach your agents.
- The agents get the skills read-only, and what an agent writes into that place itself is never
  seen by a later run.
- A program that comes with a skill runs inside the agent's container, with the same limits as
  everything else the agent does. It never runs on your machine.
- A skill may only hold ordinary files, at most 200 of them and 5 MB.

A private repository works when `git` on this machine can already fetch it without asking for a
password.

## Conventions that tools enforce

A prompt that says "follow our conventions" is a wish. A rule pack makes the conventions of a
project hold: every rule in a pack names the tool that checks it, and the checks run where the
agent cannot skip them.

```sh
maestro conventions                 # the packs: blender, dotnet, godot, klad, typescript, unity
maestro conventions show dotnet     # every rule, and the tool that enforces it
```

| Pack | For | Rules, in short |
|---|---|---|
| `blender` | 3D models made by Python scripts that Blender runs without a screen | every generator runs without an error and writes something, every model loads back into Blender, an optional triangle limit. Comes with the skill `blender-art`. See [Generated art](#game-development) |
| `godot` | A Godot 4 project in GDScript, with GUT | the project on the image's Godot version, every script parses, gdformat, gdlint, every asset imported with its `.uid` and `.import` files, GUT tests pass. Comes with the skill `godot-dev`. See [Game development](#game-development) |
| `dotnet` | C# with one solution in the root | `dotnet format`, warnings as errors with the .NET analyzers and SonarAnalyzer, security rules, limits on file and method size, tests, 80% coverage per test project, no vulnerable packages |
| `unity` | A Unity project in the root, on the Unity version of the image | every asset has its `.meta` file and no GUID is used twice, assets saved as text, the project on the Editor's version, scripts compile in Unity, EditMode and PlayMode tests pass, packages changed by the owner only. Comes with the skill `unity-dev`. Needs a Unity licence: see [Game development](#game-development) |
| `klad` | 2D art painted with [klad](https://github.com/klad-app/klad) through its command line | every drawing exports, every committed picture is exactly what its drawing exports to now. Comes with the skill `klad-art`. See [Generated art](#game-development) |
| `typescript` | TypeScript with React, Next.js or TanStack, npm or pnpm | Prettier, ESLint without warnings (typescript-eslint strict, React, hooks, accessibility, SonarJS, security, the framework's own rules), limits on size and complexity, strict type check, tests with vitest, 80% coverage of all source files, no vulnerable packages |

A rule is enforced in three places:

1. **Before the push.** The service runs the pack's checks itself. What fails goes back to the
   agent, which fixes it and tries again.
2. **In CI.** The pull request runs the same script, plus the package audit. Make that check
   required on the main branch and nothing that breaks a rule can be merged, whoever wrote it.
3. **The rules themselves are locked.** The service refuses any change by the agent to the rule
   files, the scripts and the CI workflows. Without that, the cheapest way to pass a rule is to
   weaken it. You change a rule by hand.

Commit hooks are not used for this: the agent commits inside its sandbox and could skip them.

To add a pack to a project:

```sh
git clone <the project's address> && cd <the project>
maestro conventions apply typescript
```

It writes the files and then lists what is left, step by step: install the tools, check that the
project passes, commit and push yourself, make the CI check required on GitHub, and switch the
pack on for the service:

```sh
maestro conventions enable typescript    # on every machine that runs agents for the project
maestro image                            # once more, when the pack needs tools in the sandbox
```

The rules end up in the repository as `conventions/<pack>/RULES.md`, so the agent can read what
is expected and run the same checks while it works. Thresholds such as the coverage minimum are
at the top of `conventions/<pack>/verify.sh`.

Limits for now: an agent cannot add a new package by itself, because its sandbox does not reach
package registries. Add the dependency yourself, or the ticket fails its checks. Monorepos are
not covered, and the TypeScript pack measures coverage with vitest only.

## Game development

Maestro can run cards for games: the agent writes code, scenes and tests, and can make art when
the art is made by scripts or commands. Four rule packs cover it, each with checks the agent
cannot skip; pick the ones that fit the project.

| Pack | For | Needs a licence | The agent can |
|---|---|---|---|
| `unity` | A Unity project | Yes, Unity's (`maestro unity activate`) | write C#, scenes and prefabs as text, run the quick compile itself; Unity's tests run in the checks |
| `godot` | A Godot 4 project in GDScript | No | write GDScript, scenes and resources as text, run Godot, the linter and the tests itself |
| `blender` | 3D models made by Python scripts | No | write a generator, build it, look at its render |
| `klad` | 2D art painted with klad | No | paint through klad's commands, export, look at the picture |

What it does not do: play the game, judge how it feels, or run anything that needs a screen or a
GPU (the sandbox has neither; Blender renders on the CPU). Art drawn by hand stays yours: commit
it and the agent works with it. Git LFS is not supported yet, so files kept in LFS arrive in the
workspace as pointer files.

Each game pack comes with a skill. The agents get it from Maestro, next to the skills you added
with `maestro skills`, because the agent's Claude Code does not read skills from the repository.
The pack also writes the skill into the repository (`.claude/skills/`), so your own Claude Code
uses it in that project too. A skill of your own with the same name wins.

### Unity

The `unity` pack puts the Unity Editor for Linux (6000.3.29f1, about 8 GB) in the sandbox image.
Unity needs a licence to open a project and run its tests, so the pack also needs one:

```sh
maestro conventions enable unity && maestro image
maestro unity activate          # once, with a Unity account; or --file Unity_lic.ulf
maestro unity                   # whether this machine has a licence
```

The licence is kept on this machine and given read-only to the steps that run the project's own
scripts (preparing the project and the checks), never to the agent's container, and only to
repositories that have the `unity` pack enabled. The secret scan of a pull request also looks for
the licence's serial and signature, so an agent cannot copy it into a change it sends. Your password
is not kept. The checks do run code the agent wrote, and that code can read the licence: use a
Unity account that holds nothing else, such as a free Personal account made for this.

The agent has no licence, but it has the quick checks: `sh conventions/unity/check.sh` checks the
`.meta` files and compiles every script with the compiler that comes with the Editor. The skill
`unity-dev` tells the agent how to work without the editor: scenes and prefabs are edited as
text, every new asset gets a `.meta` file with a new GUID, packages are left to you.

Not yet run against Unity's servers: the activation, the package download in `prepare.sh` and the
tests with a real licence. Git LFS is not supported yet: files kept in LFS arrive in the
workspace as pointer files.

### Godot

The `godot` pack puts Godot 4.7.2 (from its GitHub release, checked against the published
checksums) and gdtoolkit in the sandbox image. Godot needs no licence and runs without a screen,
so the agent runs everything the checks run:

```sh
maestro conventions apply godot      # in a checkout; add GUT to addons/gut if it is not there
maestro conventions enable godot && maestro image
```

The checks: the project is made for the Godot version of the image, every script parses with
the project's classes known, `gdformat` and `gdlint` pass, every asset imports and its `.uid`
and `.import` files are committed, and the GUT tests under `test/` pass. The skill `godot-dev`
tells the agent how to work without the editor: scenes are edited as text, and Godot is run
with `--headless`. Only GDScript is covered; Godot's C# version is not.

### Art made by scripts: Blender and klad

Art that is made by scripts or by commands is code to Maestro: an agent can make a new piece,
look at it, and try again. Two packs set this up for any project, each with a skill that teaches
the agent the way of working, and checks the agent cannot skip:

- **`blender`**: one Python generator per model, which Blender runs without a screen. The pack
  adds Blender 5.0.1 to the image (from blender.org, checked against its checksum), the helper
  module `maestro_art` (export for Unity, a material, a preview render), and the skill
  `blender-art`.
- **`klad`**: drawings painted with [klad](https://github.com/klad-app/klad), each exported to a
  PNG file. The pack builds klad from its source into the image, and adds the skill `klad-art`,
  which starts the agent on klad's own skills (`klad skill`, `klad skill drawing`).

```sh
maestro conventions apply blender     # in a checkout of the project; then set the folders in
                                      # conventions/blender/settings.sh
maestro conventions enable blender && maestro image
```

As with the other game packs, the agents get the skill from Maestro, and the repository gets a
copy for your own Claude Code.

The models and pictures are binary files, which Maestro refuses by default. Name the one folder
they go into, and only there may the agent commit them:

```yaml
policy:
  binary_paths: ["art/models/**", "art/textures/**"]
  binary_max_file_bytes: 52428800     # per file there; the default
screenshots:
  command: sh conventions/blender/previews.sh   # renders of what the change touched, on the pull request
```

A text file in such a folder is read and limited like any other, so the secret scan still sees
it.

The Blender check builds everything into a throwaway folder, because an FBX export carries the
time it was made and Maestro fails a check that changes files. It fails when two generators
write the same file, and when a generator was changed but its committed models were not built
again (a glTF export is the same every time, so an unchanged one passes). klad renders the same
pixels on every machine, so its check compares the committed pictures with a fresh export and
fails when one is out of date. The pack pins klad's version, and the check fails when the klad
in the image is another one. A project with no generators or drawings yet passes.

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
  root: "/home/you/.config/maestro/workspaces"
  repo: your-organization/your-repo
sandbox:
  backend: docker
verify:
  command: |
    npm run lint
    npm test
budget:
  state_dir: "/home/you/.config/maestro/state"
worker:
  name: linux
---

Implement the issue below in this repository.

Follow the conventions in CLAUDE.md. Add or update tests for every behaviour you change.
```

Check a workflow file without starting anything:

```sh
maestro check examples/WORKFLOW.example.md
```

Every setting, with its default, is listed in [docs/SPEC.md](docs/SPEC.md), section 5. Commonly
changed ones:

| Setting | Default | What it does |
|---|---|---|
| `conventions` | none | Rule packs that are switched on, for example `[typescript]`. Set it with `maestro conventions enable` |
| `verify.command` | none | What the service runs to decide whether the work is acceptable. With a rule pack, the pack's checks run first and this may be left out |
| `screenshots.pages` | none | Pages the service takes a picture of for the pull request, see [Screenshots on the pull request](#screenshots-on-the-pull-request) |
| `publish.required_checks` | the repository's checks | `none` hands a pull request to you without waiting for CI. A list of names waits for exactly those. |
| `publish.rework_on_comments` | `true` | Your comments on a pull request send the card back to work, see [Asking for changes](#asking-for-changes) |
| `publish.review_quiet_ms` | `120000` | How long you have to be quiet on the pull request before that happens, in milliseconds |
| `sandbox.network.allow_hooks` | none | Hosts the `after_create` and `before_run` hooks may reach, for installing dependencies |
| `claude.model` | `opus` | The model for every ticket, see [Choosing the model](#choosing-the-model) |
| `claude.planning_model` | `claude.model` | The model while an idea is planned and while its design is made |
| `design.kind` | `auto` | What a design is: `web` (HTML and CSS pages), `game` (a game design with mock screens) or `auto` |
| `design.folder` | `design` | Where designs are kept in the repository, one folder per idea |
| `tracker.design_state` | `Design` | The column that asks for a design; `""` switches designs off |
| `claude.card_models` | any | The models a card may ask for |
| `agent.max_concurrent_agents` | 1 | How many tickets are worked on at once |
| `budget.max_failure_attempts` | 3 | Failed attempts before a ticket is blocked |
| `budget.max_runs_per_day` | 20 | Daily limit on agent runs |
| `policy.deny_paths` | built-in list | Extra paths the agent may never change |
| `policy.flag_paths` | none | Paths whose changes are pointed out in the pull request |

## Project status

Last updated 2026-11-22.

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
| Comments on a pull request send the card back to work | Written, tested against a fake GitHub API. Not yet run against github.com. |
| Screenshots on the pull request | Written. Taking the pictures is tested here with a real Chromium, storing them against a stand-in for GitHub. Not yet run through the service on a real project: the image with the browser has not been built with `maestro image`, and the pictures have not been seen in a pull request on github.com. |
| Container sandbox (isolated network, proxies, image) | Done, tested with real containers on Docker and on rootless Podman. The image with the real Claude Code is built in CI. Not yet run on RHEL with SELinux. |
| `maestro image`, `maestro login`, `maestro run` | Done. Tested in real containers against a stand-in for GitHub and for Claude Code, and run once on a Mac with the real Claude Code against github.com, from a labelled issue to a merged pull request. |
| `maestro status`, `pause`, `resume` | Done, tested against the running service |
| Planning stage (idea, questions, plan, tasks) | Done. Run once on a Mac with the real Claude Code against github.com: a plan was posted, changed after a comment, approved, and its tasks were created in Todo. Questions from the agent and tasks that depend on each other have only been tested against stand-ins. Needs the board. |
| Design stage (mock-ups or a game design between plan and tasks) | Written. Tested against stand-ins for the board, GitHub and Claude Code; the pictures are tested with a real Chromium and the design's commits with real Git. Not yet run against github.com. |
| Tools and a skill for your own Claude Code (`maestro assistant`, `maestro mcp`) | Written. The server is tested here by speaking the protocol to it against a stand-in for the running service. Not yet added to a real Claude Code. |
| Skills and agent files for the agents (`maestro skills`) | Written. Finding, checking, storing and removing them is tested, also fetching from a git repository. The read-only mount is tested with a real container in CI. Not yet tried with a real Claude Code run: that the agent picks a skill up from that place is taken from Claude Code's documentation. |
| Model per card, for planning and by default | Written and tested against stand-ins for the board and for Claude Code. Not yet run with a real card. |
| Pointing out code that reaches outside the program | Done. Patterns for C#, TypeScript and JavaScript, Go, Python, C and shell, tested on examples. A pattern list can be evaded by someone who knows it, so it is an aid for the reviewer and not a guarantee. |
| Conventions (rule packs `dotnet` and `typescript`) | Written. Every rule of both packs was run against a sample project in a container, without network, and shown to fail on a violation. The command and the settings are tested. Not yet run through the service on a real project, and the sandbox image with the .NET SDK has not been built with `maestro image` yet. |
| Unity (pack `unity`, `maestro unity`) | Written. The `.meta`, version and quick compile checks were run against a sample project in the Unity 6.3 image, and shown to fail on a violation. The Editor was shown to start under the sandbox's settings and stop for want of a licence. The licence is shown to reach the hooks and verify read-only and not the agent. Not yet run with a real licence, against Unity's servers, or in an image built with `maestro image`. |
| Packs `blender` and `klad`, binary folders (`policy.binary_paths`) | Written. Both packs were run under the sandbox's settings (no network, its user, no capabilities) against sample projects: Blender 5.0.1 with a good generator, one that raises, one that writes nothing, and a triangle limit; klad v0.4.0 (built from source here) with a missing, a current and an out-of-date picture. The image lines that download Blender and build klad have not been built with `maestro image`. |
| Pack `godot` | Written. Run under the sandbox's settings against a sample project with Godot 4.7.2, gdtoolkit 4.5.0 and GUT 9.7.1: a script that does not parse, unformatted code, a lint error, a passing and a failing test. The image line has not been built with `maestro image`. |
| Running in the background (`maestro service install`) | Written and tested as far as the file and the commands go. Not yet run on a real Mac or a real systemd. |
| `maestro unblock`, `doctor`, start-up self-test | Not started |
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

The project was first called Symphony-beads and its command `symphony`. [Coming from `symphony`](#coming-from-symphony) says what changes for an installation made under that name.

### Coming from `symphony`

Until release 0.4.0 the command was called `symphony`. An installation made under that name keeps
working: its settings, its GitHub App, its board and its tickets stay as they are. Do this once:

1. Stop the old service: Ctrl-C in its terminal, or `symphony service uninstall` if it runs in the
   background.
2. Install again with the command under [Install](#install). It puts `maestro` next to the old
   `symphony` and tells you how to remove the old copy.
3. `maestro image`, then `maestro login`. The container and the place that holds the Claude Code
   sign-in have new names, so both are made again.
4. `maestro run`, or `maestro service install`.

Two things to know:

- New branches start with `maestro/` and no longer with `symphony/`. A ticket that already has a
  pull request on a `symphony/` branch is not followed any more. Merge or close those first, or
  keep the old name with `publish: {branch_prefix: symphony/}` in the settings file.
- Setup keeps using the old settings directory when it exists, so nothing has to be moved.

## License

Apache License 2.0, see [LICENSE](LICENSE). [NOTICE](NOTICE) says where the design comes from.
