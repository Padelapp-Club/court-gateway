package core

import (
	"context"
	"testing"
	"time"
)

func TestKeyedLocks(t *testing.T) {
	k := keyedLocks{m: map[string]*keyedLock{}}
	unlock, err := k.lock(context.Background(), "room-1")
	if err != nil {
		t.Fatal(err)
	}
	// another key is independent
	other, err := k.lock(context.Background(), "room-2")
	if err != nil {
		t.Fatal(err)
	}
	other()
	// the same key waits, and gives up with its context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := k.lock(ctx, "room-1"); err == nil {
		t.Fatal("a held lock was taken twice")
	}
	unlock()
	again, err := k.lock(context.Background(), "room-1")
	if err != nil {
		t.Fatal(err)
	}
	again()
	if len(k.m) != 0 {
		t.Fatalf("locks left behind: %d", len(k.m))
	}
}
