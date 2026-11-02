package appkit

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// A provider call the platform makes for a person whose agent acts for them carries the agent and how the call came about (the
// assertion's agent and origin claims): the app's Caller is the one its code sees in the platform's process, so "an agent may read,
// never write" is the app's same rule in both modes.
func TestInboundProviderCallCarriesTheAgent(t *testing.T) {
	sg, err := k3.NewSigner("agent")
	if err != nil {
		t.Fatal(err)
	}
	var plat Platform
	h := Inbound(sg.JWKS(), "crm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plat, _ = PlatformOf(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	body := []byte(`{"subject":"leo"}`)
	d, err := k3.InputDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ agent, origin string }{{"agent:assistant", k3.ViaDraft}, {"", ""}} {
		now := time.Now()
		a, err := sg.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Iss: "p", Sub: "ada", Aud: k3.Audience{"crm"},
			Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: "j-" + tc.agent}, Tenant: "alpha", Via: k3.ViaProvider, Op: "handover",
			InputDigest: d, Name: "Ada", Agent: tc.agent, Origin: tc.origin})
		if err != nil {
			t.Fatal(err)
		}
		plat = Platform{}
		req := httptest.NewRequest(http.MethodPost, "/k3/providers/handover", bytes.NewReader(body))
		req.Header.Set(k3.HeaderAssertion, a)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent || plat.Caller.User != "ada" || plat.Caller.Agent != tc.agent || plat.Caller.Via != tc.origin {
			t.Fatalf("agent %q: %d %+v", tc.agent, rec.Code, plat.Caller)
		}
	}
}
