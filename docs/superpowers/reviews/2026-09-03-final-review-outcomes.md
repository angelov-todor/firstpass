# firstpass — final review outcomes and open decisions

**Date:** 2026-09-03
**Branch:** `firstpass-impl`
**Status:** implementation complete and reviewed. Decision 1 has since been narrowed by the owner
and decision 2 is closed — see the notes under each. A live run has still never happened.

All twelve planned tasks were implemented, each individually reviewed for spec compliance and
quality, with eight fix rounds. A whole-branch review then found defects that existed only in the
composition; eighteen were fixed in `8c49966` and verified by a scoped re-review (all eighteen
addressed, no new Critical or Important breakage, merge-ready).

This document records what the reviews found that is **not** resolved in code, so it survives in git
rather than in a scratch directory.

## Open decision 1 — the review subprocess is not sandboxed (security)

`config.Default()` sets `ClaudeArgs: ["--permission-mode", "bypassPermissions"]`. It is there for a
real reason: headless `claude -p` cannot run `gh` to post a comment under the default permission
mode, because it would block on a prompt nobody can answer.

**The design documents originally claimed this grant was bounded "inside a throwaway worktree". That
claim was false and has been corrected in the plan.** A working directory is not a sandbox:

- The subprocess inherits the parent's full environment. It can write anywhere the operator can,
  read `~/.ssh` and `~/.claude`, and use `gh` with the operator's token.
- It can therefore reach repositories outside `allow_owners`. That allowlist gates which PRs firstpass
  *decides* to review; it sits upstream of a process it cannot confine.
- The input is attacker-influenced. The worktree contains `refs/pull/N/head`. For a fork PR that is
  code authored by someone outside the org — `allow_owners` constrains the *repository* owner, not
  the *PR author*. That checkout may carry its own `CLAUDE.md`, `.claude/settings.json`, or
  `.claude/skills/*`, which a headless agent with unrestricted tools and no human at the prompt
  reads as instructions.

The owner accepted the risk of posting machine-written comments to colleagues' PRs. This is a
different and larger risk, and it was accepted on the strength of a claim that was not true.

Mitigations, roughly in descending value:

1. Replace `bypassPermissions` with an explicit `--allowed-tools` set sufficient for
   `/code-review --comment` and nothing more.
2. Point `--settings` at a firstpass-owned settings file, so project-local settings and hooks in the
   reviewed checkout are not honoured.
3. Consider `--add-dir` restrictions.
4. If the org accepts PRs from outside contributors, treat that as the actual threat model rather
   than trusting the org allowlist.

Nothing in this area was changed by the fix wave: `ClaudeArgs` is byte-identical to what the plan
specified, deliberately, because tightening it could break the review's ability to post at all and
that is the owner's call.

### Owner's decision, 2026-09-04 — NARROWED, not closed

The owner has accepted the subprocess **reading** across their workspace, on the grounds that wider
context produces better reviews. That is well-founded: in the first real dry run the reviewer opened
a file in a *different* repository to check the premise of the change it was reviewing, and its
findings were stronger for it. Mitigation 3 (`--add-dir` restrictions) is therefore declined.

What that decision does **not** cover, and what remains open:

- **Writing.** The subprocess inherits the environment, including a `GITHUB_TOKEN` whose scopes
  include `admin:org` and `admin:enterprise`. Reading being desirable places no limit on pushing,
  commenting, or deleting anywhere that token reaches — including repositories outside
  `allow_owners`, which gates only which PRs firstpass *chooses* to review.
- **Injection.** The reviewed checkout is the pull request author's code. On a fork PR that author is
  outside the org, and the checkout may carry its own `CLAUDE.md`, `.claude/settings.json` or
  `.claude/skills/*`, which a headless agent with no human at the prompt reads as instructions.

Mitigations 1, 2 and 4 above still stand unaddressed, and mitigation 2 (`--settings`) is the cheapest
answer to the injection half specifically. Accepted knowingly for now; recorded here so the next
reader does not mistake the narrowing for a clean bill of health.

## Decision 2 — CLOSED: the review stage has now been executed

The operator checkpoint that ran was `scan -print-only -backfill 200`, which validated PR extraction
against 200 real messages (70 references found, independently cross-checked as exactly right) but
returns before the worktree and reviewer are ever reached.

The whole-branch review found that the review stage as originally planned would have reviewed
*nothing*: the prompt was a bare `/code-review` with no PR target, and the prepared worktree had a
detached HEAD, no `refs/remotes/origin/*`, and an empty `git diff`. Both halves are fixed in
`8c49966` — the prompt now names the PR (`/code-review <url>`), and the mirror fetches branches into
remote-tracking refs so a diff base resolves.

**Both fixes are now confirmed end to end.** On 2026-09-04 the owner ran a dry-run
`replay` of a real 3-commit pull request. It completed in 12m15s and produced a substantive report:
the reviewer saw the actual diff (`master...HEAD`, 3 commits), which proves both halves — the PR URL
reached `/code-review`, and the mirror's remote-tracking refs gave it a resolvable base. It returned
four findings, each with a file:line and a concrete mechanism.

Two consequences were folded back into the code. `review_timeout` went 20m → 30m, because 12m15s is
61% of the old budget and a timeout becomes `needs_attention` that is never retried. And the run was
silent for its whole 12 minutes while holding the store lock, so `status` could not be run either —
which produced the progress-output feature.

**Still unverified: posting.** A live run has never happened, so `/code-review --comment` finding the
pull request and writing to it is the one part of the path still taken on faith.

The plan's Task 11 Step 7 was a "read a real dry-run report" checkpoint. It was not run, and that
omission is why the composition defect survived twelve per-task reviews: in the review package's
slice the prompt looked correct, and in the worktree package's slice the checkout looked correct.

## Parked findings

Real, non-blocking, deliberately not fixed at the time. Two are worth knowing before a live run.

Items 2 and 4 were subsequently fixed on the `review-followups` branch and are marked below; the rest
still stand, as do both open decisions above.

| # | Finding | Why parked |
|---|---|---|
| 1 | **Legacy mixed-case store keys stop deduping.** `ParseKey` now folds case but a stored `Review.Key` keeps its original spelling, so a lookup misses a pre-existing mixed-case row and that PR would be re-reviewed and re-commented. | The live `state.db` was checked and holds no PR keys, so there is no impact on this machine. It matters only for a database that acquires rows before another machine upgrades. A rekey-on-read in `recoverInFlight` would settle it. |
| 2 | ~~**A transient git failure destroys a healthy mirror.** `mirrorUsable` maps any failure of `git config --get remote.origin.url` — including "could not run git at all" — to "not usable", and `Prepare` then removes the mirror.~~ | **FIXED** on `review-followups`. `gitExit` now exposes the exit code, so only "git ran and reported no origin" discards the mirror; an inability to run git is returned as an error and the ref is deferred. |
| 3 | `WatermarkGap` can hold the watermark permanently if the watermark message is *deleted* from the space, and `-backfill` (the printed remedy) is itself a non-advancing case. | Reviews still progress via store dedupe; it is a stuck watermark plus a per-tick warning, not lost work. |
| 4 | ~~The message-ordering assertion false-positives if the newest message lacks `createTime` (zero time is `Before` everything).~~ | **FIXED** on `review-followups`. The comparison now applies only when both endpoints carry a non-zero `CreateTime`; a genuine oldest-first payload is still refused. |
| 5 | `gh_timeout: 1m` now interacts with `pending_max_attempts`: a persistently slow `gh` expires a PR terminally after ~20 sweeps, where before it was merely slow. | Accepted trade for having a timeout at all. |
| 6 | `watch`'s store-release window can collapse to milliseconds, because a sweep may legitimately run `3 × 30m` against a 5-minute interval — a 90-minute window in which `status`, `scan` and `replay` cannot open the store. (`review_timeout` was 20m when this was found and is now 30m, so the window is half an hour longer than the number originally recorded here.) | The requirement (release between ticks) is met; the observability goal behind it only partly. |
| 7 | `interval` is the one config field `watch` does not hot-reload — the ticker is built once. | Inconsistent rather than wrong. |
| 8 | `TestReviewOneIgnoresThePerSweepCap` is now vacuous by construction: a replay's fresh `SweepReport` starts at zero attempts, so the cap gate cannot trip. | Direct consequence of a fix instruction; the invariant now holds by construction. Residual value is its non-mutation assertion. |

Roughly eighteen further minor findings (regex breadth, structural duplication, doc-comment and
struct-tag inconsistencies, and test-or-report-only issues) were triaged as safe to leave
indefinitely.

## Process note

The per-task review loop worked: code quality inside each package is high and the fix rounds
converged. What the process lacked was a seat that owned the *composition* — the three most serious
findings were all cross-package or cross-process, and a single integration test, or one manual run of
the full path, would have caught the worst of them. A useful change for next time: make the operator
checkpoint exercise the *deepest* stage rather than the shallowest. `-print-only` was the cheap
checkpoint and it validated the parser beautifully, but it also guaranteed that the riskiest half of
the system reached final review unexecuted.
