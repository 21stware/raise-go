package appkit

import (
	"context"
	"net/http"
	"strings"

	"github.com/21stware/raise-go/provide"
)

// The headers the app gateway sets on a verified request (internal/modules/appgw); Inbound removes every such header a client sends
// and sets them again from what it verified, so an app can trust these.
const (
	HeaderTenant       = "X-K3-Tenant"
	HeaderUser         = "X-K3-User"
	HeaderRoles        = "X-K3-Roles"
	HeaderName         = "X-K3-Name"         // the person's display name or e-mail (timelines name who acted)
	HeaderInstallation = "X-K3-Installation" // app@version, the version the tenant is pinned to
	HeaderAgent        = "X-K3-Agent"
	// HeaderToken carries the App token the gateway forwards to an app that runs on its own (flow 3).
	HeaderToken = "K3-App-Token"
)

// Caller is the verified person (or agent acting for a person) behind a request.
type Caller = provide.Caller

// CallerOf reads the caller the gateway verified. ok is false for an anonymous request.
func CallerOf(r *http.Request) (Caller, bool) {
	t, u := r.Header.Get(HeaderTenant), r.Header.Get(HeaderUser)
	if t == "" || u == "" {
		return Caller{}, false
	}
	c := Caller{Tenant: t, User: u, Name: r.Header.Get(HeaderName), Agent: r.Header.Get(HeaderAgent)}
	if rs := r.Header.Get(HeaderRoles); rs != "" {
		c.Roles = strings.Split(rs, ",")
	}
	if c.Name == "" {
		c.Name = u
	}
	return c, true
}

type ctxKey struct{}

// WithCaller / From carry the caller in a context (for code below the handler).
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}
func From(ctx context.Context) Caller { c, _ := ctx.Value(ctxKey{}).(Caller); return c }

// The platform roles the kit's rules know (internal/authn).
const (
	RoleTenantMember  = "tenant.member"
	RoleTenantAnalyst = "tenant.analyst"
	RoleTenantAdmin   = "tenant.admin"
	RoleTenantAuditor = "tenant.auditor"
)
