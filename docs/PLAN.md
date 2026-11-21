# Symphony for Claude: plan

This is the roadmap. The design itself is specified in [SPEC.md](SPEC.md), which is normative and
written at the level of detail of OpenAI's Symphony specification, from which it is derived.

Status: in progress. Phase 1 is done, phases 2 and 5 are partly done, and nothing has run on the
owner's machines yet. The README lists what exists. This repository is public, so it never
contains secrets, host names or tokens.

## What is being built

A small daemon that watches a board, runs Claude Code on approved tickets inside a sandbox, checks
the result itself, and opens a pull request for the owner to review and merge.

- Board: an existing one. GitHub Issues with a GitHub Project first, Linear as a second adapter.
- Agent: the official `claude` CLI, logged in with the owner's subscription. The service never
  handles an Anthropic credential.
- Workers: a Linux server (RHEL host, KVM guest) and a Mac (Unity and Xcode work). Independent
  instances, routed by a `run:linux` or `run:mac` label, with no connection between them.
- Language: Go, one static binary per platform.

## The five rules the design rests on

1. The agent holds nothing worth stealing: no repository token, no board token, no route to the
   home network (SPEC 3.2, 11).
2. The agent's word is not trusted: the orchestrator runs the verify command, checks the diff and
   waits for CI before a ticket reaches Human Review (SPEC 7.4, 14, 15).
3. Nothing reaches the main branch without the owner's review (SPEC 15.7).
4. Only the owner can approve work, and only the owner's comments reach the agent (SPEC 12.3, 13.3).
5. Cost is bounded: failure budget per ticket, daily budgets, pause (SPEC 8.4, 8.7).

## Decisions made

1. Language: Go.
2. Linux worker: RHEL 8 or 9 host with libvirt, RHEL 9 compatible guest.
3. Pilot repository: created after the service exists, built to pass readiness (SPEC Appendix C).
4. Claude: subscription through a logged-in CLI.
5. Board: existing product, GitHub first, Linear second. Beads is not used as the board.
6. Mac worker: Unity and Xcode.
7. GitHub: repositories stay on the owner's personal account and the service uses a GitHub App
   for them. Ticket state is kept in labels (`state_source: labels`): the owner approves a
   ticket by adding the `status:todo` label. A Project board on a personal account was tested
   and dropped for now, because a token limited to Projects cannot see the issue behind a card
   in a private repository, and GitHub does not say who moved a card. Dragging cards remains
   possible later with an organization-owned Project (SPEC 12.5).
8. Claude Code runner: two runners behind one interface, selected by configuration. The pilot
   uses headless mode (`claude -p`), which is documented and reports structured events. Since
   2026-06-15 Anthropic bills headless use on a subscription from a separate monthly credit
   equal to the plan price (press reports, not confirmed in Anthropic's own documentation), with
   extra usage switched off so that work pauses when the credit is spent. A second runner that
   drives the interactive CLI in a terminal multiplexer is added in phase 4 after a test on a
   real machine. It is not a documented automation path and Anthropic may change how such
   sessions are treated, so it is the owner's choice per workflow, not the default.

9. Installation: one guided command, `symphony setup`, that explains each step, asks before it
   changes anything and can be run again at any point (SPEC 16.8). It replaces a manual runbook
   as the way to install. The binary itself will be a single download with a checksum.

## Phases

Each phase lands as one or more pull requests and is done when the listed tests pass.

Progress on 2026-11-21: phase 0 is done for everything that can be checked without a machine.
Phase 1 is done. Phase 2 is done for label mode, and Project mode is parked (decision 7). Phase 5
has the diff policy, the secret scan, the import step, the App tokens, the push and the pull
request, the checks gate and the hand-off watch; the restart probe is open. Phase 6 has five of the seven steps of the
guided setup. Phase 4 has the Claude Code runner, written from the documentation and not yet run
against the real CLI. The sandbox launcher and everything on the Mac need the owner's machines.

| Phase | Scope (SPEC sections) | Done when |
|---|---|---|
| 0. Verification spikes | Every `To verify` item in SPEC.md | Each is confirmed or the spec is corrected. Results recorded in `docs/phase0.md`. |
| 1. Spec core | 4 to 9, 12.1, 16.1, 16.5, 16.7 with a fake agent, fake tracker, no sandbox backend yet (test-only stub) | Test matrix 20.1, 20.2, 20.4, 20.8 |
| 2. GitHub adapter and board | 12.2 to 12.5, 16.4, Project and label setup | Test matrix 20.3. A ticket approved on the board moves across the columns with the fake agent. A ticket from another account is ignored. |
| 3. Sandbox, Linux | 11, launcher, proxy | Test matrix 20.6 on a RHEL 9 compatible VM |
| 4. Claude runner | 10, 13 | Test matrix 20.5. A real ticket produces commits on a scratch repository. |
| 5. Verify and publish | 14, 15, 17 | Test matrix 20.7 |
| 6. Linux deployment | Appendix A, `doctor`, `symphony setup` (16.8) | A fresh VM reaches a running service through `symphony setup` alone |
| 7. Linux pilot | Appendix C, 20.9 | The adversarial tickets end harmlessly. Two weeks at concurrency 1. |
| 8. Linear adapter | 12.6 | Test matrix 20.3 for Linear |
| 9. Mac worker | Appendix B | Test matrix 20.6 and 20.9 on the Mac with an Xcode and a Unity sample |

## Phase 0 status

The documentation checks are done and recorded in [phase0.md](phase0.md). SPEC.md was corrected
where an assumption turned out wrong. The main corrections:

- The service authenticates to GitHub as a GitHub App, not with a personal token.
- A GitHub Project owned by a personal account cannot be reached with least-privilege tokens.
  A test confirmed it for private repositories: a `project`-scope token sees the cards and not
  the issues behind them. Label mode works and was run against a real repository.
- Claude Code denies permission prompts automatically in print mode, so runs cannot stall.
- The Claude Code version is pinned, because a future default (`--bare`) would ignore the
  subscription login.

Still open, and needing a real machine or a real run: systemd properties on the guest, the macOS
guest with Xcode and Unity, one real `claude -p` run in the sandbox, one real push of a workflow
file, and the remaining items marked Open in phase0.md.

## Open questions for the owner

- Which board is used day to day once both adapters exist.
- Whether the macOS guest is acceptable for Unity, or the weaker dedicated-user backend is wanted.
- Whether to rename the repository, since Beads is no longer part of the design.
