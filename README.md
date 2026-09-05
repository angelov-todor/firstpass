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
   drafts, and anything already reviewed **at the commit it is now at** — see
   [Second pass](#second-pass).
4. For each survivor, prepare a throwaway git worktree against a local bare
   mirror of the repository.
5. Run `claude -p "/code-review <pr-url>"` in that worktree, with a
   `--append-system-prompt` asking the reviewer to finish by printing one
   machine-readable verdict line. The verdict instruction travels as a system
   prompt rather than inside the `-p` value, because everything after the
   slash command there becomes the command's own arguments.
6. In dry run (the default), the findings are written to a report file. Live,
   `/code-review` posts them as inline comments on the PR.
7. Submit that verdict as a GitHub review, so a reviewed PR is never silent —
   a clean one used to produce nothing at all, indistinguishable from the tool
   never having run. Nothing needing change (no findings, or only minor nits)
   is an **approval**, posted under your own GitHub identity. Anything
   Critical or Important is a **comment** review, deliberately not
   request-changes: a comment leaves `reviewDecision` at `REVIEW_REQUIRED`, so
   the PR stays in the team's human review queue, whereas an approval would
   take it out. Every verdict body says in its first sentence that it is
   machine-written. A dry run submits nothing and its report says what the
   verdict would have been.
8. Live, the chat message that carried the link gets a reaction, so the team
   can see the PR has been picked up and, later, how it came out.

## Second pass

The dedupe rule is **once per commit, on a re-post** — not once per pull
request. A PR posted to the space again is reviewed again, but only if it has
new commits since the pass that already reviewed it.

**A second pass posts a fresh set of inline comments.** It is a whole review,
not an increment: the reviewer is told a previous pass ran and asked to
concentrate on what has changed and not to restate that pass's findings, but
nothing enforces it. Expect new comments on the pull request, and a new
verdict submitted alongside them.

What the reviewer is told depends on what the earlier pass actually did:

- **It posted its findings** — the usual live case. The reviewer is told which
  commit it reviewed and asked not to restate its findings, because they are
  already on those lines.
- **It did not finish** (a `needs_attention` record, reached only by
  `firstpass replay`). The reviewer is told plainly that some of that pass's
  findings may be posted and some may not, that nothing knows how far it got,
  and to check the pull request before posting a comment — and to raise
  anything that is not already there.
- **It posted nothing** — a dry run records a review but withholds
  `--comment`, so its findings went to a report on disk. The reviewer is told
  **nothing at all**: there is no comment to duplicate and nothing to hold
  back, and claiming otherwise would suppress every finding for no reason.
- **The head has not moved** (again only via `replay`). "Concentrate on what
  has changed" would name changes that do not exist, so instead the reviewer
  is asked for a full review, with the one caveat that still applies: check
  before posting a comment that may already be there.

All three of these must hold, or the re-post is skipped:

1. The existing record's outcome is `reviewed`, and it records the commit it
   reviewed. **No other outcome qualifies.** `needs_attention` in particular
   still needs an explicit `firstpass replay`: it means a review died
   mid-post, so comments may be half posted, and an automatic retry risks
   putting a second copy of each of them on a colleague's pull request. A
   re-post is not consent to that. Every skipped outcome fails for the plainer
   reason that nothing was ever reviewed.
2. The re-post is a **different chat message** from the one that triggered the
   recorded review, **and it was posted later than that message was**. The
   name check alone answers the wrong question. The same message can still be
   sitting in the fifty-message fetch window on the next sweep, and both a
   `-backfill` and a watermark gap re-offer *older* posts by design — while
   the sweep picks the oldest message carrying a link and the record holds the
   newest one seen. So the names differ, and an ordinary push nobody
   re-posted would otherwise become a second review.

   Comparing the two **post** times settles all of it, and it settles it for a
   reason that holds whatever the machine's clock says: both timestamps come
   from the Chat API, so any skew between Google's clock and this laptop's
   cancels instead of deciding the answer. Comparing a post time against the
   *review's* time would not — a clock an hour behind is enough to make a post
   that genuinely predates a review look later than it. A row written before
   the post time was recorded has nothing to compare and falls back to the
   review time, exactly as it behaved before.

   A re-post that arrives while its review is still running is refused and not
   kept: the review's own decision time is checked too, and a post from before
   it is dropped rather than deferred. That loses a nudge, silently; the next
   re-post triggers normally. It is the deliberate direction — a missed pass
   costs someone a second nudge, a spurious pass costs a colleague a duplicate
   comment set.

   A PR re-offered from the backlog carries the post that parked it, so a
   deferred second pass can still retry; one with no post recorded is not a
   re-post at all.
3. The **live head SHA is not a commit any pass has already reviewed** — not
   merely different from the last one. This is the invariant: no pull request
   is ever reviewed twice for the same commit, because that pass's comments
   are already on those exact lines. A head force-pushed back to an earlier
   reviewed commit is refused for the same reason as one that never moved.

Re-posted at an already-reviewed commit, the record's trigger message is
updated to the new post and nothing else about it changes, so the same re-post
is not re-inspected on every sweep for as long as it sits in the window.

The cost is one `gh pr view` per qualifying re-post — every re-post that gets
past conditions 1 and 2, not only the ones that turn out to have nothing new.
A re-post of a PR that has since been merged, been put back into draft or
changed hands pays that call too, and those skip *without* advancing the
recorded trigger, so each later re-post or re-scan of the same post pays it
again. All of them are reads; no new write of any kind is made.

`firstpass status` marks a later pass — `reviewed / findings (pass 2)`. A row
written before this feature existed has no pass number and reads as the first
pass it was. A dry run's report for a later pass is written to
`<owner>_<repo>_<n>_after_<short-sha>.md`, so it does not overwrite the report
the previous pass left behind: reading both is how you see what the new
commits changed.

Re-posted with no new commits, a merged, closed, drafted or reassigned pull
request keeps the record of the pass that reviewed it. Skipping is still right
— a merged PR must not be reviewed — but the reviewed commit, the submitted
verdict and the pass count are the only evidence of what firstpass did here,
and none of it is recoverable from anywhere else, so a skip does not overwrite
them. `firstpass status` therefore still shows such a PR as `reviewed`, not
`skipped_state`. Only the backlog entry is cleared.

`firstpass replay` still ignores the record when deciding whether to review —
that is the whole point of it, and none of the three conditions applies — but
it now **reads** the record first, so the reviewer is told that a pass has
already been here. That matters most for the case replay is documented for: a
`needs_attention` PR is one whose review died part-way through posting, and a
reviewer told nothing about that will restate every finding on the lines that
may already carry them. For such a record the reviewer is told plainly that
the earlier pass did not finish, that nothing knows how far it got, and to
check the pull request before posting a comment. A replay of a PR firstpass
has never reviewed is a first pass and says nothing. Unlike a re-post, a
replay that ends in a skip does record its own fresh decision: the operator
asked what firstpass makes of the PR now, and that is the answer.

## Chat reactions

Live only, a message that carried a PR link is reacted to:

- 👀 as soon as the first pull request from that message starts being
  reviewed.
- ✅ or 💬 once **every** pull request that message carried has reached a
  terminal outcome, at which point the 👀 is removed.

✅ means every pull request that message carried **and that firstpass actually
reviewed** came back with an **approve** verdict — the same verdict that
submits an approving GitHub review. 💬 means at least one of them wants a
human's eye: findings were raised, or the reviewer printed no verdict firstpass
recognised, or the verdict could not be submitted, or the review did not
finish. firstpass never reads ✅ into silence, so anything it does not know
counts as 💬.

Pull requests firstpass **skipped** are excluded from that decision. A skip is
not a finding — nothing was wrong with the code, firstpass simply had no
business reviewing it — so a message carrying one clean review and one merged
or out-of-org link still gets ✅.

The reaction belongs to the **chat message, not the pull request**. One post
routinely carries several links and reviews run strictly one at a time, so a
per-PR reaction would say nothing a reader could act on: the useful statement
is "this post has been picked up" and then "this post is done with".

Three consequences worth knowing:

- **A sibling PR that is not ready holds the result reaction back.** The
  result belongs to the message, so it waits until *every* link on that
  message is finished. A draft posted alongside a reviewable PR is deferred,
  not decided, and keeps being retried until it is marked ready or its backlog
  entry expires — up to `pending_max_age`, a week by default. Until then the
  message keeps 👀 even though the PRs firstpass could review are all done.
  That is deliberate: one result reaction per message is the whole point, and
  reacting before the message is finished would mean reacting twice. But it is
  the state you are most likely to see and misread, so `firstpass status` is
  the place to look — the draft will be sitting in the deferred list.
- A message whose every link was skipped — owner not on `allow_owners`, a
  denied repo, merged, closed, a draft, your own PR — gets **no reaction at
  all**. Nothing was reviewed, so there is nothing to report, and a bare ✅
  would be the first the team heard of it.
- `firstpass replay` reacts to nothing on the way in: its trigger is the
  literal `replay` and identifies no chat message. A PR re-offered from the
  backlog **does** now react, because a parked PR keeps a record of the post
  that offered it — so a draft that becomes ready a day later, or a review the
  per-sweep cap pushed to the next sweep, finally puts 👀 on the post that
  asked for it instead of leaving that post silent about a review that
  happened. A backlog row written before this was recorded carries no post and
  reacts to nothing, as before. Either way the message is still finished off:
  the sweep that decides its last pull request adds the result reaction,
  whichever path decided it.

Reactions are cosmetic, and firstpass treats them that way. A failed reaction
— a missing OAuth scope, a deleted message, a network blip — is logged and
nothing more: it never changes a review's outcome, never defers a PR, never
affects the verdict submitted on it, and never stops the next review.
`dry_run` reacts to nothing, `-print-only` reacts to nothing, and a `PAUSE`
file stops reactions along with everything else outward-facing.

The chat script needs two more subcommands for this, `add-reaction
<message-name> <emoji>` and `remove-reaction <reaction-name>`; see
`internal/chat` for the expected output shape. Without them, reactions fail
and are logged, and reviews carry on exactly as before.

Decisions and outcomes are recorded in a local `bbolt` database, so restarts
and crashes never cause a PR to be reviewed twice for the same commit. Which
PRs each chat message carried is recorded there too, because the result
reaction can be hours behind the post that triggered it.

## Prerequisites

- Go 1.24 to build.
- [`gh`](https://cli.github.com/), authenticated (`gh auth login`).
- [`claude`](https://claude.com/product/claude-code) on `PATH`.
- `git` on `PATH`.
- A script that can read your Google Chat space and print its messages as
  JSON on stdout, and add and remove reactions on a message.
  **firstpass does not talk to the Google Chat API directly** — it drives
  this script as a subprocess, the same way it drives `git` and `gh`. No such
  script is included in this repository; you need to supply or write one
  yourself (see `internal/chat` for the expected subcommands and output
  shapes). Reactions need an OAuth scope covering
  `spaces.messages.reactions`; `https://www.googleapis.com/auth/chat.messages`
  covers it.

## Install and configure

```
go build ./cmd/firstpass
```

Copy `config.yaml.example` to `%APPDATA%\firstpass\config.yaml` and fill in
the fields it marks as required: `space`, `github_login`, `allow_owners`, and
`paths.chat_script`. `firstpass doctor` refuses to say a fresh, unconfigured
install is healthy — it will tell you which of these is still missing.

Run `firstpass doctor` to check every external dependency (`git`, `claude`,
`gh` auth, whether the `gh` token actually carries the `repo` scope
`gh pr review` needs to submit a verdict, the chat script, and that the
configured Google Chat account can actually see named spaces).

## Commands

- `scan` — one sweep, then exit. Flags: `-print-only`, `-backfill N`,
  `-live`, `-quiet`. Also the intended Task Scheduler entry point.
- `watch` — sweep on a ticker until interrupted. Flag: `-live`.
- `status` — print the review table: what's been reviewed, skipped, or
  deferred.
- `replay <pr-url | owner/repo#n>` — force one PR through review again,
  ignoring the dedupe record but telling the reviewer that an earlier pass has
  already been here. Flags: `-live`, `-quiet`.
- `doctor` — preflight every external dependency.
- `pause` / `resume` — write / remove a kill-switch file. While paused,
  sweeps still queue new PRs but run no reviews and post nothing.

Every command accepts `-config <path>` to point at a config file other than
the default.

## Concurrency

By default reviews are serial: one pull request is reviewed to completion
before the next starts. Three posted at once, at roughly 12 minutes each, is
therefore about 36 minutes before the last one is done.

`review_concurrency` raises that. The default is `1`, so an upgrade never
makes firstpass do more at once than the operator asked for. Setting it above
`max_reviews_per_sweep` is allowed and just has no effect beyond the cap.

```yaml
max_reviews_per_sweep: 3
review_concurrency: 3
```

Sweeps themselves stay serial — one sweep finishes before the next begins.
That is what keeps `recover in-flight` sound: an `in_flight` record found at
the start of a sweep can only have come from a run that is no longer alive.

Two things worth knowing before raising it:

- **The limit you hit first is probably not this machine.** Each review is a
  `claude -p` session doing tool calls, and they share one account's rate
  limits. Past a small number, extra workers tend to queue rather than add
  throughput. Try 3 before trying more.
- **Ctrl-C costs more.** A hard kill mid-review leaves an `in_flight` record
  that becomes `needs_attention` and is never retried automatically. With
  three reviews in flight that is three of them. Stop the daemon between
  sweeps where you can.

Worktrees are per pull request, but the bare mirror behind them is per
repository, so `Prepare` takes a per-repository lock: two pull requests from
the same service queue for the clone and fetch, then review in parallel.

That lock covers checkout setup, not the reviews themselves. Worktrees share
the mirror's refs, so while one review is running, a sibling's fetch does move
`refs/remotes/origin/*` under it. Measured: a two-dot `git diff origin/main`
inside a live worktree grew a file the pull request never touched. A three-dot
`origin/main...HEAD` — which is what the reviewer actually uses — is immune,
because the merge base stays at the branch point. Making this airtight would
mean either a clone per pull request or serialising same-repository reviews,
which is the case worth parallelising most, so it is documented rather than
locked.

## Tests

```
go test ./...            # the suite
scripts/test-race.sh     # the suite under the race detector, in Docker
```

The race detector needs cgo and a working C toolchain. Rather than require one
on every machine, `scripts/test-race.sh` runs the suite in the container
defined by `Dockerfile.test`; nothing is installed on the host. It takes the
same arguments as `go test`:

```
scripts/test-race.sh ./internal/pipeline
scripts/test-race.sh -run TestSweep ./internal/pipeline
```

Run it before changing anything concurrent. It has already caught one defect
the Windows suite could not see: a cancelled command was killed but `Run` did
not return until it finished anyway, so `review_timeout` and Ctrl-C both failed
to stop a review.

## Safety

Read this before running anything other than `doctor` or `scan -print-only`.

- **`dry_run: true` is the default.** Passing `-live`, or setting
  `dry_run: false` in the config, is what makes firstpass post real comments
  to real pull requests, under your GitHub identity — and what makes it react
  to real chat messages, under your Google identity. `dry_run` is an absolute
  "no outward effect" switch: a dry run does not react, does not submit
  a verdict, and does not even record the state a reaction would need.
- **A re-post reviews the PR again, and posts a second set of inline
  comments.** Only when it has new commits since the last pass — see
  [Second pass](#second-pass) — but when it does, the author gets a fresh
  review on their pull request and a fresh verdict submitted under your
  identity. The reviewer is asked not to restate the previous pass's findings;
  that is a request in a system prompt, not a guarantee.
- **A clean PR is approved under your own GitHub identity.** Live, a
  successful review always ends in a submitted GitHub review: an approval when
  the reviewer raised nothing or only nits, a comment review when it raised
  anything Critical or Important. Your colleagues see both as *you*, which is
  why every body opens by saying it is machine-written and links this
  repository. A findings verdict is never request-changes, so a machine never
  blocks a merge on its own — but an approval does clear `reviewDecision`, and
  that is a real approval on someone else's work. Nothing is submitted unless
  the review itself succeeded and the reviewer printed a verdict line
  firstpass recognises: a missing or unrecognised line submits nothing and is
  recorded as `reviewed / verdict unknown`, never guessed into an approval. A
  submission that fails leaves the review recorded as `reviewed` with the
  error visible in `firstpass status`, and is not retried.
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
- **Inline comments are exercised; the verdict is not.** Parsing and
  filtering were checked against 200 messages of real chat history, and a
  dry-run `replay` produced a substantive report in 12m15s, which is why
  `review_timeout` defaults to 30m. Comments have since been posted live: on
  a 7-file, +539/−12 pull request firstpass left three inline comments,
  including one that falsified a security claim in the PR's own description,
  one `IsSuccessStatusCode` trap where a 2xx with an undeserialisable body
  silently skips an audit-trail write, and one null dereference whose broad
  `catch` then logged the opposite of what had happened. That is the standard
  to hold it to, and it is the reason to read your own dry-run report (`scan
  -backfill N`, or `replay <url>`, without `-live`) before setting `dry_run:
  false`.

  What has **not** been exercised is the verdict. No review verdict has ever
  been submitted by this tool: the code path is new, and no real `claude` run
  has yet been observed printing the `FIRSTPASS-VERDICT:` line the reviewer is
  asked for. Until you have seen a dry-run report say "the verdict would have
  been" with a value you agree with — several times — treat the verdict as
  unproven, and remember that the approve case posts a real approval under
  your identity.
