package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/21stware/raise-go/k3"
)

// TypAppToken is the JWS typ of an App token (the gateway's, internal/modules/appgw).
const TypAppToken = "k3-app+jwt"

// AppClaims are the claims of an App token: the person the gateway verified, for one app (audience) and one tenant.
type AppClaims struct {
	k3.Registered
	Tenant string   `json:"tenant"`
	Roles  []string `json:"roles,omitempty"`
	// Name is the person's display name or e-mail (the audit trail names who acted).
	Name string `json:"name,omitempty"`
	// Agent names the agent that acts for Sub (set only by the agent read endpoint). Its calls are read-only.
	Agent string `json:"agent,omitempty"`
	// Env is the tenant's environment on the open platform (k3.EnvTest for a test enterprise): Tokens takes the app key of that
	// environment for the app's own calls while it serves the request. Empty: a platform older than the claim.
	Env string `json:"env,omitempty"`
}

// Inbound is the door of an app that runs in a process of its own (ADR-0066 §5.1, ADR-0130 §2.3). The app's handlers read the caller
// from the X-K3-* headers (CallerOf), which in the platform's process only the gateway sets; here anybody who reaches the port could,
// so Inbound drops every such header and sets them again from what it verified with the platform's keys (/.well-known/k3-jwks.json):
//
//   - a person: the App token the gateway forwards (K3-App-Token), for this app, not expired → the caller headers, and the token on
//     the context so the app's own calls to the platform go as that person (a page session, flow 3);
//   - the platform: its signed assertion (K3-Assertion, flow 5) for this app, via an event, a site action, a provider or a service,
//     bound to the body → PlatformCall on the context (ops carry their own assertion, checked by the ops handler);
//   - anyone else: no caller (the app's anonymous routes answer for themselves).
//
// The request's trace (traceparent, tracestate) goes on the context too: the app's calls to the platform carry it on (Client), so one
// trace runs platform → app → platform (ADR-0130 §2.2).
func Inbound(keys k3.KeySource, app string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{HeaderTenant, HeaderUser, HeaderRoles, HeaderName, HeaderInstallation, HeaderAgent} {
			r.Header.Del(h)
		}
		ctx := withTrace(r.Context(), r.Header)
		if a := r.Header.Get(k3.HeaderAssertion); a != "" { // an op or another call of the platform's: the tenant's environment (Tokens)
			ctx = assertedEnv(ctx, keys, app, a)
		}
		if tok := r.Header.Get(HeaderToken); tok != "" {
			var c AppClaims
			if err := k3.Verify(ctx, tok, TypAppToken, keys, &c); err != nil || !c.Aud.Contains(app) || c.Tenant == "" || c.Sub == "" ||
				strings.HasPrefix(c.Sub, appPrefix) { // a page session is a person's: an app's subject is never one (ownSubject)
				http.Error(w, `{"error":"the App token is not valid"}`, http.StatusUnauthorized)
				return
			}
			ctx = withTenantEnv(ctx, c.Tenant, c.Env)
			r.Header.Set(HeaderTenant, c.Tenant)
			r.Header.Set(HeaderUser, c.Sub)
			r.Header.Set(HeaderRoles, strings.Join(c.Roles, ","))
			if c.Name != "" {
				r.Header.Set(HeaderName, c.Name)
			}
			if c.Agent != "" {
				r.Header.Set(HeaderAgent, c.Agent)
			}
			if c.Agent == "" { // an agent's token is read-only and never travels on to the platform as the person
				ctx = WithPersonToken(ctx, tok, c.Tenant)
			} else { // it goes to the platform only as the agent's own, delegated session, for the draft reads (Drafts)
				ctx = withAgentSession(ctx, tok, c.Tenant, c.Sub, c.Agent)
			}
		} else if a := r.Header.Get(k3.HeaderAssertion); a != "" && strings.HasPrefix(r.URL.Path, "/k3/") && !strings.HasPrefix(r.URL.Path, "/k3/apps/") {
			pc, ok := verifyCall(ctx, r, keys, app, a, platformVias)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the platform's assertion is not valid"})
				return
			}
			ctx = context.WithValue(ctx, platformKey{}, pc)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// assertedEnv puts on ctx the tenant's environment the platform's assertion names (k3.AssertionClaims.Env), when the assertion is
// the platform's and for this app: Tokens takes the app key of that environment for the app's calls back. Nothing else about the
// request is decided here (the ops handler and verifyCall check the rest).
func assertedEnv(ctx context.Context, keys k3.KeySource, app, tok string) context.Context {
	var c k3.AssertionClaims
	if err := k3.Verify(ctx, tok, k3.TypAssertion, keys, &c); err != nil || !c.Aud.Contains(app) || c.Tenant == "" || c.Env == "" {
		return ctx
	}
	return withTenantEnv(ctx, c.Tenant, c.Env)
}

// platformVias are the assertions of the platform's calls Inbound accepts (ops are the ops handler's).
var platformVias = []string{k3.ViaEvent, k3.ViaSiteAction, k3.ViaProvider, k3.ViaService, k3.ViaOpen}

// openPrefix is where an app answers the platform's open API calls it forwards (ViaOpen): only there, and only those.
const openPrefix = "/k3/open/v1/"

// verifyCall checks the platform's assertion of a call (its via one of vias) and that it was signed for exactly this body. The body is
// read and put back.
func verifyCall(ctx context.Context, r *http.Request, keys k3.KeySource, app, tok string, vias []string) (Platform, bool) {
	var c k3.AssertionClaims
	if err := k3.Verify(ctx, tok, k3.TypAssertion, keys, &c); err != nil || !c.Aud.Contains(app) || c.Tenant == "" || !slices.Contains(vias, c.Via) {
		return Platform{}, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return Platform{}, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	signed := body
	if open := strings.HasPrefix(r.URL.Path, openPrefix); open != (c.Via == k3.ViaOpen) || open && c.Sub == "" {
		return Platform{}, false // an open API call is answered for the one the gate let through, and nothing else reaches those routes
	}
	if c.Via == k3.ViaOpen { // the platform signs the request it forwards: method, path, query and body (k3.OpenInput)
		if signed, err = k3.OpenInput(r.Method, r.URL.Path, r.URL.RawQuery, body); err != nil {
			return Platform{}, false
		}
	}
	if c.Via == k3.ViaService { // the service runtime signs the run's input, not the delivery around it
		var run struct {
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(body, &run) != nil || len(run.Input) == 0 {
			return Platform{}, false
		}
		signed = run.Input
	}
	if d, err := k3.InputDigest(signed); err != nil || d != c.InputDigest {
		return Platform{}, false
	}
	p := Platform{Tenant: c.Tenant, Via: c.Via, Op: c.Op, Key: c.IdempotencyKey, Scopes: c.Scopes}
	if c.Sub != "" && c.Sub != "platform" && !strings.HasPrefix(c.Sub, appPrefix) { // an app's subject is no person (noPerson)
		p.Caller = Caller{Tenant: c.Tenant, User: c.Sub, Name: c.Name, Roles: c.Roles, Agent: c.Agent, Via: c.Origin}
	}
	return p, true
}

// Platform is a call the platform made to the app (Inbound verified its assertion): for which tenant, why, and — for a provider asked
// for a person (a 分析 viewer) — that person.
type Platform struct {
	Tenant, Via, Op, Key string
	Caller               Caller // zero when no person is behind the call
	// Scopes are what the caller's token was granted, for a forwarded open API call (Via k3.ViaOpen; Caller.User is who it is for,
	// "customer:{id}" for a customer). The app checks the scope its route needs.
	Scopes []string
}

type platformKey struct{}

// PlatformCall says whether the request is the platform's own call (verified by Inbound), and for which tenant.
func PlatformCall(ctx context.Context) (tenant string, ok bool) {
	p, ok := PlatformOf(ctx)
	return p.Tenant, ok
}

// PlatformOf returns the platform's call Inbound verified.
func PlatformOf(ctx context.Context) (Platform, bool) {
	p, ok := ctx.Value(platformKey{}).(Platform)
	return p, ok && p.Tenant != ""
}

// The W3C trace context headers Inbound keeps and Client sends on.
var traceHeaders = []string{"traceparent", "tracestate"}

type traceKey struct{}

func withTrace(ctx context.Context, h http.Header) context.Context {
	var t map[string]string
	for _, k := range traceHeaders {
		if v := h.Get(k); v != "" {
			if t == nil {
				t = map[string]string{}
			}
			t[k] = v
		}
	}
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, t)
}

func traceOf(ctx context.Context) map[string]string {
	t, _ := ctx.Value(traceKey{}).(map[string]string)
	return t
}
