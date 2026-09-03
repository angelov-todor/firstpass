# firstpass — automated PR review triggered from Google Chat

**Date:** 2026-09-03
**Status:** approved design, not yet implemented

## Problem

The AstraEx team posts every PR needing review to the `[AstraEx] Team` Google Chat space
(`spaces/EXAMPLE123`) — a standing rule, enforced by the `posting-prs-for-review` skill. Reviewing
those PRs is manual and therefore late and uneven.

`firstpass` is a Go daemon on the user's machine that watches that space and, for each newly posted
PR, runs a Claude Code review and posts the findings as inline GitHub PR comments.

## Scope

In scope:

- Poll the `[AstraEx] Team` space for newly posted GitHub PR links.
- Review each qualifying PR once, in an isolated checkout.
- Post findings as inline comments on the PR, under the user's own GitHub identity.

Out of scope:

- Any chat platform other than Google Chat; any forge other than GitHub.
- Re-reviewing on subsequent pushes. The team re-posts a PR whose approval lapsed, so the re-post
  is the re-trigger.
- Replying to review threads, resolving them, or approving / requesting changes on the PR.
- Running on a server, or for anyone but this user.

## Decisions and their reasons

| Decision | Chosen | Why |
|---|---|---|
| Trigger | Google Chat space, not GitHub polling | Only PRs actually posted to the space should be reviewed. GitHub carries repos and PRs that are not review requests. |
| Chat access | Subprocess to the existing `google-chat` skill's `chat.py` | Already OAuth'd, token in Windows Credential Locker, no Workspace admin needed, no MCP server. A hosted MCP connector could not be used by a standalone binary at all. |
| Review engine | `claude -p "/code-review --comment"` | Reuses the repo's `CLAUDE.md`, the `dotnet-techne-code-review` skill, the sonarqube MCP, and `/code-review`'s existing inline-comment posting. Go builds no prompt and touches no GitHub comment API. |
| Checkout | Bare mirror cache plus a throwaway worktree per PR | Never touches the user's working copies in `example-org/`, so a background review cannot disturb an active branch or uncommitted work. |
| Scope | Every posted PR except ones the user authored | Matches how the space is used. `github_login: angelov-todor`. |
| Dedupe | Once per PR identity | The team re-posts when an approval lapses, so re-posting is the intended re-trigger. |
| Output | Inline GitHub PR comments | The user's explicit choice, made against a recommendation of local-only. Mitigated by a dry-run default, not by narrowing the choice. |
| Topology | One long-running process, serial review worker | The work is genuinely serial, and concurrent `claude` processes on a laptop are undesirable. The `scan` subcommand makes Task Scheduler operation available without further work. |

### Risk accepted deliberately

Posting machine-generated comments on colleagues' PRs is outward-facing, and a false positive costs
the author's time rather than the user's. This was raised, and the user reaffirmed the choice. The
design mitigates it with a dry-run default, a cold-start guard, per-sweep throttles and a kill
switch — not by reducing the feature.

## Architecture

Single Go module, one binary:

- `watch` — the daemon: sweep on a ticker.
- `scan` — one sweep, then exit. This is also the Task Scheduler entry point. Flags
  `--print-only`, `--backfill N`.
- `status` — print the review table.
- `replay <pr-url>` — force one PR, ignoring dedupe. Respects `dry_run` unless `--live` is passed,
  so replaying to inspect output cannot post by accident.
- `doctor` — preflight every external dependency.
- `pause` / `resume` — write and remove the kill-switch file.

### Components

| Package | Responsibility | Interface |
|---|---|---|
| `internal/chat` | Read the space | `Fetch(ctx, Watermark) ([]Message, error)` |
| `internal/prref` | Extract PR refs from message text | `Extract(text string) []PRRef` — pure |
| `internal/ghpr` | Query PR metadata | `Inspect(ctx, PRRef) (PRInfo, error)` |
| `internal/store` | Persist watermark and outcomes | `Reviewed`, `Record`, `Pending`, watermark get / set |
| `internal/worktree` | Provide code on disk | `Prepare(ctx, PRRef, sha) (dir, cleanup, error)` |
| `internal/review` | Run the reviewer | `Run(ctx, dir, PRRef) (Result, error)` |
| `internal/pipeline` | Orchestrate one sweep | `Sweep(ctx) (SweepReport, error)` |

`chat`, `ghpr` and `review` are the only packages that touch the outside world. Each is one
interface with a fake, so `pipeline` tests run without subprocesses.

### Data flow, one sweep

1. Ticker fires (default 5m).
2. `chat.Fetch` runs `python chat.py get-messages <space> --limit 50`. Output is the raw Chat API
   `spaces.messages.list` response, ordered `createTime desc`. Leading non-JSON lines
   (`Access token expired, refreshing...`) are stripped before unmarshalling. The walk goes newest
   to oldest and stops at the stored watermark.
3. `prref.Extract` runs per message; refs are de-duplicated within the batch.
4. Refs are filtered against `allow_owners` and `deny_repos`, then against `store` for an existing
   terminal record. The owner check comes first, so a disallowed owner is never even queried.
5. `ghpr.Inspect` runs `gh pr view --json state,isDraft,author,headRefOid`. A PR qualifies only if
   it is `OPEN`, not a draft, and not authored by `github_login`.
6. `worktree.Prepare` maintains a bare mirror at
   `%LOCALAPPDATA%\firstpass\repos\<owner>_<repo>.git`, fetches `refs/pull/<n>/head`, and adds a
   worktree at `%LOCALAPPDATA%\firstpass\work\<owner>_<repo>_<n>`.
7. `review.Run` executes `claude -p "/code-review --comment"` with cwd set to the worktree.
8. `store.Record` writes the outcome, then the worktree is removed.
9. The watermark advances only after the entire batch is recorded.

## State

`bbolt` at `%LOCALAPPDATA%\firstpass\state.db`, three buckets:

- `meta` — `watermark` -> `{messageName, createTime}`. The key is the message **name**
  (`spaces/.../messages/...`), which is stable and unique. `createTime` has ties and clock skew, so
  it serves only as a coarse pagination bound.
- `reviews` — `owner/repo#n` -> outcome, head SHA, triggering message, duration, exit code, report
  path.
- `pending` — `owner/repo#n` -> `{firstSeen, attempts, lastAttempt, lastReason}`.

### Terminal versus deferred outcomes

Dedupe on PR identity alone would mishandle a draft PR: recorded as skipped, marked ready an hour
later, and then never reviewed, because the watermark has passed the message. The same applies to a
single transient `gh` failure.

The dividing line is whether `claude` has started. Anything that fails **before** it starts has
posted nothing and is safe to retry; anything that fails **after** may have posted part of a comment
set.

| Class | Outcomes | Behaviour |
|---|---|---|
| Terminal | reviewed, author-is-user, merged, closed, owner not allowed, repo denylisted | Stored in `reviews`. Never revisited. |
| Deferred | draft, `gh` / network failure, `worktree` failure — all of which occur before `claude` starts | Stored in `pending`. Re-inspected every sweep, independent of the watermark. Retried with backoff. |
| Needs attention | review timeout, `claude` crash, any failure after `claude` started | Stored in `reviews` as `needs_attention`. No automatic retry, ever. |

`pending` entries expire after 7 days or 20 attempts, with a log line, so a PR abandoned in draft is
not re-inspected forever.

### Crash or timeout mid-review

`/code-review --comment` posts comments as it proceeds. A crash partway through leaves some comments
already on the PR, and a blind retry would duplicate them on a colleague's work.

The record is therefore written as `in_flight` **before** `claude` starts. A sweep that finds an
`in_flight` record from a previous run does not retry: it marks the PR `needs_attention`, reports
it, and leaves it alone. The user runs `replay` if another pass is wanted.

A review timeout is the same hazard, not a transient error: the deadline can fire while `claude` is
midway through posting. So a timeout is `needs_attention` too, never a retry. This is the reason
`review_timeout` is generous (20m) — a timeout is a manual-intervention event, so it should only
fire when something is genuinely stuck.

## Safety controls

1. **`dry_run: true` in the shipped config.** Runs `/code-review` without `--comment` and writes
   findings to `%LOCALAPPDATA%\firstpass\reports\<owner>_<repo>_<n>.md`. Same pipeline, same worktree,
   same reviewer, no GitHub write.
2. **Cold-start guard.** A first run with an empty watermark reviews nothing: it sets the watermark
   to the newest message and stops. `scan --backfill N` deliberately takes the last N messages.
3. **Kill switch.** A `PAUSE` file in the state dir, written and removed by `pause` / `resume`.
   While it is present, sweeps poll and queue into `pending` but run no reviews and post nothing.
   Refs queued while paused do **not** increment `attempts`, so a week-long pause cannot silently
   expire a backlog. A file rather than a signal, because it works when the daemon is wedged and it
   survives a restart.
4. **Throttles.** `max_reviews_per_sweep: 3`, `review_timeout: 20m`, strictly one review at a time.
5. **`allow_owners`, defaulting to `[Example-Org]`.** A PR whose owner is not on the list is
   recorded terminal and never touched. This is not the same control as `deny_repos`, and it matters
   more: the space is a chat room, so someone will eventually paste a link to an unrelated
   open-source PR. Without an owner allowlist, `firstpass` would clone a stranger's repository and
   post review comments on it. The allowlist makes the default blast radius the team's own org.
6. **`deny_repos`.** Never acted on; recorded terminal. For carving specific repos out of an
   otherwise allowed owner.

## Configuration

`%APPDATA%\firstpass\config.yaml`, every field overridable by flag:

```yaml
space: spaces/EXAMPLE123
github_login: angelov-todor
interval: 5m
dry_run: true
max_reviews_per_sweep: 3
review_timeout: 20m
pending_max_attempts: 20
pending_max_age: 168h
allow_owners:
  - Example-Org
deny_repos: []
paths:
  python: python
  chat_script: C:\Users\you\projects\github.com\example-org\.claude\skills\google-chat\scripts\chat.py
  claude: claude
```

`paths.python` must be `python`, not `python3`: the git-bash `python3` on this machine cannot import
`keyring`, so `chat.py` fails there.

## Failure modes

| Failure | Detection | Response |
|---|---|---|
| `chat.py` non-zero exit | exit code, stderr | Transient. Log, do not advance the watermark, retry next sweep. |
| Wrong Google account | `list-spaces` returns zero named rooms | Refuse to sweep, warn loudly. Two Google accounts exist on this machine and the personal one lists no named rooms; sweeping it silently looks like "no PRs posted". |
| `ACCESS_TOKEN_SCOPE_INSUFFICIENT` | stderr match | Configuration error. Fail loudly; retrying cannot fix a missing scope. |
| `gh` unauthenticated | `doctor` preflight | Fatal at startup. |
| `claude` absent from PATH | `doctor` preflight | Fatal at startup. |
| Review timeout | context deadline | `needs_attention`. Not retried — the deadline may have fired mid-post. |
| Crash mid-review | `in_flight` record found | `needs_attention`, no automatic retry. |
| PR link to an outside org | owner not in `allow_owners` | Recorded terminal, never cloned or queried. |

Logging goes to stderr and a rotating file via `log/slog`. `status` prints the review table.

## Testing

Test-driven throughout. The decision logic is I/O-free, so most tests need no subprocesses.

- **`prref.Extract`** — the largest test set, because a missed URL means a PR is never reviewed and
  nothing reports it. Cases: bare URLs, trailing punctuation (`.../pull/91.`), `/files` and
  `?diff=split` suffixes, several PRs in one bulleted message (the team's batch format), the same PR
  posted twice, and the `Example-Org/repo#91` shorthand that `post_prs.py` also accepts. Bare `#91`
  is deliberately **not** parsed: `team-chat-messages.md` records it as ambiguous, and parsing it
  would require guessing a repo.
- **`chat`** — golden files of real `get-messages` output, including one fixture carrying the
  `Access token expired, refreshing...` prefix ahead of the JSON.
- **`pipeline`** — fakes for `chat`, `ghpr` and `review`. Covers the full decision table: skip own,
  draft to `pending`, merged terminal, owner not in `allow_owners`, in-batch dedupe, per-sweep cap,
  `PAUSE` file (including that it does not increment `attempts`), cold start reviewing nothing,
  timeout landing on `needs_attention` rather than retrying, and the watermark advancing only after
  a fully recorded batch.
- **`store`** — real bbolt in a temp dir, reopened mid-test to prove `in_flight` and the watermark
  survive a restart.
- **`worktree`** — real git against a fixture repo built in a temp dir; behind `-short`.
- **Manual smoke** — `scan --print-only --backfill 200` against the real space.

## Build order

Slices 1 to 3 hold most of the logic and cannot write anywhere.

1. `prref` + `store` — pure core, fully tested.
2. `chat` + `scan --print-only` — prints the PRs it would review. **Checkpoint:** run with
   `--backfill 200` and check extraction against months of real messages. Parser correctness is the
   thing most likely to be quietly wrong, and this is the cheapest way to prove it against real
   data.
3. `ghpr` filter — `--print-only` shows a decision and a reason per PR. Still no writes.
4. `worktree` + `review`, dry-run — real reviews, reports written to disk.
5. `watch` loop — ticker, pause file, throttles, `status`, `replay`, `doctor`.
6. Flip `dry_run: false` — comments begin landing on PRs.

## Success criteria

- A PR posted to `[AstraEx] Team` by another team member receives inline review comments within one
  poll interval, with no manual step.
- The user's own PRs, drafts, and merged or closed PRs receive no comments.
- No PR receives duplicate comment sets, including across daemon restarts and crashes.
- A first run against a populated space reviews nothing.
- `pause` stops all posting within one sweep.
- The user's working copies in `example-org/` are never modified.
