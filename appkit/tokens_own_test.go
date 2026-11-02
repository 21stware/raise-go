package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// ADR-0130 P11 S3 (as internal/remoteapp): inside a person's request (their page session on the context) a call for the person carries
// the page session; the app's own call — no subject, or exactly "app:"+the app's id — never does, it goes for the app's tenant token
// (here: no app key, so the source says so instead of handing out the person's token). Any other "app:…" subject is refused: it is no
// person and not this app, so it gets neither token.
func TestTheAppsOwnCallNeverCarriesThePersonsToken(t *testing.T) {
	src := (&Tokens{Base: "http://platform.test", App: "membership"}).Source()
	ctx := WithPersonToken(context.Background(), "page-session", "alpha")
	if tok, err := src(ctx, Caller{Tenant: "alpha", User: "desk"}); err != nil || tok != "page-session" {
		t.Fatalf("the person's call: %q %v", tok, err)
	}
	for _, c := range []Caller{{Tenant: "alpha"}, {Tenant: "alpha", User: "app:membership", Name: "membership"}} {
		if tok, err := src(ctx, c); err == nil || tok == "page-session" || errors.Is(err, ErrForbidden) {
			t.Fatalf("the app's own call %+v went with %q (%v), want the tenant token's path", c, tok, err)
		}
	}
	for _, c := range []Caller{{Tenant: "alpha", User: "app:other"}, {Tenant: "alpha", User: "app:membershipx"}, {Tenant: "alpha", User: "app:"}} {
		if tok, err := src(ctx, c); !errors.Is(err, ErrForbidden) || tok != "" {
			t.Fatalf("%+v: %q %v, want refused", c, tok, err)
		}
	}
	if _, err := (&Tokens{}).Source()(ctx, Caller{Tenant: "alpha", User: "app:membership"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an app subject with no app id known: %v", err)
	}
	if tok, _ := src(ctx, Caller{Tenant: "beta", User: "desk"}); tok == "page-session" {
		t.Fatal("a call for another tenant carried this tenant's page session")
	}
	for _, c := range []Caller{{Tenant: "alpha"}, {Tenant: "alpha", User: "app:membership"}} {
		if p := personToken(asWho(ctx, c)); p.token != "" {
			t.Fatalf("asWho kept the page session for %+v", c)
		}
	}
	if p := personToken(asWho(ctx, Caller{Tenant: "alpha", User: "desk"})); p.token != "page-session" {
		t.Fatal("asWho dropped the person's page session")
	}
}

// The client refuses another app's subject before it asks any token source or the platform; its own subject goes through.
func TestTheClientRefusesAnotherAppsSubject(t *testing.T) {
	var asked, served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	tok := func(context.Context, Caller) (string, error) { asked.Add(1); return "t", nil }
	cl := &Client{Base: srv.URL, Token: tok, App: "membership"}
	ctx := context.Background()
	for _, user := range []string{"app:other", "app:membershipx", "app:"} {
		if err := cl.call(ctx, Caller{Tenant: "alpha", User: user}, "GET", "/app/v1/x", nil, nil, nil); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%q: %v, want refused", user, err)
		}
		if _, err := cl.stream(ctx, Caller{Tenant: "alpha", User: user}, "GET", "/app/v1/x", nil, nil, 0, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("stream %q: %v, want refused", user, err)
		}
	}
	if asked.Load() != 0 || served.Load() != 0 {
		t.Fatalf("a refused caller reached the token source (%d) or the platform (%d)", asked.Load(), served.Load())
	}
	for _, c := range []Caller{{Tenant: "alpha"}, {Tenant: "alpha", User: "app:membership"}, {Tenant: "alpha", User: "desk"}} {
		if err := cl.call(ctx, c, "GET", "/app/v1/x", nil, nil, nil); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	// a client that does not know its app: no "app:…" subject is its own
	if err := (&Client{Base: srv.URL, Token: tok}).call(ctx, Caller{Tenant: "alpha", User: "app:membership"}, "GET", "/app/v1/x", nil, nil, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no app id: %v", err)
	}
}

// A person never has an app's subject in the app: an App token whose subject starts with "app:" is refused at the door, a platform's
// assertion naming one is no person's call, and a caller in a signed body naming one reaches the app as no person.
func TestNoPersonHasAnAppsSubject(t *testing.T) {
	sg, err := k3.NewSigner("own")
	if err != nil {
		t.Fatal(err)
	}
	var called bool
	var plat Platform
	var isPlat bool
	h := Inbound(sg.JWKS(), "membership", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		plat, isPlat = PlatformOf(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	now := time.Now()
	for _, sub := range []string{"app:membership", "app:other"} {
		called = false
		tok, err := sg.Sign(TypAppToken, AppClaims{Registered: k3.Registered{Sub: sub, Aud: k3.Audience{"membership"}, Exp: now.Add(time.Hour).Unix()}, Tenant: "alpha"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/campaigns", nil)
		req.Header.Set(HeaderToken, tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || called {
			t.Fatalf("an App token for %q: %d (called %v)", sub, rec.Code, called)
		}
	}
	body := `{"caller":{"tenant":"alpha"}}`
	digest, err := k3.InputDigest(json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	a, err := sg.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Iss: "p", Sub: "app:other", Aud: k3.Audience{"membership"}, Iat: now.Unix(),
		Exp: now.Add(time.Minute).Unix(), Jti: "j1"}, Tenant: "alpha", Via: k3.ViaProvider, Name: "other", InputDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/k3/providers/x", strings.NewReader(body))
	req.Header.Set(k3.HeaderAssertion, a)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !isPlat {
		t.Fatalf("the platform's call: %d %v", rec.Code, isPlat)
	}
	if plat.Caller.User != "" {
		t.Fatalf("an assertion naming an app made a person: %+v", plat.Caller)
	}
	if c := noPerson(Caller{Tenant: "alpha", User: "app:order", Name: "order"}); c.User != "" || c.Tenant != "alpha" {
		t.Fatalf("noPerson: %+v", c)
	}
	if c := noPerson(Caller{Tenant: "alpha", User: "mia"}); c.User != "mia" {
		t.Fatalf("noPerson changed a person: %+v", c)
	}
}

// An op runs for the person the platform's assertion names: an assertion naming an app's subject is refused before the op runs.
func TestAnOpIsNeverRunForAnAppsSubject(t *testing.T) {
	ran := 0
	f := adapt(func(context.Context, Caller, json.RawMessage) (OpResult, error) { ran++; return OpResult{}, nil })
	for _, sub := range []string{"app:membership", "app:other"} {
		_, err := f(context.Background(), opCaller{Subject: sub, Tenant: "alpha"}, nil)
		var of *opFailure
		if !errors.As(err, &of) || of.Status != http.StatusUnauthorized {
			t.Fatalf("%q: %v, want 401", sub, err)
		}
	}
	if ran != 0 {
		t.Fatalf("the op ran %d times for an app's subject", ran)
	}
	if _, err := f(context.Background(), opCaller{Subject: "mia", Tenant: "alpha"}, nil); err != nil || ran != 1 {
		t.Fatalf("a person's op: %v (ran %d)", err, ran)
	}
}
