package review

import (
	"strings"
)

// docsNote points the reviewer at the project's specification and compliance
// documentation. It travels in the system prompt, as context rather than as a
// demand.
//
// The material cannot travel any other way. The compliance books alone are
// around 700,000 words -- roughly five context windows -- and the whole
// documentation set is about 2.4 million. So the reviewer is given the paths
// and searches them, and the note names the small top-level set worth opening
// first: gap analysis, audit requirements, open questions, the service
// registry. Those come to about 36,000 tokens between them, which is a
// reasonable read; the books are for grepping.
//
// The team's own retrieval procedure is named explicitly rather than left to be
// discovered. The skill that describes it lives in the docs checkout, outside
// the directory Claude Code searches for skills, so nothing loads it
// automatically -- and pointing at the file is deterministic in a way that
// hoping a skill gets selected is not. That distinction is the same one that
// cost fourteen reviews their verdict.
//
// The citation requirement is the important sentence. firstpass submits under
// the operator's own GitHub identity, and a fabricated regulatory claim on a
// colleague's pull request -- "this violates Reg-T" when it does not -- costs
// far more than a wrong null-check comment. A finding that cannot name the
// document and section it rests on is not to be raised at all.
func docsNote(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	// ReplaceAll, not filepath.ToSlash. ToSlash replaces the *host's* separator,
	// so on Linux it leaves a Windows path untouched -- backslash is a legal
	// filename character there. That made this normalisation silently
	// platform-dependent, which the race container caught by failing where
	// Windows passed. A path bound for a prompt has to be normalised the same
	// way everywhere: the model reads backslashes as escapes, sends the
	// reviewer at a directory that does not exist, and the result is a review
	// with no compliance dimension that looks exactly like one that found
	// nothing to say.
	slash := strings.ReplaceAll(root, `\`, "/")

	return "This repository is part of a platform whose behaviour is governed by written business " +
		"specifications and by financial regulation. That material is checked out at:\n\n  " +
		slash + "\n\n" +
		"If this change touches anything such a specification or regulation may govern -- an " +
		"endpoint, a field, a threshold, a denial rule, a status transition, an emitted event, or " +
		"any margin, futures, liquidation, fee, interest, KYC, withdrawal, surveillance or " +
		"position-limit behaviour -- check it against that material before you conclude " +
		"anything.\n\n" +
		"The team's own procedure for this is written down. Read it and follow it:\n  " +
		slash + "/skills/checking-astraex-docs-before-implementing/SKILL.md\n\n" +
		"Worth knowing where things are. These are small and worth opening:\n" +
		"  " + slash + "/COMPLIANCE-GAP-ANALYSIS.md\n" +
		"  " + slash + "/AUDIT-TECHNICAL-REQUIREMENTS.md\n" +
		"  " + slash + "/COMPLIANCE-OPEN-QUESTIONS.md\n" +
		"  " + slash + "/SERVICE_REGISTRY.md\n" +
		"  " + slash + "/ARCHITECTURE_SERVICES.md\n" +
		"These are far too large to read whole -- search them:\n" +
		"  " + slash + "/development/COMPLIANCE_BOOKS/   (around 700,000 words)\n" +
		"  " + slash + "/development/COMPLIANCE/\n" +
		"  " + slash + "/development/ATS_FORM/\n" +
		"  " + slash + "/development/FINRA_QNA.md\n\n" +
		"Two rules about compliance findings, and they are not negotiable. **Cite the document " +
		"and the section** any such finding rests on, so a reader can check it. And if you cannot " +
		"cite it, do not raise it: a regulatory claim that turns out to be invented, posted under " +
		"a real engineer's name on a colleague's pull request, does more damage than the finding " +
		"could ever have been worth. Silence is the correct output for a suspicion you cannot " +
		"support."
}
