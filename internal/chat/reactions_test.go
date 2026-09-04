package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func TestAddReactionReturnsTheReactionName(t *testing.T) {
	// The token-refresh preamble is on stdout here too: chat.py writes it
	// ahead of every response, whatever the subcommand.
	body := []byte("Access token expired, refreshing...\nToken refreshed successfully.\n" +
		`{"name":"spaces/A/messages/m1/reactions/r1","emoji":{"unicode":"\ud83d\udc40"}}`)
	c, f := clientWith(body, "add-reaction")

	name, err := c.AddReaction(context.Background(), "spaces/A/messages/m1", "👀")
	if err != nil {
		t.Fatalf("AddReaction: %v", err)
	}
	if name != "spaces/A/messages/m1/reactions/r1" {
		t.Errorf("AddReaction() = %q; the reaction name is the only handle for removing it later", name)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	line := f.Calls[0].String()
	for _, want := range []string{"chat.py", "add-reaction", "spaces/A/messages/m1", "👀"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

// A reaction whose name did not come back cannot be removed, so reporting
// success would leave a 👀 on the message forever with nothing able to find
// it. Better to fail: the caller only logs a reaction failure anyway.
func TestAddReactionRejectsAResponseWithoutAName(t *testing.T) {
	c, _ := clientWith([]byte(`{"emoji":{"unicode":"\ud83d\udc40"}}`), "add-reaction")

	if name, err := c.AddReaction(context.Background(), "spaces/A/messages/m1", "👀"); err == nil {
		t.Errorf("a response carrying no reaction name must be an error, got %q", name)
	}
}

func TestAddReactionReportsNonZeroExit(t *testing.T) {
	// The payload is a perfectly good Reaction resource, complete with a name.
	// Only the exit status says the call failed, so this fails if and only if
	// the exit status is actually checked -- with an empty or malformed stdout
	// the decode would error on its own and the test would pass without the
	// check being there at all.
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "add-reaction", Result: runner.Result{
			ExitCode: 1,
			Stdout:   []byte(`{"name":"spaces/A/messages/m1/reactions/r1"}`),
			Stderr:   []byte("boom"),
		}},
	}}
	c := New(f, "python", "chat.py", "spaces/A")

	name, err := c.AddReaction(context.Background(), "spaces/A/messages/m1", "👀")
	if err == nil {
		t.Errorf("a non-zero exit from chat.py must be an error, got name %q", name)
	}
	if name != "" {
		t.Errorf("no reaction name may be returned alongside a failure, got %q", name)
	}
}

func TestAddReactionSurfacesScopeErrorAsFatal(t *testing.T) {
	c, _ := clientWith(fixture(t, "error_scope.json"), "add-reaction")

	_, err := c.AddReaction(context.Background(), "spaces/A/messages/m1", "👀")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if !apiErr.Fatal() {
		t.Error("an insufficient scope must be fatal; retrying can never fix it")
	}
}

func TestRemoveReactionDrivesTheReactionName(t *testing.T) {
	c, f := clientWith([]byte(`{"deleted":"spaces/A/messages/m1/reactions/r1"}`), "remove-reaction")

	if err := c.RemoveReaction(context.Background(), "spaces/A/messages/m1/reactions/r1"); err != nil {
		t.Fatalf("RemoveReaction: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	line := f.Calls[0].String()
	for _, want := range []string{"chat.py", "remove-reaction", "spaces/A/messages/m1/reactions/r1"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

// A DELETE that succeeded with no body at all. chat.py already turns its own
// "Invalid JSON response" sentinel into a success, but the Go side must not
// depend on that being the only script it ever drives: an empty payload from
// a zero exit is a success here too. Failing it would make the pipeline log a
// reaction error on every removal that actually worked.
func TestRemoveReactionAcceptsAnEmptyBody(t *testing.T) {
	for _, body := range []string{"", "\n", "Token refreshed successfully.\n"} {
		c, _ := clientWith([]byte(body), "remove-reaction")
		if err := c.RemoveReaction(context.Background(), "spaces/A/messages/m1/reactions/r1"); err != nil {
			t.Errorf("an empty body on a zero exit is a successful delete, got %v (body %q)", err, body)
		}
	}
}

func TestRemoveReactionReportsNonZeroExit(t *testing.T) {
	// Three stdout shapes, all with a non-zero exit. The first is the one that
	// makes this test worth writing: it decodes cleanly and carries no error
	// object, so nothing but the exit status distinguishes it from a
	// successful delete. The other two are what chat.py really prints -- its
	// api_request wraps a rejected request as a *string* error, which is not
	// the nested object shape -- and they must not read as a delete either.
	for _, stdout := range []string{
		`{"deleted":"spaces/A/messages/m1/reactions/r1"}`,
		`{"error": "HTTP 404: reaction not found"}`,
		"",
	} {
		f := &runner.Fake{Replies: []runner.Reply{
			{Match: "remove-reaction", Result: runner.Result{
				ExitCode: 1,
				Stdout:   []byte(stdout),
				Stderr:   []byte("boom"),
			}},
		}}
		c := New(f, "python", "chat.py", "spaces/A")

		if err := c.RemoveReaction(context.Background(), "spaces/A/messages/m1/reactions/r1"); err == nil {
			t.Errorf("a non-zero exit must be an error, not a delete (stdout %q)", stdout)
		}
	}
}

func TestRemoveReactionSurfacesAPIError(t *testing.T) {
	c, _ := clientWith(fixture(t, "error_scope.json"), "remove-reaction")

	err := c.RemoveReaction(context.Background(), "spaces/A/messages/m1/reactions/r1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
}

func TestReactionRunFailureIsAnError(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "add-reaction", Err: errors.New("context deadline exceeded")},
		{Match: "remove-reaction", Err: errors.New("context deadline exceeded")},
	}}
	c := New(f, "python", "chat.py", "spaces/A")

	if _, err := c.AddReaction(context.Background(), "spaces/A/messages/m1", "👀"); err == nil {
		t.Error("a failure to run chat.py at all must be an error")
	}
	if err := c.RemoveReaction(context.Background(), "spaces/A/messages/m1/reactions/r1"); err == nil {
		t.Error("a failure to run chat.py at all must be an error")
	}
}
