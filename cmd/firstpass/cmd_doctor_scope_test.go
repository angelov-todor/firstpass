package main

// `gh pr review` is the first writing gh command firstpass runs. `gh auth
// status` says the token is authenticated, which is not the same as allowed
// to review, and the difference is only discovered after a twelve-minute
// review has already run. These pin the read-only preflight -- including that
// it stays read-only, and that it never claims to know something it does not.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// ghAPIReply is what `gh api --include user` prints: the status line and the
// response headers, then the body.
func ghAPIReply(headers ...string) runner.Result {
	out := "HTTP/2.0 200 OK\r\n" + strings.Join(headers, "\r\n")
	if len(headers) > 0 {
		out += "\r\n"
	}
	out += "\r\n{\"login\":\"angelov-todor\"}\n"
	return runner.Result{Stdout: []byte(out)}
}

func scopeFake(res runner.Result) *runner.Fake {
	return &runner.Fake{Replies: []runner.Reply{{Match: "api", Result: res}}}
}

// The check must itself be read-only: it is run by `doctor`, which an
// operator is told to run before ever going live, so it must not be capable
// of writing to a pull request. Asserted on the argv in order.
func TestGhReviewScopeRunsOnlyAReadOnlyApiCall(t *testing.T) {
	f := scopeFake(ghAPIReply("x-oauth-scopes: repo, gist"))

	if _, err := ghReviewScope(context.Background(), f, "gh"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	want := []string{"api", "--include", "user"}
	if !slices.Equal(f.Calls[0].Args, want) {
		t.Errorf("Args = %q, want %q: the preflight must be a GET", f.Calls[0].Args, want)
	}
}

func TestGhReviewScopePassesWithRepoScope(t *testing.T) {
	for _, hdr := range []string{
		"x-oauth-scopes: repo, gist, read:org",
		"X-OAuth-Scopes: read:org, repo",
		"x-oauth-scopes:repo",
		"x-oauth-scopes: public_repo, gist",
	} {
		t.Run(hdr, func(t *testing.T) {
			detail, err := ghReviewScope(context.Background(), scopeFake(ghAPIReply(hdr)), "gh")
			if err != nil {
				t.Fatalf("a token with repo scope must pass: %v", err)
			}
			// Not merely "some detail mentioning x-oauth-scopes": the
			// indeterminate branch says that too, so this must pin that the
			// header was actually read. A case-sensitive header match, for
			// instance, would silently fall through to indeterminate on the
			// canonicalised "X-OAuth-Scopes" spelling.
			if !strings.HasPrefix(detail, "x-oauth-scopes: ") {
				t.Errorf("the detail must list the scopes it found, got %q", detail)
			}
			if strings.Contains(detail, "could not be determined") {
				t.Errorf("the scopes were readable, so nothing may be indeterminate: %q", detail)
			}
		})
	}
}

// The failure this check exists for: a token that can read pull requests but
// not review them.
func TestGhReviewScopeFailsWithoutRepoScope(t *testing.T) {
	_, err := ghReviewScope(context.Background(),
		scopeFake(ghAPIReply("x-oauth-scopes: read:org, gist, workflow")), "gh")
	if err == nil {
		t.Fatal("a token that cannot review must fail the check")
	}
	for _, want := range []string{"repo", "gh auth refresh", "read:org"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q so it is actionable, got %q", want, err)
		}
	}
	// A dry run submits no verdict, so the operator must not be told this
	// blocks one.
	if !strings.Contains(err.Error(), "Dry runs are unaffected") {
		t.Errorf("the error must say dry runs are unaffected, got %q", err)
	}
}

// A fine-grained or GitHub App token sends no x-oauth-scopes header, and its
// permissions cannot be read from a response at all. The check must say so
// rather than invent either verdict: a false FAIL would send the operator
// chasing a scope that does not exist for their token type, and a confident
// PASS would claim something never established.
func TestGhReviewScopeIsHonestWhenTheHeaderIsAbsent(t *testing.T) {
	for _, name := range []string{"absent", "present but empty"} {
		t.Run(name, func(t *testing.T) {
			res := ghAPIReply("x-github-media-type: github.v3")
			if name == "present but empty" {
				res = ghAPIReply("x-oauth-scopes: ")
			}

			detail, err := ghReviewScope(context.Background(), scopeFake(res), "gh")
			if err != nil {
				t.Fatalf("an undeterminable token must not be a failure: %v", err)
			}
			if !strings.Contains(detail, "could not be determined") {
				t.Errorf("the detail must say write access is unknown rather than fine, got %q", detail)
			}
			if strings.Contains(detail, "x-oauth-scopes: repo") {
				t.Errorf("nothing may be claimed about the scopes, got %q", detail)
			}
		})
	}
}

// Per the runner contract a non-zero exit is data, not an error, so it has to
// be checked explicitly. The stdout here decodes as a normal reply on
// purpose, so the exit code is the only thing that can fail the check.
func TestGhReviewScopeReportsNonZeroExit(t *testing.T) {
	res := ghAPIReply("x-oauth-scopes: repo")
	res.ExitCode = 1
	res.Stderr = []byte("  gh: Bad credentials  ")

	if _, err := ghReviewScope(context.Background(), scopeFake(res), "gh"); err == nil {
		t.Fatal("a non-zero gh exit must fail the check")
	} else if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("the error must carry gh's stderr, got %q", err)
	}
}

func TestOauthScopesReportsHeaderPresenceSeparatelyFromItsValue(t *testing.T) {
	if _, ok := oauthScopes([]byte("HTTP/2.0 200 OK\r\ncontent-type: application/json\r\n")); ok {
		t.Error("no x-oauth-scopes header must report absent")
	}
	scopes, ok := oauthScopes([]byte("x-oauth-scopes: \r\n"))
	if !ok {
		t.Error("an empty x-oauth-scopes header is still present")
	}
	if len(scopes) != 0 {
		t.Errorf("scopes = %q, want none", scopes)
	}
	scopes, ok = oauthScopes([]byte("x-oauth-scopes: repo, gist\r\n"))
	if !ok || !slices.Equal(scopes, []string{"repo", "gist"}) {
		t.Errorf("scopes = %q (ok=%v), want [repo gist]", scopes, ok)
	}
}
