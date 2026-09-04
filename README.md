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
- `firstpass replay` and PRs re-offered from the backlog react to nothing on
  the way in. Neither identifies a chat message. They do still finish a
  message off: the sweep that decides a message's last pull request adds its
  result reaction, whichever path decided it.

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
and crashes never cause a PR to be reviewed twice. Which PRs each chat message
carried is recorded there too, because the result reaction can be hours behind
the post that triggered it.

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
  to real pull requests, under your GitHub identity — and what makes it react
  to real chat messages, under your Google identity. `dry_run` is an absolute
  "no outward effect" switch: a dry run does not react, does not submit
  a verdict, and does not even record the state a reaction would need.
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
