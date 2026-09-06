package review

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// procedureSkill is the team's own written retrieval procedure, if the docs
// checkout carries one.
//
// Named rather than left to be discovered. A skill inside the docs checkout
// sits outside the directory Claude Code searches for skills, so nothing loads
// it automatically -- and pointing at the file is deterministic where hoping a
// skill gets selected is not. That distinction is the same one that cost
// fourteen reviews their verdict.
const procedureSkill = "skills/checking-astraex-docs-before-implementing/SKILL.md"

// docsIndexLimit caps how many entries the note lists, so a docs checkout with
// two hundred top-level files cannot crowd out the review itself.
const docsIndexLimit = 20

// bigSubtreeWords is the point past which a subtree is described as searchable
// rather than readable.
const bigSubtreeWords = 40000

// docsNote points the reviewer at the project's specification and compliance
// documentation. It travels in the system prompt, as context rather than as a
// demand.
//
// The material cannot travel any other way. In the checkout this was built
// against, the compliance books alone run to about 700,000 words -- roughly
// five context windows -- and the whole set to 2.4 million. So the reviewer
// gets paths and searches them.
//
// What it lists is *discovered*, not hardcoded. The first version wrote ten
// astraex-specific filenames into the binary while the config documented
// docs_root generically, so any other checkout would have been handed nine
// paths that do not exist -- and doctor would still have passed, because the
// root itself was there. Discovery also means the note stays true as the docs
// change, which they do far more often than firstpass is rebuilt.
//
// The citation requirement is the important part. firstpass submits under the
// operator's own GitHub identity, and a fabricated regulatory claim on a
// colleague's pull request -- "this violates Reg-T" when it does not -- costs
// far more than a wrong null-check comment. A finding that cannot name the
// document and section it rests on is not to be raised at all.
func docsNote(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	// ReplaceAll, not filepath.ToSlash. ToSlash replaces the *host's*
	// separator, so on Linux it leaves a Windows path untouched -- backslash is
	// a legal filename character there. That made this normalisation silently
	// platform-dependent, which the race container caught by failing where
	// Windows passed. A path bound for a prompt has to be normalised the same
	// way everywhere: the model reads backslashes as escapes, sends the
	// reviewer at a directory that does not exist, and the result is a review
	// with no compliance dimension that looks exactly like one that found
	// nothing to say.
	slash := strings.ReplaceAll(root, `\`, "/")

	var b strings.Builder
	b.WriteString("This repository is part of a platform whose behaviour is governed by written " +
		"business specifications and, in places, by financial regulation. That material is " +
		"checked out at:\n\n  " + slash + "\n\n" +
		"If this change touches anything such a specification or regulation may govern -- an " +
		"endpoint, a field, a threshold, a denial rule, a status transition, an emitted event, " +
		"or any margin, futures, liquidation, fee, interest, KYC, withdrawal, surveillance or " +
		"position-limit behaviour -- check it against that material before you conclude " +
		"anything.\n")

	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(procedureSkill))); err == nil {
		b.WriteString("\nThe team's own procedure for this is written down. Read it and follow " +
			"it:\n  " + slash + "/" + procedureSkill + "\n")
	}

	if docs, dirs := surveyDocs(root); len(docs) > 0 || len(dirs) > 0 {
		if len(docs) > 0 {
			b.WriteString("\nTop-level documents, small enough to open:\n")
			for _, d := range docs {
				b.WriteString("  " + slash + "/" + d + "\n")
			}
		}
		if len(dirs) > 0 {
			b.WriteString("\nSubtrees. The large ones are far too big to read whole -- search " +
				"them:\n")
			for _, d := range dirs {
				b.WriteString("  " + slash + "/" + d + "\n")
			}
		}
	}

	b.WriteString("\nTwo rules about compliance findings, and they are not negotiable. **Cite " +
		"the document and the section** any such finding rests on, so a reader can check it. " +
		"And if you cannot cite it, do not raise it: a regulatory claim that turns out to be " +
		"invented, posted under a real engineer's name on a colleague's pull request, does more " +
		"damage than the finding could ever have been worth. Silence is the correct output for a " +
		"suspicion you cannot support.")
	return b.String()
}

// surveyDocs lists what the checkout actually holds: top-level markdown files,
// and subdirectories annotated with a rough size so the reviewer knows which
// it can read and which it must search.
//
// Errors are swallowed and produce an empty list. The note is still useful with
// only the root path in it, and a review must not fail because a documentation
// directory could not be listed.
func surveyDocs(root string) (docs, dirs []string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			w := subtreeWords(filepath.Join(root, name))
			switch {
			case w >= bigSubtreeWords:
				// Expanded one level, because the top level is the wrong
				// granularity for the checkout this was built against: a single
				// `development/` holds 2.4 million words, and telling the
				// reviewer "development/ -- search it" says almost nothing,
				// whereas naming COMPLIANCE_BOOKS and its size says where to
				// look. One level only: the point is an index, not a tree.
				kids := largeChildren(root, name)
				if len(kids) > 0 {
					dirs = append(dirs, kids...)
					continue
				}
				dirs = append(dirs, fmt.Sprintf("%s/   (roughly %s words -- search, do not read)",
					name, thousands(w)))
			case w > 0:
				dirs = append(dirs, name+"/")
			}
			continue
		}
		if strings.EqualFold(filepath.Ext(name), ".md") {
			docs = append(docs, name)
		}
	}
	sort.Strings(docs)
	sort.Strings(dirs)
	if len(docs) > docsIndexLimit {
		docs = docs[:docsIndexLimit]
	}
	if len(dirs) > docsIndexLimit {
		dirs = dirs[:docsIndexLimit]
	}
	return docs, dirs
}

// largeChildren lists the immediate subdirectories of one large subtree, each
// with its own size, so the index names the places worth searching rather than
// the container they happen to sit in.
//
// Only children that hold markdown are listed. A directory of raw PDFs or HTML
// is not something the reviewer can search for a rule, and naming it would
// spend the index's budget on a dead end.
func largeChildren(root, parent string) []string {
	entries, err := os.ReadDir(filepath.Join(root, parent))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel := parent + "/" + e.Name()
		w := subtreeWords(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case w >= bigSubtreeWords:
			out = append(out, fmt.Sprintf("%s/   (roughly %s words -- search, do not read)",
				rel, thousands(w)))
		case w > 0:
			out = append(out, rel+"/")
		}
	}
	sort.Strings(out)
	if len(out) > docsIndexLimit {
		out = out[:docsIndexLimit]
	}
	return out
}

// subtreeWords is a cheap size estimate: bytes of markdown divided by six.
//
// Bytes rather than words because counting words means reading every file, and
// this runs before every review. Six bytes per word is close enough for the
// only decision it drives -- readable or searchable -- and being wrong by a
// factor of two either way does not change that answer near the threshold.
func subtreeWords(dir string) int {
	var bytes int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable corner must not fail the review
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			bytes += info.Size()
		}
		return nil
	})
	return int(bytes / 6)
}

// thousands renders 703014 as "700,000": the reviewer needs the order of
// magnitude, and a precise figure would imply the estimate is precise.
func thousands(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f million", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%d,000", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
