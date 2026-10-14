package driver

import (
	"context"
	"testing"
)

// A club's context names its round's evaluation, whose context is the
// round's: a club whose own time limit is already over when it happens to
// ask first does not fail the shared read for every other club.
func TestEvaluationIsTheRoundsContext(t *testing.T) {
	round := WithEvaluation(context.Background())
	club, cancel := context.WithCancel(round)
	cancel()
	if ev := Evaluation(club); ev != round || ev.Err() != nil {
		t.Fatalf("the club's evaluation: %v", ev)
	}
	if WithEvaluation(club) != club {
		t.Fatal("a nested evaluation started a new round")
	}
	if Evaluation(context.Background()) != nil {
		t.Fatal("an evaluation outside one")
	}
}
