package appkit

import (
	"context"
	"sync"
	"time"
)

// TenantChanges tells the app about changes the platform makes to a tenant outside it: the app installed, upgraded, switched off or
// removed (Installs), and departments, roles or assignments changed (Permissions). An app that keeps state per tenant across
// requests registers here instead of waiting for its periodic re-check. Serve feeds it from the app's services of
// event:app.installed and event:permissions.changed (EventAppInstalled, EventPermissionsChanged), delivered at least once; a change
// heard by no hook is still found by the app's own periodic re-check. Hooks run on their own goroutine with a bounded context; they
// must not block. (The platform's firstparty.TenantChanges, same shape.)
type TenantChanges struct {
	mu      sync.Mutex
	install []func(ctx context.Context, tenant string)
	perms   []func(ctx context.Context, tenant string)
	wg      sync.WaitGroup // hooks still running (tests wait on it)
}

// OnInstall / OnPermissions register a hook. Safe on a nil receiver (nothing is registered: the platform has no bus).
func (h *TenantChanges) OnInstall(f func(ctx context.Context, tenant string)) {
	if h == nil || f == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.install = append(h.install, f)
}

func (h *TenantChanges) OnPermissions(f func(ctx context.Context, tenant string)) {
	if h == nil || f == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.perms = append(h.perms, f)
}

// Installs / Permissions run the hooks for a tenant (the platform calls them from its bus subscriptions). They return at once.
func (h *TenantChanges) Installs(tenant string) {
	h.run(tenant, func() []func(context.Context, string) { return h.install })
}

func (h *TenantChanges) Permissions(tenant string) {
	h.run(tenant, func() []func(context.Context, string) { return h.perms })
}

// changeHookTimeout bounds one hook.
const changeHookTimeout = 10 * time.Second

func (h *TenantChanges) run(tenant string, list func() []func(context.Context, string)) {
	if h == nil || tenant == "" {
		return
	}
	h.mu.Lock()
	fs := append([]func(context.Context, string){}, list()...)
	h.mu.Unlock()
	for _, f := range fs {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), changeHookTimeout)
			defer cancel()
			f(ctx, tenant)
		}()
	}
}

// Wait waits until the hooks started so far have returned (tests).
func (h *TenantChanges) Wait() {
	if h != nil {
		h.wg.Wait()
	}
}
