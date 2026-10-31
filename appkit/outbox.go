package appkit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// The outbox (firstparty_outbox in the app's own schema; the app's migration makes it with the platform's columns). What an app
// must tell the platform after a business change — a settlement paid is an expense in 经营 — is queued with EnqueueLedger IN the
// business transaction, so it commits with the change or not at all: a process that dies after the commit leaves the row behind, a
// rolled-back change leaves nothing. A worker (Outbox.Run, one per process) delivers what is due; right after the commit the app asks
// for its rows at once (Deps.DeliverNow), so the usual case is as quick as a direct call. The platform is idempotent by the row's key
// (the books record a (source app, reference, kind) once), so a delivery repeated after a crash between "posted" and "marked done"
// records nothing twice.
//
// Outcomes of one delivery of a ledger posting (POST /app/v1/ledger/posts, Deps.LedgerAPI):
//   - taken → done, result = the platform's key;
//   - refused (invalid, conflict, a 4xx) → failed at once: retrying would only repeat the refusal;
//   - any other error → retried after 30 s, 1 min, 2 min … (at most 4 h between tries); after OutboxMaxAttempts tries failed;
//   - no LedgerAPI wired → stays pending, looked at again in an hour.
//
// A kind other than ledger.post is failed (the kit delivers nothing else).

const (
	// OutboxLedgerPost is the kind of a queued ledger posting.
	OutboxLedgerPost = "ledger.post"
	// OutboxMaxAttempts is how many failed deliveries a row gets before it is handed over.
	OutboxMaxAttempts = 12

	outboxFirstPause   = 30 * time.Second
	outboxLongestPause = 4 * time.Hour
	outboxNotWired     = time.Hour
	outboxRetention    = 90 * 24 * time.Hour // delivered and skipped rows are removed after this
	outboxBatch        = 50
	outboxLease        = 5 * time.Minute // how long a claimed row is left to its claimant
)

// LedgerKey is the outbox key of a ledger posting (the same key 经营 keeps).
func LedgerKey(app, ref, kind string) string { return "ledger:" + app + "|" + ref + "|" + kind }

type ledgerPayload struct {
	User    string  `json:"user"`
	Name    string  `json:"name,omitempty"`
	Request PostReq `json:"request"`
}

// EnqueueLedger queues a posting to the books inside the business transaction tx (scoped to c.Tenant), in the outbox of the app's
// schema (Deps.Schema). It returns the key (for DeliverNow once the transaction committed). Queueing the same posting twice keeps
// the first.
func (d Deps) EnqueueLedger(ctx context.Context, tx *sql.Tx, c Caller, req PostReq) (string, error) {
	if err := req.Validate(); err != nil {
		return "", err
	}
	key := LedgerKey(req.SourceApp, req.SourceRef, req.Kind)
	return key, enqueue(ctx, tx, outboxTable(d.Schema), c.Tenant, req.SourceApp, OutboxLedgerPost, key, ledgerPayload{User: c.User, Name: c.Name, Request: req})
}

// outboxTable is the outbox of a schema ("": the one the connection's search_path finds).
func outboxTable(schema string) string {
	if schema == "" {
		return "firstparty_outbox"
	}
	return quoteIdent(schema) + ".firstparty_outbox"
}

func enqueue(ctx context.Context, tx *sql.Tx, table, tenant, app, kind, key string, payload any) error {
	if tenant == "" || app == "" || kind == "" || key == "" {
		return Invalid("an outbox row needs its tenant, app, kind and key")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	_, err = tx.ExecContext(ctx, `INSERT INTO `+table+` (tenant_id, id, app, kind, key, payload) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, key) DO NOTHING`, tenant, "ob_"+hex.EncodeToString(b), app, kind, key, raw)
	return err
}

// DeliverNow delivers the tenant's queued rows with these keys at once (call it after the business transaction committed).
// Best effort: whatever does not go through now is the worker's.
func (d Deps) DeliverNow(ctx context.Context, tenant string, keys ...string) {
	if d.DB == nil || d.LedgerAPI == nil || len(keys) == 0 {
		return
	}
	if err := NewOutbox(d).Deliver(context.WithoutCancel(ctx), tenant, keys...); err != nil {
		d.Logger().Warn("appkit: outbox delivery deferred to the worker", "tenant", tenant, "err", err)
	}
}

// Outbox delivers queued rows.
type Outbox struct {
	DB *sql.DB
	// Now is the clock (nil: the wall clock).
	Now func() time.Time
	Log *slog.Logger
	// LedgerAPI is where the app sends its ledger postings (the platform's POST /app/v1/ledger/posts, idempotent by the same key;
	// ADR-0066 §2): the platform's own outbox then delivers them to the books.
	LedgerAPI func(ctx context.Context, c Caller, req PostReq) (string, error)
	// Schema is whose outbox this is: the app's schema (Deps.Schema).
	Schema string
}

// NewOutbox is the outbox of the app's Deps.
func NewOutbox(d Deps) *Outbox {
	return &Outbox{DB: d.DB, Now: d.Now, Log: d.Log, LedgerAPI: d.LedgerAPI, Schema: d.Schema}
}

func (o *Outbox) table() string { return outboxTable(o.Schema) }

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (o *Outbox) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// Run sweeps every so often until ctx ends.
func (o *Outbox) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := o.Sweep(ctx); err != nil {
			o.log().Warn("appkit: outbox sweep failed", "err", err)
		} else if n > 0 {
			o.log().Info("appkit: outbox delivered", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep delivers what is due, tenant by tenant, and returns how many rows it handled. A replica that finds a tenant's lock
// taken (another replica is on it) skips the tenant; a row a request is delivering right now is skipped too (row lock).
func (o *Outbox) Sweep(ctx context.Context) (int, error) {
	if o.DB == nil {
		return 0, nil
	}
	now := o.now()
	var tenants []string
	err := SystemTx(ctx, o.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+o.table()+` WHERE state IN ('done', 'skipped') AND done_at < $1`, now.Add(-outboxRetention)); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT tenant_id FROM `+o.table()+` WHERE state = 'pending' AND next_at <= $1 LIMIT 1000`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			tenants = append(tenants, t)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, t := range tenants {
		n, err := o.tenant(ctx, t, now, nil)
		if err != nil {
			o.log().Warn("appkit: outbox skipped a tenant this time", "tenant", t, "err", err)
			continue
		}
		total += n
	}
	return total, nil
}

// Deliver delivers the tenant's pending rows with these keys now, whatever their next_at.
func (o *Outbox) Deliver(ctx context.Context, tenant string, keys ...string) error {
	if o.DB == nil || len(keys) == 0 {
		return nil
	}
	_, err := o.tenant(ctx, tenant, o.now(), keys)
	return err
}

type outboxItem struct {
	id, app, kind, key string
	payload            []byte
	attempts           int
	createdAt, claim   time.Time
}

// tenant handles the tenant's due rows (keys == nil: a sweep, under the tenant's sweep lock) or its pending rows with these keys
// (a delivery right after the commit). No transaction is held while the platform works: each row is first claimed (claimed_until,
// in a short transaction of its own; FOR UPDATE SKIP LOCKED decides between two claimants), then delivered, then its outcome is
// written only if the claim is still this one's. A claim whose holder died expires after outboxLease and the row is delivered
// again (the platform is idempotent by key).
func (o *Outbox) tenant(ctx context.Context, tenant string, now time.Time, keys []string) (int, error) {
	if keys != nil {
		return o.drain(ctx, tenant, now, keys)
	}
	n := 0
	err := Tx(ctx, o.DB, tenant, func(tx *sql.Tx) error {
		if got, err := TryLock(ctx, tx, "outbox:"+tenant); err != nil || !got {
			return err // another replica sweeps this tenant now
		}
		var err error
		n, err = o.drain(ctx, tenant, now, nil)
		return err
	})
	return n, err
}

// drain claims and delivers rows one by one (at most outboxBatch for a sweep).
func (o *Outbox) drain(ctx context.Context, tenant string, now time.Time, keys []string) (int, error) {
	n := 0
	done := []string{} // rows handled in this pass (a delivery by key ignores next_at: each is tried once)
	for keys != nil || n < outboxBatch {
		it, ok, err := o.claim(ctx, tenant, now, keys, done)
		if err != nil || !ok {
			return n, err
		}
		if err := o.handle(ctx, tenant, now, it); err != nil {
			return n, err
		}
		done = append(done, it.id)
		n++
	}
	return n, nil
}

// claim takes the next row (a due one, or one with these keys) for outboxLease.
func (o *Outbox) claim(ctx context.Context, tenant string, now time.Time, keys, skip []string) (outboxItem, bool, error) {
	var it outboxItem
	cond, arg := `next_at <= $1 AND id <> ALL($3)`, any(textArray(skip))
	if keys != nil {
		cond, arg = `key = ANY($3) AND id <> ALL($4)`, nil
	}
	found := false
	err := Tx(ctx, o.DB, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE `+o.table()+` SET claimed_until = $2 WHERE id = (
			  SELECT id FROM `+o.table()+` WHERE state = 'pending' AND `+cond+` AND (claimed_until IS NULL OR claimed_until <= $1)
			  ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING id, app, kind, key, payload, attempts, created_at, claimed_until`, append([]any{now, now.Add(outboxLease)}, args(arg, keys, skip)...)...).
			Scan(&it.id, &it.app, &it.kind, &it.key, &it.payload, &it.attempts, &it.createdAt, &it.claim)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return it, found, err
}

func args(sweep any, keys, skip []string) []any {
	if keys == nil {
		return []any{sweep}
	}
	return []any{textArray(keys), textArray(skip)}
}

// textArray is a text[] parameter written as PostgreSQL's array literal (what lib/pq's StringArray sends; the driver is the app's).
type textArray []string

func (a textArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// mark writes the outcome of a claimed row (q: the SET list after claimed_until, args from $3 on), only if the claim is still the
// claimant's.
func (o *Outbox) mark(ctx context.Context, tenant string, it outboxItem, q string, args ...any) error {
	return Tx(ctx, o.DB, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE `+o.table()+` SET claimed_until = NULL, `+q+` WHERE id = $1 AND claimed_until = $2`,
			append([]any{it.id, it.claim}, args...)...)
		return err
	})
}

// handle delivers one claimed row by its kind.
func (o *Outbox) handle(ctx context.Context, tenant string, now time.Time, it outboxItem) error {
	if it.kind != OutboxLedgerPost {
		return o.mark(ctx, tenant, it, `state = 'failed', last_error = $3, done_at = $4`, "unknown kind "+it.kind, now)
	}
	set := func(q string, args ...any) error { return o.mark(ctx, tenant, it, q, args...) }
	var p ledgerPayload
	if err := json.Unmarshal(it.payload, &p); err != nil {
		return set(`state = 'failed', last_error = $3, done_at = $4`, "unreadable payload: "+err.Error(), now)
	}
	if o.LedgerAPI == nil {
		return set(`next_at = $3, last_error = $4`, now.Add(outboxNotWired), "the platform's ledger is not wired (Deps.LedgerAPI)")
	}
	c := Caller{Tenant: tenant, User: p.User, Name: p.Name}
	ref, err := o.LedgerAPI(ctx, c, p.Request) // handed to the platform, which keeps it until the books take it
	if err == nil {
		return set(`state = 'done', result = $3, last_error = '', done_at = $4`, ref, now)
	}
	attempts := it.attempts + 1
	if refused(err) || attempts >= OutboxMaxAttempts {
		o.log().Warn("appkit: the platform refused a posting; kept in the manual list", "tenant", tenant, "key", it.key, "attempts", attempts, "err", err)
		return set(`state = 'failed', attempts = $3, last_error = $4, done_at = $5`, attempts, clipErr(err), now)
	}
	return set(`attempts = $3, next_at = $4, last_error = $5`, attempts, now.Add(outboxPause(attempts)), clipErr(err))
}

// outboxPause is the wait after the n-th failed delivery: 30 s doubling, at most 4 h.
func outboxPause(n int) time.Duration {
	d := outboxFirstPause
	for i := 1; i < n && d < outboxLongestPause; i++ {
		d *= 2
	}
	return min(d, outboxLongestPause)
}

// refused reports an error that retrying cannot fix (the receiver said no, not "not now").
func refused(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status < http.StatusInternalServerError
	}
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden)
}

func clipErr(err error) string {
	s := err.Error()
	if r := []rune(s); len(r) > 500 {
		s = string(r[:500]) + "…"
	}
	return s
}
