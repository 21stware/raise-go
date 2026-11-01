package appkit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// A deal event of an app that runs in a process of its own (ADR-0130 P11 S1). Its subscribers are registered with the platform, not
// with the app, so the app cannot queue their rows: it queues ONE row of kind OutboxDealHandover in its own outbox, in the business
// transaction (so it commits with the sale or not at all), and the row is delivered — at once after the commit (DeliverNow), else by
// the worker — to the platform's POST /app/v1/deals (Deps.DealsAPI, Client.Deals). The platform queues one row per subscriber in its
// own outbox, idempotent by the event's key, so a handover repeated after a crash queues nothing twice.
//
// Outcomes of one delivery of a handover (the platform's firstparty outbox has the same):
//   - taken → done, result = "platform";
//   - refused (invalid, conflict, forbidden, a 4xx) → failed at once;
//   - any other error → retried with the outbox's backoff; after OutboxMaxAttempts tries failed;
//   - no DealsAPI wired → stays pending, looked at again in an hour.

// OutboxDealHandover is the kind of a deal event an app hands to the platform.
const OutboxDealHandover = "deal.handover"

// DealHandoverKey is the outbox key of an event handed to the platform.
func DealHandoverKey(eventKey string) string { return "deal:" + eventKey + "|platform" }

// Deals hands one deal event to the platform (POST /app/v1/deals; Deps.DealsAPI): the platform queues it for the subscribers.
func (cl *Client) Deals(ctx context.Context, c Caller, ev DealEvent) error {
	return cl.call(ctx, c, "POST", "/app/v1/deals", nil, ev, nil)
}

// dealPayload is a queued deal event (the platform's firstparty dealPayload: subscriber "platform" for a handover).
type dealPayload struct {
	Subscriber string    `json:"subscriber"`
	User       string    `json:"user"`
	Name       string    `json:"name,omitempty"`
	Event      DealEvent `json:"event"`
}

// EnqueueDeal queues the event in the business transaction tx (scoped to c.Tenant): one deal.handover row in the outbox of the app's
// schema, delivered to the platform after the commit (DeliverNow with the keys it returns). Queueing the same event twice adds nothing.
// An empty Key is filled in; an invalid event is ErrInvalid and nothing is queued. Without DealsAPI nothing is queued and nothing is
// checked (SellsDeals is false).
func (d Deps) EnqueueDeal(ctx context.Context, tx *sql.Tx, c Caller, ev DealEvent) ([]string, error) {
	if d.DealsAPI == nil {
		return nil, nil
	}
	if ev.Key == "" {
		ev.Key = DealKey(ev.Source.App, ev.Source.Ref, ev.Kind)
	}
	if err := ev.Validate(); err != nil {
		return nil, err
	}
	if c.Tenant == "" {
		return nil, Invalid("a deal event needs its tenant")
	}
	key := DealHandoverKey(ev.Key)
	if err := enqueue(ctx, tx, outboxTable(d.Schema), c.Tenant, ev.Source.App, OutboxDealHandover, key, dealPayload{Subscriber: "platform", User: c.User, Name: c.Name, Event: ev}); err != nil {
		return nil, err
	}
	return []string{key}, nil
}

// SellsDeals reports whether the app's deal events go anywhere (EnqueueDeal queues something): the platform's /app/v1/deals.
func (d Deps) SellsDeals() bool { return d.DealsAPI != nil }

// handleDealHandover hands one queued event to the platform. A refusal (the event is invalid, not the app's) fails the row for good;
// anything else is retried with the outbox's backoff, until OutboxMaxAttempts.
func (o *Outbox) handleDealHandover(ctx context.Context, tenant string, now time.Time, it outboxItem) error {
	var p dealPayload
	if err := json.Unmarshal(it.payload, &p); err != nil {
		return o.mark(ctx, tenant, it, `state = 'failed', last_error = $3, done_at = $4`, "unreadable payload: "+err.Error(), now)
	}
	if o.DealsAPI == nil {
		// not wired in this process (yet): keep the row, as a posting waits for the books
		return o.mark(ctx, tenant, it, `next_at = $3, last_error = $4`, now.Add(outboxNotWired), "the platform's deal API is not wired")
	}
	err := o.DealsAPI(ctx, Caller{Tenant: tenant, User: p.User, Name: p.Name}, p.Event)
	if err == nil {
		return o.mark(ctx, tenant, it, `state = 'done', last_error = '', result = $3, done_at = $4`, "platform", now)
	}
	attempts := it.attempts + 1
	if refused(err) || attempts >= OutboxMaxAttempts {
		o.log().Warn("appkit: the platform refused a deal event; kept as failed", "tenant", tenant, "key", it.key, "attempts", attempts, "err", err)
		return o.mark(ctx, tenant, it, `state = 'failed', attempts = $3, last_error = $4, done_at = $5`, attempts, clipErr(err), now)
	}
	return o.mark(ctx, tenant, it, `attempts = $3, next_at = $4, last_error = $5`, attempts, now.Add(outboxPause(attempts)), clipErr(err))
}
