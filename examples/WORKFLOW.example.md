---
tracker:
  kind: github
  provider:
    repo: your-account/your-repo
    token: $GITHUB_TOKEN
    state_source: labels
    bot_login: your-app[bot]
  allowed_authors: [your-account]
workspace:
  root: /var/lib/maestro-agent/workspaces
  repo: your-account/your-repo
hooks:
  after_create: |
    npm ci
sandbox:
  backend: systemd
  network:
    proxy: 127.0.0.1:3128
    allow_hooks: [registry.npmjs.org]
verify:
  command: |
    npm run lint
    npm test
budget:
  state_dir: /var/lib/maestro/state
worker:
  name: linux
---

Implement the issue below in this repository.

Follow the conventions in CLAUDE.md. Add or update tests for every behaviour you change, and run
the test suite before you finish.
