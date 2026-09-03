package main

import (
	"testing"

	"github.com/angelov-todor/firstpass/internal/pipeline"
)

func TestReviewedCountReflectsTheDecision(t *testing.T) {
	cases := []struct {
		action pipeline.Action
		want   int
	}{
		{pipeline.ActionReview, 1},
		{pipeline.ActionSkip, 0},
		{pipeline.ActionDefer, 0},
		{pipeline.ActionNeedsAttention, 0},
	}
	for _, c := range cases {
		d := pipeline.Decision{Action: c.action}
		if got := reviewedCount(d); got != c.want {
			t.Errorf("reviewedCount(%q) = %d, want %d", c.action, got, c.want)
		}
	}
}
