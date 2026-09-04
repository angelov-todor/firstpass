package ghpr

import (
	"context"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/prref"
	"github.com/angelov-todor/firstpass/internal/runner"
)

var ref = prref.PRRef{Owner: "Example-Org", Repo: "aex-balances", Number: 12}

func TestInspectParsesGhJSON(t *testing.T) {
	body := `{"state":"OPEN","isDraft":false,"author":{"login":"stefan-cvetkovic"},"headRefOid":"deadbeef"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(body)}},
	}}

	got, err := New(f, "gh").Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	want := PRInfo{State: "OPEN", IsDraft: false, Author: "stefan-cvetkovic", HeadSHA: "deadbeef"}
	if got != want {
		t.Errorf("Inspect() = %+v, want %+v", got, want)
	}

	line := f.Calls[0].String()
	for _, want := range []string{"pr", "view", "12", "--repo", "Example-Org/aex-balances", "state,isDraft,author,headRefOid"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

func TestInspectParsesDraft(t *testing.T) {
	body := `{"state":"OPEN","isDraft":true,"author":{"login":"x"},"headRefOid":"a"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte(body)}},
	}}

	got, err := New(f, "gh").Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDraft {
		t.Error("IsDraft must decode as true")
	}
}

// Per the runner contract a non-zero exit arrives as data, not an error, so
// Inspect has to check it explicitly.
//
// The stdout here decodes cleanly on purpose. With empty stdout this test
// passed even with the exit check removed, because json.Unmarshal("") fails
// and produces an error of its own -- so it asserted that *some* error came
// back rather than the contract it is named for. A decodable body leaves the
// exit code as the only thing that can produce one.
func TestInspectReportsNonZeroExit(t *testing.T) {
	body := `{"state":"OPEN","isDraft":false,"author":{"login":"stefan-cvetkovic"},"headRefOid":"deadbeef"}`
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{
			ExitCode: 1,
			Stdout:   []byte(body),
			Stderr:   []byte("could not resolve to a PullRequest"),
		}},
	}}

	_, err := New(f, "gh").Inspect(context.Background(), ref)
	if err == nil {
		t.Fatal("a non-zero gh exit must be an error")
	}
	if !strings.Contains(err.Error(), ref.Key()) {
		t.Errorf("the error must name the PR, got %q", err)
	}
}

func TestInspectReportsBadJSON(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "pr view", Result: runner.Result{Stdout: []byte("not json")}},
	}}

	if _, err := New(f, "gh").Inspect(context.Background(), ref); err == nil {
		t.Error("undecodable gh output must be an error")
	}
}
