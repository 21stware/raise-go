package appkit

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Handlers of the app's own outbox kinds (the platform's firstparty.RegisterOutboxHandler, same shape). The outbox's own kind is
// ledger.post; an app that wants its rows delivered by the same worker (same claim, lease and sweep) registers a handler for its
// kind at start-up and says in the result what became of each delivery. The outbox writes the outcome, so a handler never touches
// the table's state columns.
// OutboxRow is a claimed row as an outside handler sees it.
type OutboxRow struct {
	ID        string
	App       string
	Kind      string
	Key       string
	Payload   json.RawMessage
	Attempts  int // failed deliveries so far
	CreatedAt time.Time
}

// Outcomes of a delivery by an outside handler.
const (
	OutDone    = "done"    // delivered; Result says what it became
	OutSkipped = "skipped" // nothing to do any more (the receiver is gone, the person muted it): not an error
	OutFailed  = "failed"  // refused for good: the row waits in the manual list until a person retries it
	OutRetry   = "retry"   // not now: tried again at RetryAt (zero: the usual backoff); after OutboxMaxAttempts tries it fails
)

// OutboxResult is what became of one delivery.
type OutboxResult struct {
	State   string
	Result  string
	Error   string
	RetryAt time.Time
}

// OutboxHandlerFunc delivers one row of its kind. It must be idempotent by the row's key (a delivery repeated after a crash
// between "sent" and "marked done" must not send twice where the receiver can tell).
type OutboxHandlerFunc func(ctx context.Context, tenant string, now time.Time, row OutboxRow) OutboxResult

var (
	extMu       sync.RWMutex
	extHandlers = map[string]OutboxHandlerFunc{}
)

// RegisterOutboxHandler makes the outbox deliver rows of kind with h (replacing an earlier handler of the same kind).
func RegisterOutboxHandler(kind string, h OutboxHandlerFunc) {
	extMu.Lock()
	extHandlers[kind] = h
	extMu.Unlock()
}

func extHandler(kind string) (OutboxHandlerFunc, bool) {
	extMu.RLock()
	h, ok := extHandlers[kind]
	extMu.RUnlock()
	return h, ok
}

// handleExt delivers a claimed row with an outside handler and writes the outcome it reports.
func (o *Outbox) handleExt(ctx context.Context, tenant string, now time.Time, it outboxItem, h OutboxHandlerFunc) (err error) {
	res := func() (res OutboxResult) {
		defer func() {
			if r := recover(); r != nil {
				res = OutboxResult{State: OutRetry, Error: "the handler panicked"}
			}
		}()
		return h(ctx, tenant, now, OutboxRow{ID: it.id, App: it.app, Kind: it.kind, Key: it.key, Payload: it.payload, Attempts: it.attempts, CreatedAt: it.createdAt})
	}()
	msg := res.Error
	if r := []rune(msg); len(r) > 500 {
		msg = string(r[:500]) + "…"
	}
	switch res.State {
	case OutDone:
		return o.mark(ctx, tenant, it, `state = 'done', result = $3, last_error = '', done_at = $4`, res.Result, now)
	case OutSkipped:
		return o.mark(ctx, tenant, it, `state = 'skipped', last_error = $3, done_at = $4, next_at = $4`, msg, now)
	case OutFailed:
		return o.mark(ctx, tenant, it, `state = 'failed', attempts = $3, last_error = $4, done_at = $5`, it.attempts+1, msg, now)
	}
	attempts := it.attempts + 1
	if attempts >= OutboxMaxAttempts {
		return o.mark(ctx, tenant, it, `state = 'failed', attempts = $3, last_error = $4, done_at = $5`, attempts, msg, now)
	}
	at := res.RetryAt
	if at.IsZero() {
		at = now.Add(outboxPause(attempts))
	}
	return o.mark(ctx, tenant, it, `attempts = $3, next_at = $4, last_error = $5`, attempts, at, msg)
}
