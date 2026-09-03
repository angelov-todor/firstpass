// Package prref extracts GitHub pull request references from chat message text.
package prref

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PRRef identifies a single GitHub pull request.
//
// Owner and Repo are canonically lowercase. Every constructor in this package
// folds them, because Key() is a bbolt key and bbolt keys are case-sensitive:
// without folding, "example-org/aex-balances#91" and
// "Example-Org/aex-balances#91" would be two records for one pull request,
// and the second would earn it a second set of inline comments. GitHub URLs
// are case-insensitive and the operator's own checkout path for this org is
// lowercase, so a lowercased paste is routine rather than exotic. The mirror
// directory derived from these fields is case-insensitive on Windows, so
// folding also keeps the on-disk cache and the database agreeing.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// Key is the stable identity used for de-duplication and storage.
func (r PRRef) Key() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// URL is the canonical web URL for the pull request.
func (r PRRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Repo, r.Number)
}

const seg = `[A-Za-z0-9._-]+`

var (
	urlRe       = regexp.MustCompile(`https?://github\.com/(` + seg + `)/(` + seg + `)/pull/(\d+)`)
	anyUrlRe    = regexp.MustCompile(`https?://\S+`)
	shorthandRe = regexp.MustCompile(`\b(` + seg + `)/(` + seg + `)#(\d+)\b`)
	keyRe       = regexp.MustCompile(`^(` + seg + `)/(` + seg + `)#(\d+)$`)
)

// Extract pulls every distinct pull request reference out of a chat message.
// URLs are returned in order of appearance, followed by any shorthand
// references.
//
// A bare "#91" is deliberately not recognised. The team's chat rules record it
// as ambiguous — it reads as an issue in whatever repository the reader last
// had open — so resolving it would mean guessing a repository, and a wrong
// guess would review a stranger's PR.
func Extract(text string) []PRRef {
	var out []PRRef
	seen := map[string]bool{}

	add := func(owner, repo, num string) {
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 {
			return
		}
		ref := PRRef{Owner: strings.ToLower(owner), Repo: strings.ToLower(repo), Number: n}
		if seen[ref.Key()] {
			return
		}
		seen[ref.Key()] = true
		out = append(out, ref)
	}

	for _, m := range urlRe.FindAllStringSubmatch(text, -1) {
		add(m[1], m[2], m[3])
	}

	// Blank out all URLs before the shorthand pass, so fragments and path
	// components of any URL cannot be misread as shorthand references.
	rest := anyUrlRe.ReplaceAllString(text, " ")
	for _, m := range shorthandRe.FindAllStringSubmatch(rest, -1) {
		add(m[1], m[2], m[3])
	}

	return out
}

// ParseKey is the inverse of Key, used when reading refs back out of storage.
// It folds owner and repo, so a key written before canonicalisation still
// parses to the canonical ref, and ParseKey(ref.Key()) round-trips.
func ParseKey(s string) (PRRef, error) {
	m := keyRe.FindStringSubmatch(s)
	if m == nil {
		return PRRef{}, fmt.Errorf("not a PR key: %q", s)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return PRRef{}, fmt.Errorf("not a PR key: %q", s)
	}
	return PRRef{Owner: strings.ToLower(m[1]), Repo: strings.ToLower(m[2]), Number: n}, nil
}
