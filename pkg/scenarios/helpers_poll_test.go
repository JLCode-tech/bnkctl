package scenarios_test

import (
	"context"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

func TestPollMarkers_SucceedsFirstTry(t *testing.T) {
	calls := 0
	ok, detail := scenarios.PollMarkers(context.Background(), 50*time.Millisecond, 5*time.Millisecond, func() (bool, string) {
		calls++
		return true, "ok-first"
	})
	if !ok {
		t.Errorf("expected ok=true, got false")
	}
	if detail != "ok-first" {
		t.Errorf("unexpected detail %q", detail)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call, got %d", calls)
	}
}

func TestPollMarkers_SucceedsOnLaterTry(t *testing.T) {
	calls := 0
	ok, detail := scenarios.PollMarkers(context.Background(), 50*time.Millisecond, 5*time.Millisecond, func() (bool, string) {
		calls++
		if calls >= 3 {
			return true, "ok-later"
		}
		return false, "not-yet"
	})
	if !ok {
		t.Errorf("expected ok=true after multiple attempts, got false")
	}
	if detail != "ok-later" {
		t.Errorf("unexpected detail %q", detail)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 calls, got %d", calls)
	}
}

func TestPollMarkers_GivesUpAfterMaxWait(t *testing.T) {
	calls := 0
	ok, detail := scenarios.PollMarkers(context.Background(), 50*time.Millisecond, 5*time.Millisecond, func() (bool, string) {
		calls++
		return false, "always-fails"
	})
	if ok {
		t.Errorf("expected ok=false after maxWait, got true")
	}
	if detail != "always-fails" {
		t.Errorf("unexpected detail %q", detail)
	}
	if calls < 2 {
		t.Errorf("expected multiple calls (at least 2), got %d", calls)
	}
}

func TestPollMarkers_HonorsCtxCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	ok, _ := scenarios.PollMarkers(ctx, 200*time.Millisecond, 5*time.Millisecond, func() (bool, string) {
		calls++
		return false, "blocked"
	})
	if ok {
		t.Errorf("expected ok=false on cancellation")
	}
	// Should have been cancelled well before maxWait (200ms); calls should be few.
	if calls == 0 {
		t.Errorf("expected at least 1 call before cancel")
	}
}
