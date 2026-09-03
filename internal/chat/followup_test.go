package chat

// Finding 9: the ordering assertion false-positived on a missing createTime.

import (
	"context"
	"strings"
	"testing"
)

// A message whose payload carries no createTime decodes to the zero time,
// which is Before everything. Comparing it turned one malformed message into a
// hard failure of every sweep: the error was returned, no messages came back,
// and the daemon reviewed nothing at all until the message scrolled away.
func TestFetchToleratesAMissingCreateTimeOnTheNewestMessage(t *testing.T) {
	body := []byte(`{"messages":[
		{"name":"spaces/A/messages/m3","text":"newest, no timestamp"},
		{"name":"spaces/A/messages/m2","text":"middle","createTime":"2026-09-03T08:00:00Z"},
		{"name":"spaces/A/messages/m1","text":"oldest","createTime":"2026-09-03T07:00:00Z"}]}`)
	c, _ := clientWith(body, "get-messages")

	msgs, _, err := c.Fetch(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("one message without a createTime must not fail the whole sweep: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Name != "spaces/A/messages/m3" {
		t.Errorf("messages must stay newest-first, got %q", msgs[0].Name)
	}
}

func TestFetchToleratesAMissingCreateTimeOnTheOldestMessage(t *testing.T) {
	body := []byte(`{"messages":[
		{"name":"spaces/A/messages/m3","text":"newest","createTime":"2026-09-03T09:00:00Z"},
		{"name":"spaces/A/messages/m1","text":"oldest, no timestamp"}]}`)
	c, _ := clientWith(body, "get-messages")

	if _, _, err := c.Fetch(context.Background(), "", 50); err != nil {
		t.Fatalf("a missing createTime at either end must not fail the sweep: %v", err)
	}
}

// The check's purpose stays intact: a genuine oldest-first payload, with real
// timestamps at both ends, is still refused.
func TestFetchStillRejectsAGenuineOrderingFlip(t *testing.T) {
	body := []byte(`{"messages":[
		{"name":"spaces/A/messages/m1","text":"oldest","createTime":"2026-09-03T07:00:00Z"},
		{"name":"spaces/A/messages/m3","text":"newest","createTime":"2026-09-03T09:00:00Z"}]}`)
	c, _ := clientWith(body, "get-messages")

	msgs, _, err := c.Fetch(context.Background(), "", 50)
	if err == nil {
		t.Fatalf("oldest-first output must still be an error, got %d messages", len(msgs))
	}
	if !strings.Contains(err.Error(), "ordering") {
		t.Errorf("the error must name the ordering as the problem, got %v", err)
	}
}
