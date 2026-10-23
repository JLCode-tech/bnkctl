package gcp

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPollUntil_ImmediateSuccess(t *testing.T) {
	ctx := context.Background()
	calls := 0
	start := time.Now()

	err := PollUntil(ctx, 1*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		calls++
		return true, nil
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if time.Since(start) >= 500*time.Millisecond {
		t.Fatalf("expected immediate success without sleeping, took %v", time.Since(start))
	}
}

func TestPollUntil_EventualSuccess(t *testing.T) {
	ctx := context.Background()
	calls := 0

	err := PollUntil(ctx, 5*time.Millisecond, 1*time.Second, func(ctx context.Context) (bool, error) {
		calls++
		if calls == 3 {
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestPollUntil_Error(t *testing.T) {
	ctx := context.Background()
	expectedErr := errors.New("check func error")

	err := PollUntil(ctx, 5*time.Millisecond, 1*time.Second, func(ctx context.Context) (bool, error) {
		return false, expectedErr
	})

	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
}

func TestPollUntil_Timeout(t *testing.T) {
	ctx := context.Background()

	err := PollUntil(ctx, 5*time.Millisecond, 20*time.Millisecond, func(ctx context.Context) (bool, error) {
		return false, nil
	})

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected errors.Is(err, context.DeadlineExceeded) to be true, got %v", err)
	}
}
