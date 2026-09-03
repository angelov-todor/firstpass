package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelov-todor/firstpass/internal/runner"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func clientWith(stdout []byte, match string) (*Client, *runner.Fake) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: match, Result: runner.Result{Stdout: stdout}},
	}}
	return New(f, "python", "chat.py", "spaces/A"), f
}

func TestStripLeadingNoise(t *testing.T) {
	in := []byte("Access token expired, refreshing...\nToken refreshed successfully.\n{\"messages\":[]}")
	if got := string(StripLeadingNoise(in)); got != `{"messages":[]}` {
		t.Errorf("StripLeadingNoise() = %q", got)
	}
}

func TestStripLeadingNoiseLeavesCleanJSONAlone(t *testing.T) {
	in := []byte(`{"messages":[]}`)
	if got := string(StripLeadingNoise(in)); got != `{"messages":[]}` {
		t.Errorf("StripLeadingNoise() = %q", got)
	}
}

func TestFetchParsesNoisyOutput(t *testing.T) {
	c, f := clientWith(fixture(t, "messages_noisy.json"), "get-messages")

	msgs, foundSince, err := c.Fetch(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("the token-refresh preamble must not break decoding: %v", err)
	}
	if !foundSince {
		t.Error("an empty sinceName is a cold start or a backfill, not a gap")
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Name != "spaces/A/messages/m3" {
		t.Errorf("messages must stay newest-first, got %q", msgs[0].Name)
	}
	if msgs[0].CreateTime.IsZero() {
		t.Error("createTime must decode")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("Calls = %+v", f.Calls)
	}
	line := f.Calls[0].String()
	for _, want := range []string{"chat.py", "get-messages", "spaces/A", "--limit", "50"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q missing %q", line, want)
		}
	}
}

func TestFetchStopsAtWatermark(t *testing.T) {
	c, _ := clientWith(fixture(t, "messages_noisy.json"), "get-messages")

	msgs, foundSince, err := c.Fetch(context.Background(), "spaces/A/messages/m2", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Name != "spaces/A/messages/m3" {
		t.Fatalf("the watermark must exclude itself and everything older, got %+v", msgs)
	}
	if !foundSince {
		t.Error("foundSince must be true when the watermark is inside the window")
	}
}

func TestFetchWithUnknownWatermarkReturnsEverything(t *testing.T) {
	c, _ := clientWith(fixture(t, "messages_noisy.json"), "get-messages")

	msgs, foundSince, err := c.Fetch(context.Background(), "spaces/A/messages/gone", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("a watermark that has scrolled out of the window must not drop messages, got %d", len(msgs))
	}
	// I1: returning everything is the safe direction, but the caller has to
	// know it happened -- otherwise it cannot tell this from "all of these are
	// new", advances the watermark, and skips every message between the old
	// watermark and the oldest one fetched, permanently and silently.
	if foundSince {
		t.Error("foundSince must be false when the watermark is not in the window: that gap must be reportable")
	}
}

func TestFetchSurfacesScopeErrorAsFatal(t *testing.T) {
	c, _ := clientWith(fixture(t, "error_scope.json"), "get-messages")

	_, _, err := c.Fetch(context.Background(), "", 50)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if !apiErr.Fatal() {
		t.Error("an insufficient scope must be fatal; retrying can never fix it")
	}
}

func TestFetchReportsNonZeroExit(t *testing.T) {
	f := &runner.Fake{Replies: []runner.Reply{
		{Match: "get-messages", Result: runner.Result{ExitCode: 1, Stderr: []byte("boom")}},
	}}
	c := New(f, "python", "chat.py", "spaces/A")

	if _, _, err := c.Fetch(context.Background(), "", 50); err == nil {
		t.Error("a non-zero exit from chat.py must be an error")
	}
}

func TestHasNamedRoomsDetectsWrongAccount(t *testing.T) {
	// Covers both real-world shapes of "unnamed": a displayName key that is
	// simply absent, and one explicitly set to null.
	c, _ := clientWith(fixture(t, "spaces_unnamed.json"), "list-spaces")

	ok, err := c.HasNamedRooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("an account listing only unnamed spaces is the wrong account and must report false")
	}
}

func TestHasNamedRoomsAcceptsRealAccount(t *testing.T) {
	c, _ := clientWith(fixture(t, "spaces_named.json"), "list-spaces")

	ok, err := c.HasNamedRooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("an account that can see a named room must report true")
	}
}

func TestHasNamedRoomsSurfacesScopeErrorAsFatal(t *testing.T) {
	c, _ := clientWith(fixture(t, "error_scope.json"), "list-spaces")

	_, err := c.HasNamedRooms(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if !apiErr.Fatal() {
		t.Error("an insufficient scope must be fatal; retrying can never fix it")
	}
}

// I6: the whole watermark walk assumes newest-first ordering, which today
// holds only because chat.py's get_messages defaults to "createTime desc" --
// a default in a script that lives in another repository. If it flipped,
// Fetch would return the messages *older* than the watermark, Sweep would set
// the watermark to the oldest of them, and from then on the slice would
// always be empty: "0 messages scanned" forever, indistinguishable from a
// quiet week. Refuse to return messages rather than mis-slice them.
func TestFetchRejectsOldestFirstOrdering(t *testing.T) {
	c, _ := clientWith(fixture(t, "messages_oldest_first.json"), "get-messages")

	msgs, _, err := c.Fetch(context.Background(), "", 50)
	if err == nil {
		t.Fatalf("oldest-first output must be an error, got %d messages", len(msgs))
	}
	if !strings.Contains(err.Error(), "ordering") {
		t.Errorf("the error must name the ordering as the problem, got %v", err)
	}
	if msgs != nil {
		t.Errorf("no messages may be returned alongside the ordering error, got %+v", msgs)
	}
}

func TestFetchAcceptsASingleMessage(t *testing.T) {
	body := []byte(`{"messages":[{"name":"spaces/A/messages/m1","text":"hi","createTime":"2026-09-03T07:00:00Z"}]}`)
	c, _ := clientWith(body, "get-messages")

	msgs, _, err := c.Fetch(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("one message cannot be out of order: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("got %d messages, want 1", len(msgs))
	}
}
