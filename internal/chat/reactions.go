package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// reactionResponse is the Reaction resource chat.py's add-reaction prints on
// success, or the {"error": {...}} object it prints instead on failure.
type reactionResponse struct {
	Name  string        `json:"name"`
	Error *apiErrorBody `json:"error"`
}

// AddReaction reacts to one message with a single Unicode emoji and returns
// the name of the reaction it created (spaces/X/messages/Y/reactions/Z). That
// name is the only handle for removing the reaction again, so a response
// without one is an error rather than a silent success -- the alternative is a
// reaction nothing can ever find.
//
// Every failure here is the caller's to log and carry on from. A reaction is
// cosmetic: it says a review is under way, and nothing about a review depends
// on it.
func (c *Client) AddReaction(ctx context.Context, messageName, emoji string) (string, error) {
	res, err := c.r.Run(ctx, "", c.python, c.script, "add-reaction", messageName, emoji)
	if err != nil {
		return "", fmt.Errorf("run chat.py add-reaction: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("chat.py add-reaction exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var rr reactionResponse
	if err := json.Unmarshal(StripLeadingNoise(res.Stdout), &rr); err != nil {
		return "", fmt.Errorf("decode chat.py output: %w", err)
	}
	if rr.Error != nil {
		return "", &APIError{Code: rr.Error.Code, Status: rr.Error.Status, Message: rr.Error.Message}
	}
	if rr.Name == "" {
		return "", fmt.Errorf("chat.py add-reaction returned no reaction name, so the reaction " +
			"could never be removed again")
	}
	return rr.Name, nil
}

// RemoveReaction deletes one reaction by its full name.
//
// An empty payload on a zero exit is a success. The Chat API's delete has
// nothing to report, and firstpass's own chat.py already converts the
// "Invalid JSON response" that an empty body provokes in its api_request
// helper into an explicit success -- but treating an empty body as a failure
// here would still make every genuinely successful removal log an error the
// moment the script, or the API, stopped sending a body. A real failure is
// visible without a body: chat.py exits non-zero on one.
func (c *Client) RemoveReaction(ctx context.Context, reactionName string) error {
	res, err := c.r.Run(ctx, "", c.python, c.script, "remove-reaction", reactionName)
	if err != nil {
		return fmt.Errorf("run chat.py remove-reaction: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("chat.py remove-reaction exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	// The first non-noise byte decides whether there is a payload at all, the
	// same way HasNamedRooms tells its two payload shapes apart. Testing for an
	// empty stdout is not enough: StripLeadingNoise returns its input unchanged
	// when it finds no JSON in it, and chat.py prints its token-refresh
	// preamble whether or not a response follows.
	body := StripLeadingNoise(res.Stdout)
	if len(body) == 0 || (body[0] != '{' && body[0] != '[') {
		return nil
	}

	var rr reactionResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		return fmt.Errorf("decode chat.py output: %w", err)
	}
	if rr.Error != nil {
		return &APIError{Code: rr.Error.Code, Status: rr.Error.Status, Message: rr.Error.Message}
	}
	return nil
}
