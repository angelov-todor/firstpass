package review

import "strings"

// UsageLimitError reports that the review did not run because the Claude
// account has no capacity left, rather than because anything is wrong with the
// pull request.
//
// The distinction is the whole point. Every other claude failure is recorded
// needs_attention: terminal, never retried automatically, and warning the
// operator that comments may be half posted. That is right for a killed review
// and wrong for this, which posted nothing, will succeed unchanged once the
// limit resets, and applies to every pull request equally rather than to this
// one.
//
// Without it, an account that ran out of tokens mid-sweep stranded up to three
// pull requests per sweep, each needing a hand-typed `firstpass replay`. The
// operator's own defence was to notice and run `firstpass pause`, which worked
// and should not have been necessary.
type UsageLimitError struct {
	Err error
	// Matched is the phrase that identified it, kept so the log says what was
	// recognised rather than only that something was.
	Matched string
}

func (e *UsageLimitError) Error() string {
	return "claude usage limit reached (" + e.Matched + "): " + e.Err.Error()
}
func (e *UsageLimitError) Unwrap() error { return e.Err }

// usageLimitPhrases are the strings the claude CLI actually emits when an
// account is out of capacity.
//
// Taken from the shipped binary rather than invented: the 220 MB bundle
// contains "You've hit your limit", "You've hit your fast limit", "You've hit
// your monthly limit" and "You've hit your monthly spend limit", alongside a
// USAGE_LIMIT_ERROR_PREFIXES constant whose value is minified beyond reading.
// So this list is grounded but not authoritative, and the design accounts for
// that: an unmatched failure keeps the old behaviour exactly, so a phrase this
// list misses costs a needs_attention record -- today's outcome -- and never
// something worse.
//
// Deliberately not matching "rate limit" on its own. That phrase appears in
// the CLI about GitHub's rate limiting and about bash tool concurrency, and
// treating either as an account limit would defer a pull request that a plain
// retry will never fix.
var usageLimitPhrases = []string{
	"you've hit your limit",
	"you've hit your fast limit",
	"you've hit your monthly limit",
	"you've hit your monthly spend limit",
	"you've reached your usage limit",
	"claude usage limit reached",
	"usage limit reached",
	"upgrade to increase your usage limit",
}

// UsageLimitPhrase returns the phrase that identifies output as a usage-limit
// failure, or "" when none does.
//
// Both streams are searched. The CLI writes its limit notice to stderr when it
// refuses outright, and into stdout when it stops part-way through a session,
// and firstpass has no way to know in advance which happened.
func UsageLimitPhrase(stdout, stderr []byte) string {
	hay := strings.ToLower(string(stdout) + "\n" + string(stderr))
	for _, p := range usageLimitPhrases {
		if strings.Contains(hay, p) {
			return p
		}
	}
	return ""
}
