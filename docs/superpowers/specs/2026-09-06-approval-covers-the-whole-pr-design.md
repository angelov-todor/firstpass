# An approval covers the whole pull request

Status: approved for implementation
Date: 2026-09-06

## The problem

An approval submitted by firstpass currently means less than it appears to.

Two separate narrowings produce that:

1. **A later pass only looks at new commits.** `secondPassNote` tells the
   reviewer to "concentrate on what has changed since <sha>" and not to
   restate the previous pass's findings. So an approval on pass 2 means "the
   commits added since pass 1 raised nothing" — while pass 1's findings may sit
   unaddressed above it. Production has already produced passes 2, 3 and 5.
2. **No pass has ever looked at anybody else's review.** firstpass reads
   `state`, `isDraft`, `author` and `headRefOid` and nothing else. A pull
   request can carry a colleague's review asking for changes, and firstpass
   will approve it.

Submitted under the operator's own GitHub identity, an approval that appears to
bless a change nobody has finished reviewing is the most expensive thing this
tool can get wrong. The owner's requirement: **an approval must cover all the
code in the pull request, and every mandatory point already raised on it — by
anyone — must be addressed. Minor nits need not block.**

## What was measured first

Against real pull requests, before designing:

- Feedback lives on **three** surfaces, and the team uses all of them.
  `aex-margin-service#197`: 2 reviews (one `DISMISSED` carrying substantive
  text, one `APPROVED` with an empty body) and **6 general PR comments**, which
  is where the actual human review content was. `aex-backoffice#309`: 3 inline
  review threads. Reading one surface would miss most of the feedback.
- **`isOutdated` is not "addressed".** All three threads on #309 are
  `isResolved=false, isOutdated=true`: the code moved, which may mean fixed or
  merely shifted. Only reading the code can tell.
- **One `gh api graphql` call returns all of it**, plus `reviewDecision`:
  threads with resolution and outdated flags, review states and bodies, and
  comments.
- **Bots post on these surfaces too.** One of #197's six comments is
  `github-actions` posting a CI summary. Unmarked, that reads as feedback
  needing to be addressed.

## Decisions

| Question | Decision | Why |
| --- | --- | --- |
| Who judges "addressed"? | The reviewer | It requires reading the code. firstpass never sees a finding |
| How does the reviewer learn what exists? | firstpass injects a compact **index**; the reviewer fetches full text with `gh` | An instruction to go and look is the exact shape that was ignored for fourteen reviews. Evidence in the prompt cannot be silently skipped, and an index keeps the prompt bounded |
| Which surfaces | Unresolved threads, resolved threads, review bodies, general comments | Owner's decision, all four. Resolved threads are included so one closed without a fix is still caught |
| Re-posting on resolved threads | Never | Including resolved threads is for the *verdict*, not for re-litigating what a teammate closed |
| Bot authors | Marked in the index | A CI summary is not review feedback |
| Feedback unavailable | **No approval.** A comment review instead | An approval resting on evidence firstpass failed to gather is worse than a comment |
| Outstanding human `CHANGES_REQUESTED` | **Never approve** | firstpass must not appear to clear a human's block |
| Comment scope on a later pass | Still only the new commits | Unchanged: a second copy of the same comment spends the author's time twice |
| Verdict scope on any pass | The whole pull request | The change this spec exists for |

## Design

### `ghpr.Feedback`

One new method, one `gh api graphql` call, bounded by `GHTimeout`.

```go
type FeedbackItem struct {
    Surface  string // "thread" | "review" | "comment"
    Author   string
    IsBot    bool
    Path     string // threads
    Line     int    // threads
    Resolved bool   // threads
    Outdated bool   // threads
    State    string // reviews: COMMENTED | CHANGES_REQUESTED | APPROVED | DISMISSED
    Excerpt  string // first non-empty line, truncated
    URL      string // so the reviewer can fetch the full text
}

type Feedback struct {
    Items          []FeedbackItem
    ReviewDecision string // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
    Truncated      bool
}
```

Excerpts are truncated to a fixed width and the item count is capped;
`Truncated` says so, and a truncated index is treated as **feedback
unavailable** for approval purposes, because "there is more you have not been
shown" cannot support "everything has been addressed".

### Where each piece goes in the prompt

The split follows what this project learned the hard way about which channel
is obeyed:

- **The instruction goes in the `-p` value**, next to the verdict ask: before
  printing `approve`, confirm every mandatory point in the list below has been
  addressed in the current code; nits need not block; do not re-post comments
  that are already on the pull request.
- **The index goes in `--append-system-prompt`.** It is evidence, not a
  demand for output. It cannot be ignored the way an instruction can, because
  the reviewer needs it to answer the question the prompt asks.

### `secondPassNote`, rewritten

The two scopes are separated explicitly, because conflating them is the bug:

- **What to comment on:** the commits added since the previous pass. Do not
  restate findings already on the pull request.
- **What the verdict covers:** the whole pull request, including whether every
  mandatory point already raised on it has been addressed.

### The deterministic gates

The reviewer's judgement decides `approve` vs `findings`. Two conditions
override an `approve` in `submitVerdict`, and both submit a comment review
instead, recording why in `Review.Detail`:

1. `ReviewDecision == CHANGES_REQUESTED`.
2. The feedback could not be fetched, or was truncated.

These are gates rather than prompt instructions because they are the two cases
where firstpass has grounds to refuse regardless of what the reviewer decided.

## Failure modes

| Situation | Behaviour |
| --- | --- |
| `gh` fails or times out fetching feedback | No approval; comment review; detail says the feedback was unavailable |
| Feedback index truncated | Same as unavailable |
| A human has requested changes | No approval; comment review; detail names it |
| Only bot comments exist | Approval still possible: bots are marked and are not review feedback |
| Every thread resolved and nothing else open | Approval possible on the reviewer's judgement |
| Reviewer approves while an unresolved mandatory thread stands | Submitted as approve. firstpass cannot judge "mandatory", and the owner's requirement allows nits to be omitted; the index is what makes this the reviewer's informed decision rather than an oversight |

## Testing

- `ghpr.Feedback` against a `runner.Fake` replaying a **captured** GraphQL
  payload from a real pull request — all three surfaces, a bot author, a
  resolved thread and an outdated one. Captured, not invented: an invented
  payload shape is what shipped the `list-spaces` wrapper defect.
- The index renderer: bots marked, excerpts truncated, resolution and outdated
  flags visible, cap enforced and `Truncated` set.
- The gates: `CHANGES_REQUESTED` turns an approve into a comment; unavailable
  feedback turns an approve into a comment; neither touches a `findings`
  verdict; both record a detail naming the reason.
- The prompt: the instruction is in the `-p` value and the index in the system
  prompt, asserted independently so a reshuffle cannot quietly swap them.
- `secondPassNote`: comment scope and verdict scope are both stated, and the
  verdict scope is not narrowed to the new commits. Asserted by meaning rather
  than one phrasing — that guard has been fooled by rewording before.

## Success criteria

1. An approval is never submitted on a pull request whose `reviewDecision` is
   `CHANGES_REQUESTED`.
2. An approval is never submitted when firstpass could not enumerate the
   existing feedback.
3. The reviewer is shown every open item across all four surfaces, or told the
   list is incomplete.
4. A later pass still does not re-post comments already on the pull request.
5. Each behaviour fails a test when reverted.

## Out of scope

- Judging "mandatory" in firstpass. It never sees a finding; that is the
  reviewer's call by construction.
- Resolving threads. firstpass never marks anybody's thread resolved.
- Changing `request-changes` policy: firstpass still never submits one.
