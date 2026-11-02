package appkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	raise "github.com/21stware/raise-go"
)

// Tokens is the TokenSource of an app that runs in a process of its own (ADR-0130 §2.3): a call made for a person carries that person's
// page session (the App token Inbound put on the context, flow 3); any other call — the app's own work, a call the platform made to the
// app — carries the app's tenant token (flow 1), which the app gets for its app key (K3_APP_CLIENT_ID / K3_APP_CLIENT_SECRET) at
// POST /open/v1/oauth/token. The exchange, the per-tenant cache and the renewal 60 seconds before the end are the Go SDK's.
type Tokens struct {
	Base         string // the platform, e.g. "http://kwork:8000" (the token endpoint is Base + /open/v1/oauth/token)
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	// App is the app's id: "app:"+App is its own subject (ownSubject); empty, every "app:…" subject is refused.
	App string

	once sync.Once
	sdk  *raise.Client
}

// Source returns the TokenSource.
func (t *Tokens) Source() TokenSource {
	return func(ctx context.Context, c Caller) (string, error) {
		// a call for a person (c.User) goes with the person's App token of the request; a call without one is the app's own (the
		// access kernel's tree writes, the app's reads of its tenant) and goes with the app's token even while it serves a person's
		// request. A caller whose subject is the app itself ("app:{id}", what the platform's services call an app's own call) is no
		// person either — but only the subject of exactly this app (App); any other "app:…" subject is refused, it gets neither the
		// person's token nor the app's (ADR-0130 P11 S3, as internal/remoteapp).
		own, err := ownSubject(t.App, c)
		if err != nil {
			return "", err
		}
		if p := personToken(ctx); p.token != "" && p.tenant == c.Tenant && !own {
			return p.token, nil
		}
		return t.tenant(ctx, c.Tenant)
	}
}

func (t *Tokens) tenant(ctx context.Context, tenant string) (string, error) {
	if tenant == "" {
		return "", errors.New("appkit: a call without a tenant")
	}
	if t.ClientID == "" || t.ClientSecret == "" {
		return "", errors.New("appkit: no app key (K3_APP_CLIENT_ID, K3_APP_CLIENT_SECRET)")
	}
	t.once.Do(func() {
		hc := t.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 10 * time.Second}
		}
		t.sdk = raise.New(t.ClientID, t.ClientSecret, raise.WithBaseURL(strings.TrimRight(t.Base, "/")+"/open/v1"), raise.WithHTTPClient(hc))
	})
	return t.sdk.Token(ctx, tenant)
}

// appPrefix starts the subject the platform's services give an app's own call ("app:{id}"). No person has it: Inbound refuses an App
// token whose subject starts with it, and a platform call naming one is no person's (noPerson).
const appPrefix = "app:"

// ownSubject says whether c is the app's own call: no subject, or exactly "app:"+app. Any other "app:…" subject — another app's, or
// any when the app's id is not known — is refused (ErrForbidden): it is no person, and not this app either.
func ownSubject(app string, c Caller) (bool, error) {
	if c.User == "" {
		return true, nil
	}
	if !strings.HasPrefix(c.User, appPrefix) {
		return false, nil
	}
	if app != "" && c.User == appPrefix+app {
		return true, nil
	}
	return false, fmt.Errorf("%w: appkit: the caller %q is not this app (%q) and no person", ErrForbidden, c.User, app)
}

// isPerson says c is a call made for a person: a subject that is not an app's ("" or "app:…"; ownSubject refuses a foreign one).
func isPerson(c Caller) bool { return c.User != "" && !strings.HasPrefix(c.User, appPrefix) }

// noPerson is a caller the platform named in a call to the app (an assertion's subject, a signed body's caller) as the app may use it:
// an "app:…" subject is no person, so it becomes the app's own work for that tenant (no subject), never a subject the app's client
// would take for a person or for itself.
func noPerson(c Caller) Caller {
	if strings.HasPrefix(c.User, appPrefix) {
		return Caller{Tenant: c.Tenant, Name: c.Name, Via: c.Via}
	}
	return c
}

type personKey struct{}

type person struct{ token, tenant string }

// WithPersonToken puts the App token of the person a request came from on its context (Inbound does it).
func WithPersonToken(ctx context.Context, token, tenant string) context.Context {
	return context.WithValue(ctx, personKey{}, person{token: token, tenant: tenant})
}

func personToken(ctx context.Context) person {
	p, _ := ctx.Value(personKey{}).(person)
	return p
}

type agentKey struct{}

type agentSession struct{ token, tenant, user, agent string }

// withAgentSession puts the App token of an agent's page session on the context (Inbound does it). Unlike the person's token it is
// never the token of the app's calls for a person (TokenSource): only the draft reads use it, for the agent, as a delegated session
// the platform answers redacted (ADR-0130 P10 leftover).
func withAgentSession(ctx context.Context, token, tenant, user, agent string) context.Context {
	return context.WithValue(ctx, agentKey{}, agentSession{token: token, tenant: tenant, user: user, agent: agent})
}

func agentSessionOf(ctx context.Context) agentSession {
	s, _ := ctx.Value(agentKey{}).(agentSession)
	return s
}

type sessionTokenKey struct{}

// withSessionToken makes the client's next call on ctx go with token instead of what the TokenSource answers (the agent's session).
func withSessionToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, sessionTokenKey{}, token)
}

func sessionToken(ctx context.Context) string {
	t, _ := ctx.Value(sessionTokenKey{}).(string)
	return t
}
