// Package chat reads the team's Google Chat space by driving the google-chat
// skill's chat.py, which already holds the OAuth token in the Windows
// Credential Locker.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/angelov-todor/firstpass/internal/runner"
)

// Message is one Google Chat message.
type Message struct {
	Name       string    `json:"name"`
	Text       string    `json:"text"`
	CreateTime time.Time `json:"createTime"`
}

type listResponse struct {
	Messages []Message     `json:"messages"`
	Error    *apiErrorBody `json:"error"`
}

// spaceEntry is one element of the bare JSON array chat.py's list-spaces
// prints on success. Only the fields HasNamedRooms needs are modeled; the
// real payload carries several more (type, spaceType, createTime, ...).
type spaceEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// errorResponse is the JSON object chat.py prints instead of the array when
// list-spaces fails.
type errorResponse struct {
	Error *apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// APIError is a Google Chat API error surfaced by chat.py.
type APIError struct {
	Code    int
	Status  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("chat api %d %s: %s", e.Code, e.Status, e.Message)
}

// Fatal reports whether retrying could ever help. A missing OAuth scope or a
// revoked grant cannot be fixed by waiting, so the daemon should stop rather
// than log the same failure every five minutes forever.
func (e *APIError) Fatal() bool {
	if e.Status == "PERMISSION_DENIED" || e.Status == "UNAUTHENTICATED" {
		return true
	}
	return strings.Contains(e.Message, "ACCESS_TOKEN_SCOPE_INSUFFICIENT")
}

// Client drives chat.py for one space.
type Client struct {
	r      runner.Runner
	python string
	script string
	space  string
}

func New(r runner.Runner, python, script, space string) *Client {
	return &Client{r: r, python: python, script: script, space: space}
}

// Fetch returns messages newest-first, stopping before sinceName, and reports
// whether sinceName was actually inside the fetched window.
//
// A sinceName that is not in the window returns everything, which is the safe
// direction: it re-offers messages whose PRs a later stage will filter out,
// rather than dropping new ones. But the caller cannot tell that from "all of
// these are new" unless it is told, and if it advances its watermark anyway,
// every message between the old watermark and the oldest one fetched is
// skipped permanently and silently. foundSince is that signal. An empty
// sinceName is a cold start or a backfill, not a gap, so it reports true.
//
// The ordering of the payload is checked rather than trusted: chat.py lives
// in another repository, and its newest-first default is the only reason the
// walk above works at all.
func (c *Client) Fetch(ctx context.Context, sinceName string, limit int) ([]Message, bool, error) {
	res, err := c.r.Run(ctx, "", c.python, c.script,
		"get-messages", c.space, "--limit", strconv.Itoa(limit))
	if err != nil {
		return nil, false, fmt.Errorf("run chat.py get-messages: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, false, fmt.Errorf("chat.py get-messages exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var lr listResponse
	if err := json.Unmarshal(StripLeadingNoise(res.Stdout), &lr); err != nil {
		return nil, false, fmt.Errorf("decode chat.py output: %w", err)
	}
	if lr.Error != nil {
		return nil, false, &APIError{Code: lr.Error.Code, Status: lr.Error.Status, Message: lr.Error.Message}
	}

	if n := len(lr.Messages); n > 1 && lr.Messages[0].CreateTime.Before(lr.Messages[n-1].CreateTime) {
		return nil, false, fmt.Errorf(
			"chat.py get-messages returned messages oldest-first (%s before %s): the script's ordering "+
				"changed, so the watermark logic cannot be trusted; pass an explicit newest-first order "+
				"or fix chat.py before sweeping again",
			lr.Messages[0].CreateTime.Format(time.RFC3339),
			lr.Messages[n-1].CreateTime.Format(time.RFC3339))
	}

	if sinceName == "" {
		return lr.Messages, true, nil
	}
	for i, m := range lr.Messages {
		if m.Name == sinceName {
			return lr.Messages[:i], true, nil
		}
	}
	return lr.Messages, false, nil
}

// HasNamedRooms reports whether the authenticated account can see any named
// space. Two Google accounts exist on this machine, and the personal one
// lists none; without this check, sweeping the wrong account looks exactly
// like "nobody posted a PR". A space chat.py returns with no name at all
// (the wrong-account case) surfaces as an absent "displayName" key, not a
// null, so an unnamed space and a missing field must be treated the same.
//
// On success chat.py's list-spaces prints a bare JSON array, unlike
// get-messages' object-wrapped payload. On failure it still prints the usual
// {"error": {...}} object, so the two shapes are told apart by the first
// non-noise byte before either is decoded.
func (c *Client) HasNamedRooms(ctx context.Context) (bool, error) {
	res, err := c.r.Run(ctx, "", c.python, c.script, "list-spaces")
	if err != nil {
		return false, fmt.Errorf("run chat.py list-spaces: %w", err)
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("chat.py list-spaces exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	body := StripLeadingNoise(res.Stdout)
	switch {
	case len(body) > 0 && body[0] == '[':
		var spaces []spaceEntry
		if err := json.Unmarshal(body, &spaces); err != nil {
			return false, fmt.Errorf("decode chat.py output: %w", err)
		}
		for _, s := range spaces {
			if s.DisplayName != "" {
				return true, nil
			}
		}
		return false, nil
	case len(body) > 0 && body[0] == '{':
		var er errorResponse
		if err := json.Unmarshal(body, &er); err != nil {
			return false, fmt.Errorf("decode chat.py output: %w", err)
		}
		if er.Error == nil {
			return false, fmt.Errorf("decode chat.py output: object payload without an error")
		}
		return false, &APIError{Code: er.Error.Code, Status: er.Error.Status, Message: er.Error.Message}
	default:
		return false, fmt.Errorf("decode chat.py output: empty or unrecognized payload")
	}
}

// StripLeadingNoise drops anything chat.py prints before its JSON payload. It
// writes "Access token expired, refreshing..." and "Token refreshed
// successfully." to stdout ahead of the response, which makes a naive decode
// fail even though the call succeeded.
func StripLeadingNoise(b []byte) []byte {
	for i, ch := range b {
		if ch == '{' || ch == '[' {
			return b[i:]
		}
	}
	return b
}
