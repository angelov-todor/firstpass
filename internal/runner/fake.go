package runner

import (
	"context"
	"fmt"
	"strings"
)

// Call records one invocation made against a Fake.
type Call struct {
	Dir  string
	Name string
	Args []string
}

func (c Call) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Reply is a canned response, selected when Match is a substring of the
// command line.
type Reply struct {
	Match  string
	Result Result
	Err    error
	// Times, when positive, limits how often this reply is used; later
	// matching replies then take over.
	//
	// It exists so a test can model the world changing between two identical
	// calls -- which is what verifying an outward effect requires. firstpass
	// asks GitHub for a pull request's feedback before a review and again
	// afterwards to establish whether the review actually posted anything, and
	// with a single canned answer both calls return the same thing, so
	// "nothing was posted" is the only outcome a test could ever reach.
	Times int
}

// Fake is a Runner for tests. It records every call and replies from Replies;
// an unmatched command is an error, so a test can never silently exercise a
// path it did not set up.
type Fake struct {
	Calls   []Call
	Replies []Reply

	used map[int]int
}

func (f *Fake) Run(_ context.Context, dir, name string, args ...string) (Result, error) {
	c := Call{Dir: dir, Name: name, Args: args}
	f.Calls = append(f.Calls, c)

	line := c.String()
	for i, r := range f.Replies {
		if !strings.Contains(line, r.Match) {
			continue
		}
		if r.Times > 0 {
			if f.used == nil {
				f.used = map[int]int{}
			}
			if f.used[i] >= r.Times {
				continue
			}
			f.used[i]++
		}
		return r.Result, r.Err
	}
	return Result{}, fmt.Errorf("fake runner: no reply configured for %q", line)
}
