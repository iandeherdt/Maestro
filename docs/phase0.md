# Phase 0: verification results

Each `To verify` item in [SPEC.md](SPEC.md) is listed here with its status.

- **Confirmed (docs)**: stated in the vendor's documentation or the CLI's own help.
- **Confirmed (run)**: observed on a real system. None yet.
- **Changed**: the assumption was wrong, and SPEC.md has been corrected.
- **Open**: needs a real run or a real machine.

Sources: `claude --help` for Claude Code 2.1.330; the Claude Code documentation at
`code.claude.com/docs` (headless, cli-reference, errors, sessions, authentication, network-config,
permission-modes, sandboxing, env-vars); the GitHub documentation (Projects API, personal access
tokens, issue dependencies, labels, repository roles); the Linear documentation (API and webhooks,
security and access). Checked on 2026-11-21.

## 1. Claude Code

| Item | Status | Finding |
|---|---|---|
| Headless flags | Confirmed (docs) | `-p`, `--output-format stream-json`, `--verbose`, `--permission-mode`, `--disallowedTools`, `--strict-mcp-config`, `--mcp-config`, `--model`, `--resume`, `--session-id <uuid>`, `--setting-sources`, `--permission-prompts none` all exist. |
| Step limit | Open | `--max-turns` is documented ("print mode only", env fallback `CLAUDE_CODE_MAX_TURNS`) but is not listed in `--help` of 2.1.330. Needs one real run. `--max-budget-usd` also exists and is a client-side estimate. |
| Prompt on standard input | Confirmed (docs) | "Non-interactive mode reads stdin". Capped at 10 MB. |
| Stream schema | Confirmed (docs) | First event `system`/`init`. Then `assistant`, `user`, `system`/`api_retry`, `permission_denied`, hook events. Last line is `result` with subtype `success`, `error_max_turns`, `error_max_budget_usd`, `error_during_execution` or `error_max_structured_output_retries`, and fields `result`, `session_id`, `total_cost_usd`, `usage`, `num_turns`, `stop_reason`, `permission_denials`. |
| Resume in headless mode | Confirmed (docs) | `claude -p --resume <session_id>`. Transcripts are stored per working directory, so the workspace path must be stable. `--mcp-config` and `--settings` are not restored and must be passed again. |
| Approval requests | Changed | With `--permission-prompts none`, anything that would prompt is denied automatically and reported as `permission_denied`. The run continues. A run can therefore not stall on a prompt, and `turn_input_required` is no longer a failure category. |
| Repository settings re-enabling tools | Changed | Without extra flags, `-p` runs project hooks and `.mcp.json` servers with no trust dialog. `--setting-sources user` drops project and local settings, and `--strict-mcp-config` drops project MCP servers. Whether project skills and commands still load is not documented. They run inside the sandbox either way. |
| Not logged in | Confirmed (docs), partly open | `claude auth status` exits `1` when logged out, which gives a clean preflight check. In a run the result text is "Not logged in · Please run /login" or "Login expired · Please run /login", and retries appear as `api_retry` with `error: authentication_failed`. Exit status and result subtype are not documented. |
| Quota exhausted | Confirmed (docs), partly open | Text only: "You've hit your session limit · resets 3:45pm" (with weekly variants), and `api_retry` with `error: rate_limit` or `billing_error`. No machine-readable reset time. |
| Login storage | Confirmed (docs) | Linux: `~/.claude/.credentials.json`, mode 0600. macOS: Keychain, with the same file as a fallback. `CLAUDE_CONFIG_DIR` relocates both config and credentials. |
| Interactive login persists for `-p` | Confirmed (docs) | A prior `claude auth login` is used by later sessions. If `ANTHROPIC_API_KEY` is set, `-p` always uses the key instead, so the sandbox environment must never contain it. |
| `--bare` | Changed (new risk) | `--bare` never reads the subscription login. The docs say it "will become the default for `-p` in a future release". The CLI version is therefore pinned, and `doctor` checks that the pinned version still honours the login in print mode. |
| Hosts needed | Confirmed (docs), partly open | Documented list includes `api.anthropic.com`, `claude.ai`, `platform.claude.com` and several optional ones (updates, telemetry, marketplace). `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` turns off updates, telemetry, error reporting and feature-flag fetches. The minimal set that keeps login refresh working needs observing in the proxy log. |
| Proxy support | Confirmed (docs) | `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`. No SOCKS. Read at startup. |
| Permissive mode protections | Confirmed (docs) | `bypassPermissions` refuses to run as root, and deny rules still apply. The docs state it "offers no protection against prompt injection", which is the reason the OS sandbox is mandatory here. |
| Built-in command sandbox | Confirmed (docs) | Needs `bubblewrap` and `socat` and user namespaces on Linux. Covers Bash only, not file tools or hooks. It conflicts with `RestrictNamespaces`, so it stays off by default and the OS sandbox is the control. |
| Credential format for the secret scan | Open | Not documented. |
| Billing of `-p` on a subscription | Open | Not stated in the CLI documentation. Press coverage from May 2026 describes a separate monthly credit for programmatic use. To be confirmed on the account's usage page and in the current terms. |

## 2. GitHub

| Item | Status | Finding |
|---|---|---|
| Fine-grained token and a user-owned Project | Changed | Not supported. The documentation lists it as a known gap. User-owned Projects need a classic token with the `project` scope. Organization-owned Projects have a proper "Projects" permission. |
| Checks API with a fine-grained token | Changed | Not supported either. Required checks from Actions are check runs, so a fine-grained personal token cannot implement the checks gate. |
| Who opens the pull request | Changed (found during review) | A pull request cannot be approved by its own author. If the service used the owner's personal token, the owner could not review the service's pull requests, and the token would have the owner's admin rights. The service therefore needs its own identity. |
| Resulting credential design | Changed | A GitHub App installed on the target repositories replaces the personal token: it has its own identity, is not an administrator, can read checks, and its tokens expire after an hour. See SPEC 15.5. |
| Label permissions | Confirmed (docs) | Applying labels needs the Triage role or higher. Read access is not enough. The table is for organization repositories. On a personal repository only the owner and collaborators can label. |
| `labeled` event actor | Confirmed (live test) | The issue timeline carries a `labeled` event with the actor for every label, including labels given when the issue is created. |
| Label mode end to end | Confirmed (live test) | Listing, approval, state change, status comment and feedback work against a scratch repository on github.com. The test used one account as both owner and service, so the rule that separates the two is covered only by the unit tests. |
| Freshness of the issue list | Changed (live test) | `GET /issues` filtered by label trailed writes by about seven seconds: new issues were missing and closed ones were still listed. A read of a single issue was current. The adapter no longer trusts the list for an issue it has just written to (SPEC 12.5). |
| Actor of a Project status change | Confirmed unavailable (live test) | The status field value has `creator` and `updatedAt`, but `creator` stayed `github-project-automation` after the owner moved the card by hand. It does not identify who moved a card (residual risk R6). |
| `project`-scope token and private repositories | Changed (live test) | The token reads the Project and its cards. For an issue in a private repository the card's content is null, so the service cannot tell which issue a card is. A user-owned Project therefore only works with public repositories. |
| Issue dependencies | Confirmed (live test) | `GET /repos/{owner}/{repo}/issues/{n}/dependencies/blocked_by`, Issues read. Answers on a personal repository. |
| Push of workflow files without the Workflows permission | Open | The permission exists and is described as the right to edit workflow files. The rejection itself needs one real push. |
| Reading branch protection | Open | The permission needed was not in the pages read. |

Consequence for the board: a kanban that the owner drags cards on means a GitHub Project. Two ways
to make that work with least privilege are in SPEC 12.5. The cleaner one is a free GitHub
organization that owns the repositories and the Project.

## 3. Linear

| Item | Status | Finding |
|---|---|---|
| API key scope | Changed | Keys can be restricted to permissions (Read, Write, Admin, Create issues, Create comments) and to specific teams. The earlier assumption that keys are workspace-wide was wrong. |
| Issue history | Open | An `IssueHistory` type exists in the public schema. Its fields were not read. |
| Rate limits and paging | Open | Documented on pages not yet read. |
| Workspace hardening | Confirmed (docs) | Admins can stop members creating API keys. Passkeys are available. Enforcement options were not found. |

## 4. Still needing a machine

- systemd unit properties and `IPAddressDeny` on the RHEL 9 compatible guest (SPEC 11.3).
- macOS guest: Xcode, Unity (performance, graphics, licence activation), command channel (Appendix B).
- One real `claude -p` run in the sandbox to capture a sample stream, confirm `--max-turns`, and
  record the hosts requested through the proxy. The same run should show: whether a resumed
  session keeps its ID, what the CLI prints for an unknown session, whether the result's `usage`
  covers one invocation or the whole session, and the exit status and result subtype for a
  missing login and for a usage limit. The runner's handling of each is in SPEC 10.2 and 10.7.
