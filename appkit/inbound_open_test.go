package appkit

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The platform forwards a call of its open API (via open): the app sees who it is for and the scopes granted, for exactly the request
// the assertion signed, and only on /k3/open/v1/…; nothing else reaches those routes.
func TestInboundOpenCall(t *testing.T) {
	sg, err := k3.NewSigner("open")
	if err != nil {
		t.Fatal(err)
	}
	var plat Platform
	var isPlat, called bool
	h := Inbound(sg.JWKS(), "membership", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		plat, isPlat = PlatformOf(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	sign := func(via, sub, method, path, query string, body []byte) string {
		t.Helper()
		in := orNull(body)
		if via == k3.ViaOpen {
			if in, err = k3.OpenInput(method, path, query, body); err != nil {
				t.Fatal(err)
			}
		}
		d, err := k3.InputDigest(in)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		a, err := sg.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Iss: "p", Sub: sub, Aud: k3.Audience{"membership"},
			Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "j-" + path}, Tenant: "alpha", Via: via, InputDigest: d,
			Scopes: []string{"membership.me:read"}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	do := func(method, target string, body []byte, assertion string) int {
		called, isPlat, plat = false, false, Platform{}
		req := httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set(k3.HeaderAssertion, assertion)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// a read: no body (null), the query signed with it
	a := sign(k3.ViaOpen, "customer:c1", http.MethodGet, "/k3/open/v1/me/sessions", "from=2026-11-01", nil)
	if code := do(http.MethodGet, "/k3/open/v1/me/sessions?from=2026-11-01", nil, a); code != http.StatusNoContent || !isPlat {
		t.Fatalf("the forwarded read: %d (platform %v)", code, isPlat)
	}
	if plat.Via != k3.ViaOpen || plat.Caller.User != "customer:c1" || plat.Tenant != "alpha" || !slices.Equal(plat.Scopes, []string{"membership.me:read"}) {
		t.Fatalf("the open call: %+v", plat)
	}
	// the same assertion for another query, path or method is not the call the gate let through
	for _, x := range []struct{ method, target string }{
		{http.MethodGet, "/k3/open/v1/me/sessions?from=2026-12-01"},
		{http.MethodGet, "/k3/open/v1/me/bookings"},
		{http.MethodPost, "/k3/open/v1/me/sessions?from=2026-11-01"},
	} {
		if code := do(x.method, x.target, nil, a); code != http.StatusUnauthorized || called {
			t.Fatalf("%s %s with another request's assertion: %d (called %v)", x.method, x.target, code, called)
		}
	}
	// a write: the body is signed
	body := []byte(`{"memberId":"m1","sessionId":"s1"}`)
	a = sign(k3.ViaOpen, "customer:c1", http.MethodPost, "/k3/open/v1/me/bookings", "", body)
	if code := do(http.MethodPost, "/k3/open/v1/me/bookings", body, a); code != http.StatusNoContent || plat.Caller.User != "customer:c1" {
		t.Fatalf("the forwarded write: %d %+v", code, plat)
	}
	if code := do(http.MethodPost, "/k3/open/v1/me/bookings", []byte(`{"memberId":"m2","sessionId":"s1"}`), a); code != http.StatusUnauthorized || called {
		t.Fatalf("another body: %d (called %v)", code, called)
	}
	// an open assertion without a subject, or anywhere but /k3/open/v1/…, is refused
	if code := do(http.MethodGet, "/k3/open/v1/me", nil, sign(k3.ViaOpen, "", http.MethodGet, "/k3/open/v1/me", "", nil)); code != http.StatusUnauthorized || called {
		t.Fatalf("no subject: %d", code)
	}
	if code := do(http.MethodPost, "/k3/providers/x", []byte(`{}`), sign(k3.ViaOpen, "customer:c1", http.MethodPost, "/k3/providers/x", "", []byte(`{}`))); code != http.StatusUnauthorized || called {
		t.Fatalf("an open assertion on a provider route: %d", code)
	}
	// and another via on /k3/open/v1/… is refused too
	if code := do(http.MethodPost, "/k3/open/v1/me/bookings", body, sign(k3.ViaSiteAction, "customer:c1", "", "", "", body)); code != http.StatusUnauthorized || called {
		t.Fatalf("a site action's assertion on an open route: %d", code)
	}
}

func orNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
