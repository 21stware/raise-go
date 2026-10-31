package appkit

import (
	"context"
	"errors"
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

	once sync.Once
	sdk  *raise.Client
}

// Source returns the TokenSource.
func (t *Tokens) Source() TokenSource {
	return func(ctx context.Context, c Caller) (string, error) {
		if p := personToken(ctx); p.token != "" && p.tenant == c.Tenant {
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
