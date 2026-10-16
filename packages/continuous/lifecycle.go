package continuous

import (
	"context"
	"errors"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// storeLifetime derives an execution context that is cancelled when the
// store reports terminal failure, with that failure as the cause. Stores
// without lifecycle observation return ctx unchanged. stop releases the
// watcher and must be called after execution has been joined.
func storeLifetime(ctx context.Context, store storage.Store) (context.Context, func()) {
	lc, ok := store.(storage.StoreLifecycle)
	if !ok {
		return ctx, func() {}
	}
	exec, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		select {
		case <-lc.Done():
			if err := lc.Err(); err != nil {
				cancel(err)
			}
		case <-done:
		case <-exec.Done():
		}
	}()
	return exec, func() {
		close(done)
		cancel(context.Canceled)
	}
}

// storeFailure returns the store's terminal cause, if any. Callers prefer
// it over a cancellation it caused, so a lost database session is reported
// as such instead of as a clean shutdown.
func storeFailure(store storage.Store) error {
	if lc, ok := store.(storage.StoreLifecycle); ok {
		return lc.Err()
	}
	return nil
}

// preferStoreFailure replaces err with the store's terminal cause when err
// is a cancellation, or joins the cause otherwise.
func preferStoreFailure(store storage.Store, err error) error {
	cause := storeFailure(store)
	if cause == nil {
		return err
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, storage.ErrClosed) {
		return cause
	}
	if errors.Is(err, cause) {
		return err
	}
	return errors.Join(cause, err)
}
