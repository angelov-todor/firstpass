# firstpass — final review outcomes and open decisions

**Date:** 2026-09-03
**Branch:** `firstpass-impl`
**Status:** implementation complete and reviewed; two items need the owner's decision before a live run

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

## Open decision 2 — the review stage has never been executed

The operator checkpoint that ran was `scan -print-only -backfill 200`, which validated PR extraction
against 200 real messages (70 references found, independently cross-checked as exactly right) but
returns before the worktree and reviewer are ever reached.

The whole-branch review found that the review stage as originally planned would have reviewed
*nothing*: the prompt was a bare `/code-review` with no PR target, and the prepared worktree had a
detached HEAD, no `refs/remotes/origin/*`, and an empty `git diff`. Both halves are fixed in
`8c49966` — the prompt now names the PR (`/code-review <url>`), and the mirror fetches branches into
remote-tracking refs so a diff base resolves.

**Those fixes are structurally verified but not end-to-end verified.** Confirming them means running
a real `claude` review over a checkout, which is exactly the action open decision 1 makes risky. So
the sequence is: settle decision 1, then run one dry-run review and read the report, then consider
`dry_run: false`.

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
| 6 | `watch`'s store-release window can collapse to milliseconds, because a sweep may legitimately run `3 × 20m` against a 5-minute interval. | The requirement (release between ticks) is met; the observability goal behind it only partly. |
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
