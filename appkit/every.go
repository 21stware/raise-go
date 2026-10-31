package appkit

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

// Periodic work of an app (reminder sweeps, trash purges, timeout scans) runs through Deps.Every / Deps.EveryTenant instead of
// a ticker of its own, so every such job behaves the same way:
//   - one replica at a time: a round runs under a database advisory lock (TryLock, held by a transaction on a connection of its
//     own for the whole round); a replica that finds it held skips the round. The lock does not make a round happen once per
//     period across replicas — each replica runs its own schedule — so the work must be idempotent (it already is: reminder
//     rows, SKIP LOCKED, purge preconditions in the database);
//   - per tenant (EveryTenant): each tenant is locked on its own (key "{name}:{tenant}"), so replicas share a round's tenants;
//   - jitter: each wait is the period ± 10 %, so replicas started together drift apart;
//   - a panic in a round is recovered and logged with its stack, an error logged; the schedule goes on;
//   - shutdown (Deps.Shutdown) cancels the round's context and ends the schedule.

// EveryOption tunes Every / EveryTenant.
type EveryOption func(*everyOpts)

type everyOpts struct {
	immediately bool
	unlocked    bool
	jitter      float64
	after       func(time.Duration) <-chan time.Time
}

// Immediately runs the first round when the job is scheduled (default: after the first wait).
func Immediately() EveryOption { return func(o *everyOpts) { o.immediately = true } }

// Unlocked runs Every's rounds without its lock: for a round that takes finer locks itself (EachTenant per tenant), so that
// replicas share the work instead of one doing all of it.
func Unlocked() EveryOption { return func(o *everyOpts) { o.unlocked = true } }

// WithJitter sets the jitter as a fraction of the period (default 0.1; 0 waits exactly the period).
func WithJitter(f float64) EveryOption { return func(o *everyOpts) { o.jitter = min(max(f, 0), 0.5) } }

// WithTimer replaces time.After (tests drive the rounds with it).
func WithTimer(after func(time.Duration) <-chan time.Time) EveryOption {
	return func(o *everyOpts) { o.after = after }
}

// Periodic is a scheduled job. Stop ends it (the platform does through Deps.Shutdown).
type Periodic struct {
	name   string
	cancel context.CancelFunc
	done   chan struct{}
}

// Stop cancels the round in progress and waits until the job has ended. Nil-safe; safe to call twice.
func (p *Periodic) Stop() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
}

// Done is closed when the job has ended.
func (p *Periodic) Done() <-chan struct{} { return p.done }

// Every runs fn every period (± jitter), on one replica at a time (the lock key is name). It returns nil and schedules nothing
// without a database or with a period ≤ 0. name is also what the logs call the job ("biz.remind", "flow.timeouts").
func (d Deps) Every(name string, period time.Duration, fn func(ctx context.Context) error, opts ...EveryOption) *Periodic {
	var o everyOpts
	for _, f := range opts {
		f(&o)
	}
	if o.unlocked {
		return d.schedule(name, period, fn, opts)
	}
	return d.schedule(name, period, func(ctx context.Context) error {
		return d.locked(ctx, name, fn)
	}, opts)
}

// EveryTenant runs fn for each tenant tenants lists, every period (± jitter): each tenant under its own lock ("{name}:{tenant}"),
// skipped when another replica is doing it. A failing tenant does not stop the others. See EachTenant.
func (d Deps) EveryTenant(name string, period time.Duration, tenants func(ctx context.Context) ([]string, error),
	fn func(ctx context.Context, tenant string) error, opts ...EveryOption) *Periodic {
	return d.schedule(name, period, func(ctx context.Context) error {
		var ts []string
		if err := guarded(func() (err error) { ts, err = tenants(ctx); return err }); err != nil {
			return fmt.Errorf("listing tenants: %w", err)
		}
		return d.EachTenant(ctx, name, ts, fn)
	}, opts)
}

// EachTenant is one round of EveryTenant over the given tenants (a test, an operator's "run now"): each tenant in turn, under the
// lock "{name}:{tenant}" (a tenant another replica holds is skipped), a panic recovered. It returns the tenants' errors joined.
func (d Deps) EachTenant(ctx context.Context, name string, tenants []string, fn func(ctx context.Context, tenant string) error) error {
	var errs []error
	for _, t := range tenants {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if err := d.locked(ctx, name+":"+t, func(ctx context.Context) error { return fn(ctx, t) }); err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

// locked runs fn while this replica holds the advisory lock key (TryLock in a transaction of its own that stays open, without
// statements, for the round); it does nothing when another replica holds it. A panic in fn is an error.
func (d Deps) locked(ctx context.Context, key string, fn func(ctx context.Context) error) error {
	if d.DB == nil {
		return errors.New("appkit: no database")
	}
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // the transaction only holds the lock
	got, err := TryLock(ctx, tx, key)
	if err != nil || !got {
		return err
	}
	return guarded(func() error { return fn(ctx) })
}

// guarded calls fn, turning a panic into an error (*PanicError) that carries the stack.
func guarded(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return fn()
}

// PanicError is a panic recovered in a periodic job.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

func (d Deps) schedule(name string, period time.Duration, round func(ctx context.Context) error, opts []EveryOption) *Periodic {
	if d.DB == nil || period <= 0 {
		return nil
	}
	o := everyOpts{jitter: 0.1, after: time.After}
	for _, f := range opts {
		f(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Periodic{name: name, cancel: cancel, done: make(chan struct{})}
	log := d.Logger().With("job", name)
	run := func() {
		start := time.Now()
		err := guarded(func() error { return round(ctx) })
		var pe *PanicError
		switch {
		case err == nil || ctx.Err() != nil:
		case errors.As(err, &pe):
			log.Error("appkit: periodic job panicked", "err", err, "stack", string(pe.Stack), "took", time.Since(start).Round(time.Millisecond))
		default:
			log.Warn("appkit: periodic job failed", "err", err, "took", time.Since(start).Round(time.Millisecond))
		}
	}
	wait := func() bool {
		w := period
		if o.jitter > 0 {
			w += time.Duration((rand.Float64()*2 - 1) * o.jitter * float64(period))
		}
		select {
		case <-ctx.Done():
			return false
		case <-o.after(w):
			return ctx.Err() == nil
		}
	}
	go func() {
		defer close(p.done)
		if o.immediately {
			run()
		}
		for wait() {
			run()
		}
	}()
	var once sync.Once
	d.Shutdown.Add(func(sctx context.Context) {
		once.Do(cancel)
		select {
		case <-p.done:
		case <-sctx.Done():
		}
	})
	return p
}
