package review

// maxCommandLine is the hard ceiling Windows puts on a process's whole command
// line: 32,767 characters.
//
// Measured on the target machine rather than taken from documentation. A
// 33,000-character argument fails with "fork/exec: The filename or extension
// is too long"; 32,000 gets through the exec. firstpass is a Windows daemon,
// so this is not a theoretical limit.
const maxCommandLine = 32767

// argvBudget is what firstpass allows itself of that, leaving room for
// everything it does not control: the claude executable path, --add-dir with a
// docs path, and operator-supplied claude_args. Eight thousand characters of
// slack is generous for those.
const argvBudget = 24000

// fits reports whether the prompt and system prompt together leave the command
// line inside what the operating system will accept.
//
// This check exists because of the worst failure mode this project has: an
// exec that fails is a review that did not happen, recorded as
// needs_attention and never retried automatically. An oversized prompt would
// not degrade a review, it would take a pull request that reviewed fine
// yesterday and leave it permanently unreviewed -- silently, until somebody
// ran `firstpass replay` by hand.
//
// Every piece is already bounded: sibling diffs at 6 KB each and at most two
// of them, the feedback index at sixty items. So this should never fire. It is
// here because "should never" and "cannot" are different, and the difference
// is paid for by a pull request nobody reviews.
func fits(prompt, system string) bool {
	return len(prompt)+len(system) <= argvBudget
}
