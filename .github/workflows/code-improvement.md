---
name: code-improvement
description: Diagnose and fix code or CI issues
intent: >-
  Keep vtessera's hermetic build and test suite green on main, so that the
  agent-to-agent marketplace can be built, tested, and released with
  confidence.
on:
  schedule: daily on weekdays
  push:
    branches: [main]
  skip-if-match: 'is:issue is:open "gh-aw-workflow-id: code-improvement" in:body'
  steps:
    - name: Skip scheduled runs when too many workflow PRs are open
      id: pr_pressure
      env:
        GH_TOKEN: ${{ github.token }}
        EVENT_NAME: ${{ github.event_name }}
      run: |
        set -uo pipefail
        if [ "$EVENT_NAME" != "schedule" ]; then
          echo "not a scheduled run; PR-pressure gate does not apply"
          exit 0
        fi
        n=$(gh pr list --repo "$GITHUB_REPOSITORY" --state open --limit 100 \
              --json title --jq '[.[] | select(.title | startswith("code-improvement:"))] | length')
        echo "open code-improvement PRs: $n (ceiling 2)"
        [ "$n" -lt 2 ]
if: needs.pre_activation.outputs.pr_pressure_result == 'success'
permissions:
  contents: read
engine: copilot
tools:
  bash: true
  github:
    mode: gh-proxy
    toolsets: [default]
  cache-memory: true
network:
  allowed:
    - defaults
    - go
steps:
  - name: Set up Go toolchain
    run: |
      set -euo pipefail
      GO_VERSION=1.27.1
      curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
      rm -rf /usr/local/go
      tar -C /usr/local -xzf /tmp/go.tgz
      echo "/usr/local/go/bin" >> "$GITHUB_PATH"
  - name: Record hermetic baseline
    run: |
      set -uo pipefail
      export PATH="/usr/local/go/bin:$PATH"
      export TMPDIR="${TMPDIR:-/tmp}"
      mkdir -p /tmp/gh-aw/agent
      {
        echo "## gofmt -l . (empty is good)"
        gofmt -l . ; echo "gofmt_exit=$?"
        echo "## go vet ./..."
        go vet ./... 2>&1 ; echo "vet_exit=$?"
        echo "## go build ./..."
        go build ./... 2>&1 ; echo "build_exit=$?"
        echo "## go test ./..."
        go test ./... 2>&1 | tail -60 ; echo "test_exit=${PIPESTATUS[0]}"
      } | tee /tmp/gh-aw/agent/baseline.txt
safe-outputs:
  create-pull-request:
    max: 1
    expires: 7
    protected-files: fallback-to-issue
  push-to-pull-request-branch:
  merge-pull-request:
concurrency:
  group: code-improvement-${{ github.ref }}
  cancel-in-progress: false
timeout-minutes: 30
evals:
  - id: operational_value
    question: >-
      Does the agent output include real command output demonstrating that
      gofmt, go vet, go build, and go test all pass on the branch the agent
      produced?
  - id: single_failing_check_fixed
    question: >-
      When the recorded baseline contained a failing check, does the agent
      output name that specific check and show it passing afterwards?
  - id: scope_is_one_concern
    question: >-
      Does the agent output show that the change touched only files belonging
      to the single selected concern, with no unrelated modifications?
  - id: no_repeat_of_declined_work
    question: >-
      Does the agent output show that recently closed pull requests from this
      workflow were inspected, and that the proposed change was not one of
      them?
---

# Code Improvement

## Intent

vtessera is an agent-to-agent marketplace where AI agents connect, negotiate, and
exchange value and information through trades, established processes, and open
protocols. It is non-custodial: the service never holds funds, and buyers sign
their own on-chain settlements.

This workflow exists for one guarantee: `main` builds and its tests pass. Every
run works on that single outcome.

## Baseline — read this first

`/tmp/gh-aw/agent/baseline.txt` holds the recorded output of `gofmt -l .`,
`go vet ./...`, `go build ./...`, and `go test ./...`, captured before you
started. Read it before choosing work.

- If it shows a **failure** in any of those four, this run's scope is fixed:
  fix that one failure and prove the same command now passes. Choose nothing else.
- If all four **pass**, take this run's scope from the rotation below.

## Rotation — avoid re-picking the same category

Read `/tmp/gh-aw/cache-memory/rotation.json`. It records categories chosen in
recent runs. Pick exactly one category that is not in `last_selected`,
preferring the least-recently-used of: `tests`, `docs`, `code-quality`,
`performance`. Append your choice and the run id to that file before you finish,
so the next scheduled run rotates away from it. If the file is absent, treat
every category as unused and create it.

## Choosing one narrow change

- Scope each run to **one** narrow, well-defined improvement: a single failing
  check, or one named category of fix. Never an open-ended "fix anything broken"
  pass — broad, unscoped improvement runs have the lowest success rates.
- Read `AGENTS.md` first. It is binding: it defines this repository's commands
  and its hard constraints. Do not contradict it.
- Before proposing anything, inspect recently closed pull requests from this
  workflow: `gh pr list --repo "$GITHUB_REPOSITORY" --state closed --limit 20
  --json title,body,url`. A close marked *not planned*, or a rejecting review
  comment, is a strong signal that change was unwanted. Do not re-propose it.
  Choose something else, or `noop`.

## Hard constraints — DO NOT

- **DO NOT** modify `go.mod`, `go.sum`, `AGENTS.md`, or anything under
  `.github/`. These are protected; a change to them is routed to a maintainer
  issue, never a pull request.
- **DO NOT** add a dependency, a toolchain, or a second Go module. This is a
  single Go module on Go 1.27 using its existing dependency set.
- **DO NOT** start `solana-test-validator` or run `make test-solana`. The
  validator-backed suite is build-tagged behind `-tags solana` and needs a
  transient local validator; it is out of scope here. `make smoke` likewise.
- **DO NOT** change on-chain settlement behaviour, fee policy, or token registry
  constants. Settlement is under active Phase 3 design and those values are
  pinned deliberately — including a known-bad EURC mint that is a tracked,
  separately-fixed defect. Do not "fix" it here.
- **DO NOT** substitute `make race` or the E2E suite for the hermetic suite; use
  `go test ./...`.
- **DO NOT** modify files outside the one concern you selected.
- **DO NOT** add comments to code that does not need them. This repository is
  already densely commented; match the surrounding density.
- **DO NOT** touch `bin/`, `data/`, or any `*.db` file.

## Validate before proposing

Run these in order and paste the real output. Never claim a result you did not
observe.

```sh
gofmt -l .        # must print nothing
go vet ./...
go build ./...
go test ./...
```

If anything still fails, do not open a pull request. Fix it, or `noop`.

## Safe outputs

- `create-pull-request` — open as a **draft**. The title MUST begin with
  `code-improvement: `. The body must state the problem, the rationale, the exact
  commands run with their output, and what you deliberately did not do.
- `push-to-pull-request-branch` — only to a branch this workflow created.
- `merge-pull-request` — only for a pull request this workflow created, only
  once every required check has passed, and only after independent validation.
  **NEVER** merge a human-authored pull request. **NEVER** force-push. If you
  are unsure, leave it for a maintainer: merging is the one irreversible action
  available here.
- `noop` — call it with a short reason when no single narrow improvement is
  available, or when the only candidates were previously declined. A noop is a
  legitimate outcome, not a failure.
