# Symphony for Claude: plan

This is the roadmap. The design itself is specified in [SPEC.md](SPEC.md), which is normative and
written at the level of detail of OpenAI's Symphony specification, from which it is derived.

Status: proposal, nothing built yet. This repository is public, so it never contains secrets, host
names or tokens.

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

## Phases

Each phase lands as its own pull request and is done when the listed tests pass.

| Phase | Scope (SPEC sections) | Done when |
|---|---|---|
| 0. Verification spikes | Every `To verify` item in SPEC.md | Each is confirmed or the spec is corrected. Results recorded in `docs/phase0.md`. |
| 1. Spec core | 4 to 9, 12.1, 16.1, 16.5, 16.7 with a fake agent, fake tracker, no sandbox backend yet (test-only stub) | Test matrix 20.1, 20.2, 20.4, 20.8 |
| 2. GitHub adapter and board | 12.2 to 12.5, 16.4, Project and label setup | Test matrix 20.3. A ticket approved on the board moves across the columns with the fake agent. A ticket from another account is ignored. |
| 3. Sandbox, Linux | 11, launcher, proxy | Test matrix 20.6 on a RHEL 9 compatible VM |
| 4. Claude runner | 10, 13 | Test matrix 20.5. A real ticket produces commits on a scratch repository. |
| 5. Verify and publish | 14, 15, 17 | Test matrix 20.7 |
| 6. Linux deployment | Appendix A, `doctor`, runbook | A fresh VM reaches a running service from the scripts alone |
| 7. Linux pilot | Appendix C, 20.9 | The adversarial tickets end harmlessly. Two weeks at concurrency 1. |
| 8. Linear adapter | 12.6 | Test matrix 20.3 for Linear |
| 9. Mac worker | Appendix B | Test matrix 20.6 and 20.9 on the Mac with an Xcode and a Unity sample |

## Phase 0 list

Collected from SPEC.md:

- Claude Code: flag names, stream event schema, resume in headless mode, empty MCP configuration,
  repository settings that could re-enable tools, how missing login and exhausted quota are
  reported, which hosts the CLI needs, which files hold the login, whether an interactive login
  persists for headless use on a server, credential format for the secret scan.
- Anthropic terms: current wording on unattended use of a subscription through the official CLI.
- GitHub: fine-grained token access to a user-owned Project, attribution of Project status
  changes, label permissions on public repositories, issue dependency API, push rejection of
  workflow changes without the Workflows permission.
- Linear: issue history through the API, scope of API keys.
- systemd on the chosen guest: every unit property in SPEC 11.3, `IPAddressDeny` enforcement.
- macOS: Xcode and Unity inside a macOS guest (performance, graphics, licence activation), the
  host-only command channel, guest limits.

## Open questions for the owner

- GitHub Project or labels as the source of ticket state, if the token cannot update a Project.
- Which board is used day to day once both adapters exist.
- Whether the macOS guest is acceptable for Unity, or the weaker dedicated-user backend is wanted.
- Whether to rename the repository, since Beads is no longer part of the design.
