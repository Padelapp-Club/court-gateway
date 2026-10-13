package driver

import (
	"context"
	"testing"
)

// The shared read of a round runs under the round's context: a club whose
// own time limit is already over when it happens to ask first does not
// fail the read for every other club of the round.
func TestSharedReadUsesTheRoundContext(t *testing.T) {
	round := WithEvaluation(context.Background())
	club, cancel := context.WithCancel(round)
	cancel()
	reads := 0
	read := func(ctx context.Context) (int, error) { reads++; return 7, ctx.Err() }
	if v, err := Shared(club, read); v != 7 || err != nil {
		t.Fatalf("first club: %d %v", v, err)
	}
	if v, err := Shared(round, read); v != 7 || err != nil || reads != 1 {
		t.Fatalf("second club: %d %v, %d reads", v, err, reads)
	}
	if WithEvaluation(club) != club {
		t.Fatal("a nested evaluation started a new round")
	}
}
