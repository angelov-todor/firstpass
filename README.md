# firstpass

A machine first pass over a PR before a human looks.

`firstpass` is a Go daemon for Windows that watches a Google Chat space for
posted GitHub pull request links and, for each newly posted PR, runs a
[Claude Code](https://claude.com/product/claude-code) review in an isolated
git worktree and posts the findings as inline comments on the PR.

## How it works

One sweep, on a ticker or on demand:

1. Read new messages from the configured Google Chat space.
2. Extract GitHub pull request links from the message text (full URLs and
   `owner/repo#n` shorthand).
3. Filter: skip PRs you authored, PRs whose owner isn't on `allow_owners`,
   drafts, and anything already reviewed.
4. For each survivor, prepare a throwaway git worktree against a local bare
   mirror of the repository.
5. Run `claude -p "/code-review <pr-url>"` in that worktree.
6. In dry run (the default), the findings are written to a report file. Live,
   `/code-review` posts them as inline comments on the PR.

Decisions and outcomes are recorded in a local `bbolt` database, so restarts
and crashes never cause a PR to be reviewed twice.

## Prerequisites

- Go 1.24 to build.
- [`gh`](https://cli.github.com/), authenticated (`gh auth login`).
- [`claude`](https://claude.com/product/claude-code) on `PATH`.
- `git` on `PATH`.
- A script that can read your Google Chat space and print its messages as
  JSON on stdout. **firstpass does not talk to the Google Chat API
  directly** — it drives this script as a subprocess, the same way it drives
  `git` and `gh`. No such script is included in this repository; you need to
  supply or write one yourself (see `internal/chat` for the expected output
  shape).

## Install and configure

```
go build ./cmd/firstpass
```

Copy `config.yaml.example` to `%APPDATA%\firstpass\config.yaml` and fill in
the fields it marks as required: `space`, `github_login`, `allow_owners`, and
`paths.chat_script`. `firstpass doctor` refuses to say a fresh, unconfigured
install is healthy — it will tell you which of these is still missing.

Run `firstpass doctor` to check every external dependency (`git`, `claude`,
`gh` auth, the chat script, and that the configured Google Chat account can
actually see named spaces).

## Commands

- `scan` — one sweep, then exit. Flags: `-print-only`, `-backfill N`,
  `-live`, `-quiet`. Also the intended Task Scheduler entry point.
- `watch` — sweep on a ticker until interrupted. Flag: `-live`.
- `status` — print the review table: what's been reviewed, skipped, or
  deferred.
- `replay <pr-url | owner/repo#n>` — force one PR through review again,
  ignoring the dedupe record. Flags: `-live`, `-quiet`.
- `doctor` — preflight every external dependency.
- `pause` / `resume` — write / remove a kill-switch file. While paused,
  sweeps still queue new PRs but run no reviews and post nothing.

Every command accepts `-config <path>` to point at a config file other than
the default.

## Safety

Read this before running anything other than `doctor` or `scan -print-only`.

- **`dry_run: true` is the default.** Passing `-live`, or setting
  `dry_run: false` in the config, is what makes firstpass post real comments
  to real pull requests, under your GitHub identity.
- **`allow_owners` is the blast-radius control, and it has no default.** A
  Google Chat space is a chat room, not an access boundary — someone will
  eventually paste a link to a repository you don't intend firstpass to
  touch. Set `allow_owners` before running anything live.
- **`--permission-mode bypassPermissions` in `claude_args` is not
  sandboxed by the worktree.** The review subprocess inherits your full
  environment and can reach anything you can reach — including
  repositories outside `allow_owners` — because that allowlist decides
  which PRs firstpass chooses to review, not what the subprocess is capable
  of once it's running. The checkout it reviews is also attacker-influenced:
  it's the pull request author's code, and it may carry its own `CLAUDE.md`
  or `.claude/` configuration that a headless agent with no human at the
  prompt will read as instructions. See
  `docs/superpowers/reviews/2026-09-03-final-review-outcomes.md` for the
  full analysis and the mitigations that were considered but not applied.
- **The review stage has not been verified end to end by the authors.**
  Parsing and filtering have been checked against real chat history; running
  `claude` against a real checkout and posting real comments has not. Run
  one dry-run review (`scan -backfill N`, or `replay <url>`, without
  `-live`) and read the resulting report before ever setting `dry_run:
  false`.
