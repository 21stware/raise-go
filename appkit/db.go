package appkit

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
)

// Tx runs fn in a transaction scoped to one tenant: every statement sees only that tenant's rows (row-level security,
// ADR-0008), even if it forgets its WHERE clause. Use it for every tenant-owned table. The scope is the platform's settings
// (kwork.tenant, kwork.scope), which the policies of the app's own tables read too (platform.tenant_visible).
func Tx(ctx context.Context, db *sql.DB, tenant string, fn func(*sql.Tx) error) error {
	if db == nil {
		return errors.New("appkit: no database")
	}
	if tenant == "" {
		return errors.New("appkit: tenant scope required")
	}
	return run(ctx, db, tenant, "tenant", fn)
}

// SystemTx runs fn with the platform scope: it sees every tenant. Only for the few paths that must start without a tenant
// (resolving a public share link to the tenant it belongs to); everything after that goes through Tx.
func SystemTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	if db == nil {
		return errors.New("appkit: no database")
	}
	return run(ctx, db, "", "system", fn)
}

func run(ctx context.Context, db *sql.DB, tenant, scope string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx, `SELECT set_config('kwork.tenant', $1, true), set_config('kwork.scope', $2, true)`, tenant, scope); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// errDry ends a dry transaction: it is the sentinel TxDry rolls back on.
var errDry = errors.New("appkit: dry run")

// dryTxs are the transactions TxDry is running (IsDry).
var dryTxs sync.Map

// IsDry says tx is a dry run's (TxDry): code that writes outside it (another service, its own transaction: the access kernel's
// tree, a real-time document) skips the write, since nothing of a dry run may stay.
func IsDry(tx *sql.Tx) bool {
	if tx == nil {
		return false
	}
	_, ok := dryTxs.Load(tx)
	return ok
}

// TxDry runs fn exactly as Tx would and then rolls the transaction back, whatever fn returned. It is how an op's dry-run uses
// the same command code as its execute: the checks, the permission rules and the writes all happen, and nothing stays. What fn
// returns through its own variables is the account of what would have happened; side effects outside the database (blobs,
// notices, audit entries) must stay out of fn, or be skipped when dry.
func TxDry(ctx context.Context, db *sql.DB, tenant string, fn func(*sql.Tx) error) error {
	err := Tx(ctx, db, tenant, func(tx *sql.Tx) error {
		dryTxs.Store(tx, struct{}{})
		defer dryTxs.Delete(tx)
		if err := fn(tx); err != nil {
			return err
		}
		return errDry
	})
	if errors.Is(err, errDry) {
		return nil
	}
	return err
}

// TryLock takes a transaction-scoped advisory lock on key without waiting: true when this transaction now holds it (until it
// ends), false when another transaction (on any replica: the lock lives in the database) holds it. For periodic work that one
// replica is enough to do — a reminder sweep, a trash purge — the replica that does not get it skips.
func TryLock(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var got bool
	err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, "kwork.fp:"+key).Scan(&got)
	return got, err
}

// quoteIdent quotes a PostgreSQL identifier (what lib/pq's QuoteIdentifier does: the driver is the app's choice).
func quoteIdent(name string) string {
	if end := strings.IndexRune(name, 0); end > -1 {
		name = name[:end]
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
