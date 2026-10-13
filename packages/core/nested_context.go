package core

import (
	"context"
	"time"
)

type nestedKey struct{}
type nestedOrigin struct{}
type nestedTimeout struct{}
type toolProgressObserver struct{}

// WithToolProgressObserver lets a durable host commit the current invocation's
// progress before publishing it. Descendants can replace the observer.
func WithToolProgressObserver(ctx context.Context, observer func(AgentEvent)) context.Context {
	return context.WithValue(ctx, toolProgressObserver{}, observer)
}

// WithNestedToolKey assigns a replay-stable key to the next nested call.
// Hosts that do not implement durable nested execution ignore the key.
func WithNestedToolKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, nestedKey{}, key)
}
func NestedToolKey(ctx context.Context) string {
	key, _ := ctx.Value(nestedKey{}).(string)
	return key
}

// WithNestedToolOrigin identifies the host callback that issued a nested call.
func WithNestedToolOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, nestedOrigin{}, origin)
}
func NestedToolOrigin(ctx context.Context) string {
	origin, _ := ctx.Value(nestedOrigin{}).(string)
	return origin
}

// WithNestedToolTimeout bounds execution, not durable approval waits.
func WithNestedToolTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, nestedTimeout{}, timeout)
}
func NestedToolTimeout(ctx context.Context) time.Duration {
	timeout, _ := ctx.Value(nestedTimeout{}).(time.Duration)
	return timeout
}
