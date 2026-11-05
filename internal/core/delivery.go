package core

import (
	"context"
	"time"
)

// DeliveryFailure distinguishes a confirmed rejection from a write whose
// outcome is unknown. A retryable rejection may be scheduled again; an
// uncertain write always requires explicit recovery, even when it has a UUID.
type DeliveryFailure interface {
	error
	DeliveryRetryable() bool
	DeliveryUncertain() bool
	DeliveryRetryAfter() time.Duration
}

type deliveryContextKey uint8

const (
	deliveryIDKey deliveryContextKey = iota
	explicitRetryKey
	terminalCardKey
)

// WithDeliveryID binds all transport steps to one durable outbox/render ID.
// Callers keep the payload immutable until its message ID is confirmed, then
// use Update for subsequent snapshots. A new logical send needs a new ID.
func WithDeliveryID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, deliveryIDKey, id)
}

func DeliveryID(ctx context.Context) string {
	id, _ := ctx.Value(deliveryIDKey).(string)
	return id
}

// WithExplicitRetry may only be used after the caller has authorized recovery
// of an uncertain write. It is not an automatic retry policy.
func WithExplicitRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, explicitRetryKey, true)
}

func IsExplicitRetry(ctx context.Context) bool {
	explicit, _ := ctx.Value(explicitRetryKey).(bool)
	return explicit
}

// WithTerminalCard marks an acknowledged final reply, not a temporary pause in
// streaming. Disabling streaming while waiting for approval is not terminal.
func WithTerminalCard(ctx context.Context) context.Context {
	return context.WithValue(ctx, terminalCardKey, true)
}

func IsTerminalCard(ctx context.Context) bool {
	terminal, _ := ctx.Value(terminalCardKey).(bool)
	return terminal
}

// CardRetainer is optional so local and test transports need no persistence.
// Protected references include message IDs and logical delivery IDs which are
// still active or referenced by an unfinished outbox item.
type CardRetainer interface {
	Retain(context.Context, time.Time, []string) (int64, error)
}

// CardRecovery closes acknowledged cards left open by a crashed process. It is
// a startup-only operation before active renders exist, cannot create messages,
// and leaves unfinished outbox items in control of their own final delivery.
type CardRecovery interface {
	RecoverCards(context.Context) (int64, error)
}
