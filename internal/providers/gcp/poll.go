package gcp

import (
	"context"
	"fmt"
	"time"
)

// PollUntil polls checkFunc every interval until it returns done=true, err!=nil,
// or the timeout/context deadline expires.
// Note: It executes a leading tick (checks checkFunc immediately before sleeping).
// If the timeout expires or ctx is cancelled, it returns an error that wraps
// context.DeadlineExceeded / ctx.Err() so errors.Is(err, context.DeadlineExceeded) is true.
func PollUntil(ctx context.Context, interval, timeout time.Duration, checkFunc func(ctx context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		done, err := checkFunc(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("polling timed out after %v: %w", timeout, ctx.Err())
		case <-time.After(interval):
		}
	}
}
