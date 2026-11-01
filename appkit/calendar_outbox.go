package appkit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An app's calendar projection queued in its own transaction (ADR-0130 §3.2 H7, P11 S3): what the platform's calendar package does
// for an app in its kit (calendar.Upsert / Cancel, then calendar.RegisterRemote hands each row to /app/v1/calendar/projections). The
// row commits with the change that moved the fact or not at all; the outbox delivers it through Deps.CalendarAPI (Client.CalendarPush).
// Each push carries a version taken when it is queued (strictly increasing within the process); the calendar applies only a push newer
// than what it has, so a repeated or late delivery ends with the newest.
//
// Outcomes of one delivery (the platform's calendar outbox handler has the same):
//   - applied → done, result = the calendar item's id, or "stale" when a newer push was applied already;
//   - ErrInvalid (the platform refused the push itself) → failed at once;
//   - any other error → retried with the outbox's backoff; after OutboxMaxAttempts tries failed;
//   - no CalendarAPI wired → stays pending, looked at again in an hour.

// The outbox kinds of a calendar push (the platform's calendar.OutboxUpsert / OutboxCancel).
const (
	OutboxCalendarUpsert = "calendar.event.upsert"
	OutboxCalendarCancel = "calendar.event.cancel"
)

// The privacy of a projection: Detail is shown to everyone who sees the item (normal), to the item's own people (private), or to them
// only (health).
const (
	CalendarNormal  = "normal"
	CalendarPrivate = "private"
	CalendarHealth  = "health"
)

// calendarPush is a queued push (the platform's calendar push JSON).
type calendarPush struct {
	App     string              `json:"app"`
	Ref     string              `json:"ref"`
	Version int64               `json:"version"`
	Item    *CalendarProjection `json:"item,omitempty"` // nil for a cancel
}

var (
	calVerMu   sync.Mutex
	calLastVer int64
)

// calendarVersion is the wall clock in nanoseconds, made strictly increasing within the process.
func calendarVersion() int64 {
	calVerMu.Lock()
	defer calVerMu.Unlock()
	v := time.Now().UnixNano()
	if v <= calLastVer {
		v = calLastVer + 1
	}
	calLastVer = v
	return v
}

func calendarKey(app, ref string, v int64) string {
	return "cal:" + app + "|" + ref + "|" + strconv.FormatInt(v, 10)
}

func checkCalendarRef(app, ref string) error {
	if app == "" {
		return Invalid("calendar: a push needs its app")
	}
	if !strings.HasPrefix(ref, "k3://") || len(ref) > 300 {
		return Invalid("calendar: a projection's ref must be a k3:// reference")
	}
	return nil
}

// CalendarUpsert queues the app's projection p inside the business transaction tx (scoped to tenant) and returns the outbox key (for
// DeliverNow after the commit). app is the calling app's id (the platform files the push under the token's app either way). A
// projection with nobody on it (no owner, no attendees) is on no one's calendar: it is queued as a cancel. The platform checks the
// rest (start and end, privacy, time zone) when it applies the push; a push it refuses fails for good.
func (d Deps) CalendarUpsert(ctx context.Context, tx *sql.Tx, tenant, app string, p CalendarProjection) (string, error) {
	if err := checkCalendarRef(app, p.Ref); err != nil {
		return "", err
	}
	if p.Start.IsZero() || p.End.IsZero() {
		return "", Invalid("calendar: a projection needs its start and end")
	}
	if p.End.Before(p.Start) {
		return "", Invalid("calendar: a projection ends before it starts")
	}
	switch p.Privacy {
	case "", CalendarNormal, CalendarPrivate, CalendarHealth:
	default:
		return "", Invalid("calendar: privacy %q is not normal, private or health", p.Privacy)
	}
	p.Owner = strings.TrimSpace(p.Owner)
	if p.Owner == "" && len(p.Attendees) == 0 {
		return d.CalendarCancel(ctx, tx, tenant, app, p.Ref)
	}
	v := calendarVersion()
	key := calendarKey(app, p.Ref, v)
	return key, enqueue(ctx, tx, outboxTable(d.Schema), tenant, app, OutboxCalendarUpsert, key, calendarPush{App: app, Ref: p.Ref, Version: v, Item: &p})
}

// CalendarCancel queues the cancellation of the app's projection inside tx (the fact was cancelled or deleted). Cancelling what the
// calendar never had is fine.
func (d Deps) CalendarCancel(ctx context.Context, tx *sql.Tx, tenant, app, ref string) (string, error) {
	if err := checkCalendarRef(app, ref); err != nil {
		return "", err
	}
	v := calendarVersion()
	key := calendarKey(app, ref, v)
	return key, enqueue(ctx, tx, outboxTable(d.Schema), tenant, app, OutboxCalendarCancel, key, calendarPush{App: app, Ref: ref, Version: v})
}

// handleCalendar hands one queued push to the platform.
func (o *Outbox) handleCalendar(ctx context.Context, tenant string, now time.Time, it outboxItem) error {
	var p calendarPush
	if err := json.Unmarshal(it.payload, &p); err != nil || p.App == "" || p.Ref == "" {
		return o.mark(ctx, tenant, it, `state = 'failed', attempts = $3, last_error = $4, done_at = $5`, it.attempts+1, "the push cannot be read", now)
	}
	if it.kind == OutboxCalendarUpsert && p.Item == nil {
		return o.mark(ctx, tenant, it, `state = 'failed', attempts = $3, last_error = $4, done_at = $5`, it.attempts+1, "an upsert without its item", now)
	}
	if it.kind == OutboxCalendarCancel {
		p.Item = nil
	}
	if o.CalendarAPI == nil {
		return o.mark(ctx, tenant, it, `next_at = $3, last_error = $4`, now.Add(outboxNotWired), "the platform's calendar is not wired (Deps.CalendarAPI)")
	}
	res, err := o.CalendarAPI(ctx, tenant, p.Ref, p.Version, p.Item)
	if err == nil {
		return o.mark(ctx, tenant, it, `state = 'done', result = $3, last_error = '', done_at = $4`, res, now)
	}
	attempts := it.attempts + 1
	if errors.Is(err, ErrInvalid) || attempts >= OutboxMaxAttempts {
		o.log().Warn("appkit: a calendar push was not applied; kept in the manual list", "tenant", tenant, "key", it.key, "attempts", attempts, "err", err)
		return o.mark(ctx, tenant, it, `state = 'failed', attempts = $3, last_error = $4, done_at = $5`, attempts, clipErr(err), now)
	}
	return o.mark(ctx, tenant, it, `attempts = $3, next_at = $4, last_error = $5`, attempts, now.Add(outboxPause(attempts)), clipErr(err))
}
