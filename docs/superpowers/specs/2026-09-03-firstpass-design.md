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
- Review each qualifying PR once per commit, in an isolated checkout: a re-posted PR is reviewed
  again if, and only if, it has new commits since the pass that already reviewed it.
- Post findings as inline comments on the PR, under the user's own GitHub identity.
- Submit an explicit review verdict on every successfully reviewed PR, so a clean one is never
  silent: an approval when nothing needs changing, a comment review when something does.
- React to the chat message that carried the link, so the team can see the PR has been picked up
  and, later, that it is done with.

Out of scope:

- Any chat platform other than Google Chat; any forge other than GitHub.
- Re-reviewing on a push alone. The re-post is the re-trigger: firstpass watches the space, not
  the repository, so new commits on their own are invisible to it and always will be. A re-post
  with new commits does earn a second pass (see the dedupe row below); a push with no re-post
  earns nothing.
- Incremental review. A second pass reviews the whole pull request again and posts a fresh set of
  inline comments. Diffing against the previously-reviewed commit is not possible: the mirror
  force-updates `refs/firstpass/N`, so that commit can be unreachable by the time the next pass
  runs. The reviewer is told a pass already happened and asked to concentrate on what has changed
  and not to restate that pass's findings, which is a request in a system prompt rather than a
  guarantee.
- Replying to review threads or resolving them. Requesting changes is out of scope too, and
  deliberately so: see the verdict asymmetry above. Approving is in scope, but only for a PR the
  reviewer raised nothing about.
- Running on a server, or for anyone but this user.

## Decisions and their reasons

| Decision | Chosen | Why |
|---|---|---|
| Trigger | Google Chat space, not GitHub polling | Only PRs actually posted to the space should be reviewed. GitHub carries repos and PRs that are not review requests. |
| Chat access | Subprocess to the existing `google-chat` skill's `chat.py` | Already OAuth'd, token in Windows Credential Locker, no Workspace admin needed, no MCP server. A hosted MCP connector could not be used by a standalone binary at all. |
| Review engine | `claude -p "/code-review --comment"` | Reuses the repo's `CLAUDE.md`, the `dotnet-techne-code-review` skill, the sonarqube MCP, and `/code-review`'s existing inline-comment posting. Go builds no prompt and touches no GitHub comment API. |
| Checkout | Bare mirror cache plus a throwaway worktree per PR | Never touches the user's working copies in `example-org/`, so a background review cannot disturb an active branch or uncommitted work. |
| Scope | Every posted PR except ones the user authored | Matches how the space is used. `github_login: angelov-todor`. |
| Dedupe | Once per commit, on a re-post | The team re-posts when an approval lapses or when they have pushed fixes, so re-posting is the intended re-trigger — but a re-post of the same commit must not earn a second comment set on lines that already carry one. Three conditions, all required: the record's outcome is `reviewed` **and** names the commit it reviewed and when it was decided; the re-post is a different, non-empty chat message **posted after that review finished** (a name comparison alone is not enough — a watermark gap and a `-backfill` both re-offer *older* posts, and the sweep takes the oldest message carrying a link while the record holds the newest, so an ordinary push nobody re-posted would manufacture a review); and the live head SHA is not any commit a pass has already reviewed, tracked as a set in `ReviewedSHAs` because a head force-pushed back to an earlier reviewed commit differs from the last one and must still be refused. `needs_attention` is deliberately excluded — it means a review died mid-post, so comments may be half posted and an automatic retry risks duplicating them; a re-post is not consent to that, `firstpass replay` is. Every skipped outcome is excluded because nothing was ever reviewed, and a reviewed row with no head SHA is excluded because condition 3 cannot be established from a missing field. |
| Output | Inline GitHub PR comments | The user's explicit choice, made against a recommendation of local-only. Mitigated by a dry-run default, not by narrowing the choice. |
| Verdict | An explicit GitHub review on every successfully reviewed PR, decided by the reviewer in one `FIRSTPASS-VERDICT:` line and submitted by firstpass | `/code-review --comment` posts findings and nothing else, so a clean PR produced complete silence — indistinguishable from the tool never having run. It happened for real on a +9/−0 one-file PR. The reviewer decides because firstpass never sees a finding; firstpass submits because that makes the action recorded in the store, visible in `status`, and testable against `runner.Fake` rather than buried in a prompt. |
| Verdict asymmetry | `approve` for nothing-needing-change; a **COMMENT** review, never request-changes, for anything Critical or Important | An approval clears `reviewDecision`, which takes the PR out of the team's human review queue — acceptable only when there is nothing to change. A comment leaves `reviewDecision` at `REVIEW_REQUIRED`, so a PR with findings stays in the queue and a human still looks. request-changes was rejected outright: it would block a merge and speak for a human who has not read the code yet. |
| Verdict on a failure | Nothing submitted | A failed, killed or timed-out review may have posted a partial comment set and reached no conclusion; a verdict on top of that would state one it never reached. Those stay `needs_attention`. A verdict submission that *itself* fails is the opposite case — the review completed and its comments are all posted — so it stays `reviewed`, with the error in `Detail` and no automatic retry. |
| Topology | One long-running process, serial review worker | The work is genuinely serial, and concurrent `claude` processes on a laptop are undesirable. The `scan` subcommand makes Task Scheduler operation available without further work. |
| Progress signal | A reaction on the chat message | The team posts a link and otherwise sees nothing until comments appear on the PR, which can be half an hour later. A reaction needs no new message in the space and no new surface to read. |
| Reaction granularity | Per chat **message**, not per pull request | One post routinely carries several links and reviews run serially, so per-PR reactions would accumulate on one message in an order nobody can read. The useful statements are "this post has been picked up" and "this post is done with". |

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
- `replay <pr-url>` — force one PR, ignoring dedupe but still telling the reviewer that an
  earlier pass has been here. Respects `dry_run` unless `--live` is passed, so replaying to
  inspect output cannot post by accident.
- `doctor` — preflight every external dependency.
- `pause` / `resume` — write and remove the kill-switch file.

### Components

| Package | Responsibility | Interface |
|---|---|---|
| `internal/chat` | Read the space, react to a message | `Fetch(ctx, Watermark) ([]Message, error)`, `AddReaction`, `RemoveReaction` |
| `internal/prref` | Extract PR refs from message text | `Extract(text string) []PRRef` — pure |
| `internal/ghpr` | Query PR metadata; submit the verdict | `Inspect(ctx, PRRef) (PRInfo, error)`, `SubmitReview(ctx, PRRef, verdict, body) error` |
| `internal/store` | Persist watermark, outcomes and message reaction state | `Reviewed`, `Record`, `Pending`, `Message`, watermark get / set |
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
   terminal record. The owner check comes first, so a disallowed owner is never even queried. A
   `reviewed` record re-posted by a different chat message is the one case that falls through this
   gate rather than skipping at it, carrying the previous record with it: the third dedupe
   condition needs the live head SHA, which only step 5 can supply. Every other outcome still skips
   here, above every GitHub call, and no gate moved to make room for it.
5. `ghpr.Inspect` runs `gh pr view --json state,isDraft,author,headRefOid`. A PR qualifies only if
   it is `OPEN`, not a draft, and not authored by `github_login`. A re-posted PR whose head SHA
   matches the recorded one is skipped immediately below this call, above the state gate: its
   record's trigger message is moved to the new post, so the same re-post is not re-inspected on
   every sweep while it sits in the fetch window, and nothing else about the record changes. Being
   above the state gate is deliberate — a PR re-posted after it was merged then keeps its
   `reviewed` record and the verdict on it, rather than having them overwritten with
   `skipped_state`. The cost is one `gh pr view` per *qualifying* re-post — every re-post past
   conditions 1 and 2, not only the ones that turn out to have nothing new: a re-post of a PR that
   has since been merged, drafted or reassigned pays it as well, and those skip without advancing
   the recorded trigger, so each later re-post or re-scan of the same post pays it again. All reads;
   no new write.

   The gates *below* the fall-through were written without an existing review record in mind, and
   three of them record a terminal outcome: the state gate, the author gate, and pending expiry.
   For a candidate that fell through as a re-post, all three skip without rewriting the record.
   Skipping is right; overwriting is not — the reviewed commit, the submitted verdict and the pass
   count are the only evidence of what firstpass did under the operator's own identity, and a merge
   or a `github_login` change is not a firstpass decision at all. The pending entry is still
   cleared: housekeeping, not history. The owner allowlist and the deny list sit *above* the record
   gate and still record their refusal unconditionally, because that refusal *is* firstpass's own
   decision. A replay is unconditional too: it is an explicit question, so its fresh decision is
   the answer the operator asked for.
6. `worktree.Prepare` maintains a bare mirror at
   `%LOCALAPPDATA%\firstpass\repos\<owner>_<repo>.git`, fetches `refs/pull/<n>/head`, and adds a
   worktree at `%LOCALAPPDATA%\firstpass\work\<owner>_<repo>_<n>`.
7. `review.Run` executes `claude -p "/code-review --comment"` with cwd set to the worktree, plus
   `--append-system-prompt` carrying an instruction to finish by printing one machine-readable
   line, `FIRSTPASS-VERDICT: approve` or `FIRSTPASS-VERDICT: findings`, which `review` parses out
   of stdout. firstpass never sees a finding — `/code-review` posts them itself — so this line is
   the only channel between what the reviewer concluded and what firstpass can act on. On a second
   pass the system prompt also carries a note that a previous automated pass reviewed this pull
   request at a named commit and posted its findings as inline comments, asking the reviewer to
   concentrate on what has changed and not to restate that pass's findings — without it a second
   pass repeats every unfixed finding on the same lines. The `-p` value is byte-identical across
   passes.

   A `replay` sends that note too, which is why `ReviewOne` reads the review record before
   `opts.replay` makes the pipeline ignore it — read, not obeyed. The recorded head SHA is the
   evidence that a pass happened at all, so a skipped or expired row yields no note.

   The note only ever claims what is true of the earlier pass, which takes four shapes. A pass that
   **did not finish** (`needs_attention`, reached only by `replay`) is described as such: some of
   its findings may be posted and some may not, nothing knows how far it got, so check the pull
   request before posting a comment and raise anything not already there — claiming it "posted its
   findings" would invite both a duplicate of the comments that did land and silence about the ones
   that never did. A pass that **posted nothing** — a dry run records `reviewed` but withholds
   `--comment` (`Review.DryRun`) — earns **no note at all**: there is nothing to duplicate and
   nothing to hold back, and the note's claims would both be false on the very report that is the
   documented gate before going live. And when the **head has not moved** from a commit some pass
   reviewed (again only via `replay`), "concentrate on what has changed" names changes that do not
   exist and a blanket "do not restate" withholds the findings the command was run to get, so that
   framing is dropped for a request for a full review, keeping only "check before posting a comment
   that may already be there".

   The instruction is a system prompt, not part of the `-p` value: everything after `/code-review`
   there becomes the slash command's `$ARGUMENTS`, which parses an effort level, a `--comment`
   flag and a target, so prose appended to it would at best be dropped and at worst perturb the
   target.
8. After a review that succeeded, `ghpr.SubmitReview` submits that verdict as a GitHub review:
   `gh pr review <n> --repo <owner>/<repo> --approve` for `approve`, `--comment` for `findings`. A
   dry run submits nothing and says in its report what the verdict would have been. A failed,
   killed or timed-out review submits nothing at all. A missing or unrecognised verdict line
   submits nothing and is recorded as `reviewed` with the verdict `unknown`; it is never guessed.
9. `store.Record` writes the outcome and the submitted verdict, then the worktree is removed.
10. The watermark advances only after the entire batch is recorded.

## State

`bbolt` at `%LOCALAPPDATA%\firstpass\state.db`, four buckets:

- `meta` — `watermark` -> `{messageName, createTime}`. The key is the message **name**
  (`spaces/.../messages/...`), which is stable and unique. `createTime` has ties and clock skew, so
  it serves only as a coarse pagination bound.
- `reviews` — `owner/repo#n` -> outcome, submitted verdict, head SHA, triggering message, duration,
  exit code, report path, pass number and previous head SHA. The last two are what a second pass
  adds: the pass number so `status` can tell a first pass from a later one, and the previous head
  SHA so the commit whose comments are already on the pull request is not lost when the head SHA is
  overwritten. Both were added after the tool was in production, so a pre-existing row carries
  neither: it decodes as pass 0 on the wire and is read through `Review.PassNumber`, which reports
  the first pass it was. "Pass 0" is a state no code ever means.
- `pending` — `owner/repo#n` -> `{firstSeen, attempts, lastAttempt, lastReason, lastPausedAt,
  triggerMessage, triggerTime}`. The last two are the park's provenance, restored onto the candidate
  when the ref is re-offered: without them a deferred ref came back anonymous, so a re-post the
  per-sweep cap or a transient `gh` failure had parked could never satisfy the dedupe's second
  condition and was lost for good — and its row could never be retired either, because the record
  gate's skip returns above `expirePending`.
- `messages` — `spaces/.../messages/...` -> `{name, refKeys, firstSeen, watchApplied, watchReaction,
  resultApplied, resultReaction}`. Never pruned, deliberately: there is no delete method at all.
  The bucket grows one small row per chat message that carried a PR link, beside the `reviews`
  bucket's one row per PR, and pruning settled rows would discard the very state that stops a
  message being reacted to twice. Added after the tool was already in production, so it is created
  by `CreateBucketIfNotExists` alongside the other three and an existing database simply gains it.

  This bucket exists because the reaction is per message: the result reaction is only correct once
  every PR that message carried is terminal, which can be days later, after the message has left the
  fetch window and after any number of restarts. Nothing in it is ever read to decide whether a PR
  gets reviewed — the `reviews` bucket alone decides that — so a message record that is missing,
  stale or unwritable costs a reaction and nothing else.

### Terminal versus deferred outcomes

Dedupe on PR identity alone would mishandle a draft PR: recorded as skipped, marked ready an hour
later, and then never reviewed, because the watermark has passed the message. The same applies to a
single transient `gh` failure.

The dividing line is whether `claude` has started. Anything that fails **before** it starts has
posted nothing and is safe to retry; anything that fails **after** may have posted part of a comment
set.

| Class | Outcomes | Behaviour |
|---|---|---|
| Terminal | reviewed, author-is-user, merged, closed, owner not allowed, repo denylisted | Stored in `reviews`. Never revisited, with one exception: a `reviewed` record is superseded by a second pass when the PR is re-posted by a different message and has new commits. A re-post that then skips at a gate below the record gate — merged, closed, drafted, reassigned, backlog expired — leaves the `reviewed` record standing rather than overwriting it with the skip. |
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
`review_timeout` is generous (30m, raised from 20m after a real review took 12m15s) — a timeout is a manual-intervention event, so it should only
fire when something is genuinely stuck.

### Chat reactions

Live only. 👀 goes on immediately before the first `Rev.Run` for a PR that message carried — the
first moment "this is being reviewed" is true, and the last moment it is still useful to say. Once
every PR that message carried has a terminal review record, the result reaction goes on and the 👀
comes off.

**The result is decided by the verdict, not by the outcome.** ✅ means every PR this message
carried **that firstpass reviewed** was recorded `store.VerdictApproved` — the verdict that
submitted an approving GitHub review. Everything else is 💬. The verdict is the only signal that
distinguishes clean from not: the findings are inline comments on the pull request and the pipeline
never sees one, so an outcome-based rule would call a PR with twenty comments on it clean, purely
because the review process completed.

Two asymmetries are deliberate:

- **PRs firstpass never reviewed are left out of the decision.** Skipped for the owner, the deny
  list, the state, the author, or retired by pending expiry: a skip is not a finding — nothing was
  wrong with the code, firstpass simply had no business reviewing it. A message carrying one
  approved review and one merged link is clean, and gets ✅.
- **Everything short of an outright approval is 💬.** `store.VerdictApproved` is only ever set when
  a submission actually succeeded, so a findings verdict, a reviewer that printed no verdict line,
  a submission that failed, and a review that did not finish at all are all "firstpass does not
  know that this is clean" — and not knowing is never rendered as ✅. A misleading tick on a PR
  with comments waiting is worse than no reaction, so an unrecognised outcome takes the pessimistic
  reading too.

The rules. Each is pinned by a test that was checked to fail when its own guard
is reverted — including the ones that read as belt and braces, because those are the ones a suite
will happily let rot:

1. **Nothing that was not reviewed is reacted to.** `watchApplied` is the "a review actually
   started" flag, and no `watchApplied` means no result reaction either — so a message whose every
   link was skipped for its owner, the deny list, its state, its author, being a draft, the
   per-sweep cap, a pause or print-only gets no reaction at all. `settleMessageReaction` says it
   again independently: if, after excluding the skips, no ref was reviewed, it reacts to nothing.
   The two can disagree, because the ref list is re-read from the message text every sweep and a
   chat message edited after its review started ends up carrying a 👀 and a ref list holding
   nothing firstpass reviewed. Without the second check the ref loop is vacuous and the message
   earns a bare ✅ for work firstpass never did.
2. **Never twice for the same stage.** Each stage is recorded in the store *before* the API call,
   the same discipline as writing a review record as `in_flight` before `claude` starts: an
   outward act that might be repeated is worse than one that is occasionally missed, and a reaction
   is cosmetic either way. A backfill or a watermark gap re-offering the message changes nothing.

   What that trades away is a reaction lost to a transient failure — so the one transient failure
   that is not rare is caught before the latch. **An already-cancelled context reacts to nothing.**
   Ctrl-C during a review is how the daemon is normally stopped and a review runs for up to thirty
   minutes, so a dead context by the time a reaction is attempted is routine. The call cannot
   succeed on one, so latching first would turn a transient interrupt into a permanent one: the
   message would keep 👀 for good, never get its result, and never be looked at again, because the
   latch says it is finished. `Sweep` guards its own end-of-sweep pass the same way; both inline
   calls from `handle` do too, and a later healthy sweep picks the work up.
3. **A reaction failure never touches a review.** It is logged and dropped: no `needs_attention`, no
   pending entry, no held watermark, and the next review runs exactly as before.
4. **`dry_run` and `-print-only` react to nothing**, and record no reaction state either. A `PAUSE`
   file stops reactions with everything else outward-facing, deferring rather than dropping them.
5. **Reaction state is strictly additional.** Its own bucket, its own key; no reaction path ever
   writes a `Review`.

A `replay`'s trigger is the literal `"replay"` and identifies no chat message, so it can add no 👀.
A PR re-offered from `pending` now can: the pending row records the message that offered it and its
post time (`TriggerMessage`, `TriggerTime`), restored onto the candidate when the ref comes back, so
a draft that becomes ready a day later or a review the per-sweep cap pushed to the next sweep is
attributed to the post that asked for it. Before that, firstpass could review a pull request and
leave the post that carried it showing nothing at all. A row written before those fields existed
carries no message and reacts to nothing, as a `replay` still does. Either path can still finish a
message off, which is why an end-of-sweep pass over the `messages` bucket offers a result reaction
to every message still waiting on one, rather than relying on the candidate that happened to
finish last.

## Safety controls

1. **`dry_run: true` in the shipped config.** Runs `/code-review` without `--comment` and writes
   findings to `%LOCALAPPDATA%\firstpass\reports\<owner>_<repo>_<n>.md` — and a later pass to
   `<owner>_<repo>_<n>_after_<short-sha>.md`, so it does not overwrite the report the previous pass
   left behind. Same pipeline, same worktree, same reviewer, no GitHub write.
2. **Cold-start guard.** A first run with an empty watermark reviews nothing: it sets the watermark
   to the newest message and stops. `scan --backfill N` deliberately takes the last N messages.
3. **Kill switch.** A `PAUSE` file in the state dir, written and removed by `pause` / `resume`.
   While it is present, sweeps poll and queue into `pending` but run no reviews and post nothing.
   Refs queued while paused do **not** increment `attempts`, so a week-long pause cannot silently
   expire a backlog. A file rather than a signal, because it works when the daemon is wedged and it
   survives a restart.
4. **Throttles.** `max_reviews_per_sweep: 3`, `review_timeout: 30m`, strictly one review at a time.
5. **`allow_owners`, defaulting to `[Example-Org]`.** A PR whose owner is not on the list is
   recorded terminal and never touched. This is not the same control as `deny_repos`, and it matters
   more: the space is a chat room, so someone will eventually paste a link to an unrelated
   open-source PR. Without an owner allowlist, `firstpass` would clone a stranger's repository and
   post review comments on it. The allowlist makes the default blast radius the team's own org.
6. **`deny_repos`.** Never acted on; recorded terminal. For carving specific repos out of an
   otherwise allowed owner.
7. **Reactions are cosmetic by construction.** One gate (`reactionsEnabled`) refuses in a dry run,
   in print-only mode, and when no reactor is wired; `cmd` additionally wires no reactor in a dry
   run. Every reaction failure is logged and dropped — it cannot change an outcome, defer a PR, or
   alter the verdict submitted on one.

## Configuration

`%APPDATA%\firstpass\config.yaml`, every field overridable by flag:

```yaml
space: spaces/EXAMPLE123
github_login: angelov-todor
interval: 5m
dry_run: true
max_reviews_per_sweep: 3
review_timeout: 30m
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
- **`pipeline`** — fakes for `chat`, `ghpr`, `review` and the chat reactor. Covers the full decision table: skip own,
  draft to `pending`, merged terminal, owner not in `allow_owners`, in-batch dedupe, per-sweep cap,
  `PAUSE` file (including that it does not increment `attempts`), cold start reviewing nothing,
  timeout landing on `needs_attention` rather than retrying, and the watermark advancing only after
  a fully recorded batch. Each of the three second-pass conditions is pinned in both directions,
  including the two that must be decided without a GitHub call at all — those assert on the absence
  of the `Inspect` call, not on the outcome, because an outcome-only assertion would pass just as
  happily with the gate moved below GitHub.
- **`store`** — real bbolt in a temp dir, reopened mid-test to prove `in_flight` and the watermark
  survive a restart, and a fixture database built with only the original three buckets to prove a
  pre-existing database gains `messages` and keeps its records.
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
- That post carries 👀 while its PRs are being reviewed, and exactly one result reaction once they
  are all finished: ✅ only if every PR on it that firstpass reviewed was approved. A post whose
  PRs were all skipped carries neither reaction.
- The user's own PRs, drafts, and merged or closed PRs receive no comments.
- No PR receives two comment sets for the same commit, including across daemon restarts and
  crashes. A PR re-posted with new commits does receive a second, fresh set — that is the second
  pass, and it is deliberate.
- A first run against a populated space reviews nothing.
- `pause` stops all posting within one sweep.
- The user's working copies in `example-org/` are never modified.
