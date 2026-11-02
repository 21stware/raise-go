package appkit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// An agent's App token (the assistant on the app's page for its person) sets the agent, never travels on as the person, and is kept
// as the agent's own delegated session for its person: what the draft reads go to the platform with.
func TestInboundKeepsTheAgentsSession(t *testing.T) {
	sg, err := k3.NewSigner("agent-session")
	if err != nil {
		t.Fatal(err)
	}
	var got agentSession
	var person person
	var hdr string
	h := Inbound(sg.JWKS(), "crm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, person, hdr = agentSessionOf(r.Context()), personToken(r.Context()), r.Header.Get(HeaderAgent)
		w.WriteHeader(http.StatusNoContent)
	}))
	tok, err := sg.Sign(TypAppToken, AppClaims{Registered: k3.Registered{Sub: "mia", Aud: k3.Audience{"crm"}, Exp: time.Now().Add(time.Hour).Unix()},
		Tenant: "alpha", Agent: "agent:kwork"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/drafts", nil)
	req.Header.Set(HeaderToken, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || hdr != "agent:kwork" || person.token != "" {
		t.Fatalf("%d agent %q person %+v", rec.Code, hdr, person)
	}
	if got != (agentSession{token: tok, tenant: "alpha", user: "mia", agent: "agent:kwork"}) {
		t.Fatalf("agent session %+v", got)
	}
}
