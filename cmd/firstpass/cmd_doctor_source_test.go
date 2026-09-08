package main

import (
	"context"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/config"
	"github.com/angelov-todor/firstpass/internal/ghpr"
	"github.com/angelov-todor/firstpass/internal/runner"
)

// Two pull requests, one of them a bot's, in one repository.
const doctorSearchJSON = `{"total_count":2,"items":[
{"number":1,"repository_url":"https://api.github.com/repos/Example-Org/aex-a",
 "updated_at":"2026-09-07T08:30:00Z","user":{"login":"colleague","type":"User"},"pull_request":{}},
{"number":2,"repository_url":"https://api.github.com/repos/Example-Org/aex-a",
 "updated_at":"2026-09-07T08:31:00Z","user":{"login":"dependabot[bot]","type":"Bot"},"pull_request":{}}
]}`

func doctorSource() config.Source {
	return config.Source{Type: "github", Owner: "Example-Org", ExcludeBots: true}
}

// The counts are the whole point: a source returning nothing looks exactly
// like a quiet week, so doctor has to say what came back rather than only that
// the call succeeded.
func TestCheckSourceReportsWhatCameBack(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "search/issues", Result: runner.Result{Stdout: []byte(doctorSearchJSON)}},
	}}
	detail, err := checkSource(context.Background(), ghpr.New(f, "gh"), doctorSource(), "angelov-todor")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 scanned", "2 matched", "1 after bots"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail must contain %q: %s", want, detail)
		}
	}
}

// A full page is a failure and not a note. It is the one outcome the operator
// has to act on: pull requests exist that firstpass will never see until the
// noisiest authors are excluded, and reporting it as a pass would leave that
// invisible.
func TestCheckSourceFailsOnAFullPage(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "search/issues", Result: runner.Result{Stdout: []byte(doctorSearchJSON)}},
	}}
	src := doctorSource()
	src.Limit = 2 // two items against a limit of two is a full page

	detail, err := checkSource(context.Background(), ghpr.New(f, "gh"), src, "angelov-todor")
	if err == nil {
		t.Fatal("a full page must be reported as a failure")
	}
	if !strings.Contains(err.Error(), "exclude_authors") {
		t.Errorf("the error must say what to do about it: %v", err)
	}
	// The counts still come back, because they are what the operator needs in
	// order to act on the failure.
	if !strings.Contains(detail, "2 scanned") {
		t.Errorf("the detail must survive the failure: %q", detail)
	}
}

func TestCheckSourceReportsAFailedQuery(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "search/issues", Result: runner.Result{
			ExitCode: 1, Stderr: []byte("HTTP 422: Validation Failed"),
		}},
	}}
	if _, err := checkSource(context.Background(), ghpr.New(f, "gh"),
		doctorSource(), "angelov-todor"); err == nil {
		t.Fatal("a failed query must be a failed check")
	} else if !strings.Contains(err.Error(), "422") {
		t.Errorf("the error must carry what GitHub said: %v", err)
	}
}
