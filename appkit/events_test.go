package appkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// eventsApp is any app that declares every platform data event in events.receives.
type eventsApp struct {
	receives []string
	mu       sync.Mutex
	seen     []string
}

func (*eventsApp) ID() string { return "crm" }

func (a *eventsApp) Manifest(version, base string) *k3.Manifest {
	m := NewManifest("crm", "客户", version, base)
	m.Events = &k3.EventsDecl{Receives: a.receives}
	return m
}

func (*eventsApp) Handler(Deps) http.Handler { return http.NotFoundHandler() }

func (a *eventsApp) DataEvent(_ context.Context, _ Deps, kind, tenant, subject string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, kind+" "+tenant+" "+subject)
	return nil
}

// ADR-0130 T11, the app's side of the contract: POST /k3/events/{kind} takes every data event kind the manifest declares,
// customer.merged and app.uninstalled included, from the platform (via event, the same tenant), and nothing else.
func TestServeTakesEveryDeclaredDataEvent(t *testing.T) {
	if !slices.Equal(DataEvents, k3.DataEventKinds) {
		t.Fatalf("appkit.DataEvents %v, the manifest's kinds %v", DataEvents, k3.DataEventKinds)
	}
	if !slices.Contains(DataEvents, EventCustomerMerged) || !slices.Contains(DataEvents, EventAppUninstalled) {
		t.Fatalf("DataEvents %v", DataEvents)
	}
	m := NewManifest("crm", "客户", "1.0.0", "example.test")
	m.Nav = []k3.NavItem{{Label: "首页", Route: "/"}}
	m.Events = &k3.EventsDecl{Receives: []string{EventAppUninstalled}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "opsApi is required") {
		t.Fatalf("data events need the ops address: %v", err)
	}
	m.OpsAPI = OpsAPIFor(m.Web.URL, m.ID)
	if err := m.Validate(); err != nil {
		t.Fatalf("a manifest may receive app.uninstalled: %v", err)
	}
	m.Events = &k3.EventsDecl{Emits: []string{"crm.thing"}, Receives: []string{"app.removed"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "app.uninstalled") {
		t.Fatalf("an unknown kind names the ones there are: %v", err)
	}

	sg, err := k3.NewSigner("events")
	if err != nil {
		t.Fatal(err)
	}
	app := &eventsApp{receives: slices.Clone(DataEvents)}
	h, err := Serve(app, Deps{}, sg.JWKS(), "1.0.0", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	n := 0
	post := func(kind, via, tenant, subject string) int {
		t.Helper()
		body, _ := json.Marshal(DataEventCall{Tenant: tenant, Subject: subject})
		digest, err := k3.InputDigest(json.RawMessage(body))
		if err != nil {
			t.Fatal(err)
		}
		n++
		now := time.Now()
		tok, err := sg.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Iss: "p", Aud: k3.Audience{"crm"}, Iat: now.Unix(),
			Exp: now.Add(time.Minute).Unix(), Jti: "j" + kind + string(rune('a'+n))}, Tenant: "alpha", Via: via, InputDigest: digest})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+PathEventFmt+kind, strings.NewReader(string(body)))
		req.Header.Set(k3.HeaderAssertion, tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	var want []string
	for _, k := range DataEvents {
		subject := "alpha"
		switch k {
		case EventSiteDeleted:
			subject = "st_1"
		case EventCustomerMerged:
			subject = MergedSubject("cu_b", "cu_a")
		case EventAppUninstalled:
			subject = "crm"
		}
		if c := post(k, k3.ViaEvent, "alpha", subject); c != http.StatusNoContent {
			t.Fatalf("%s: %d", k, c)
		}
		want = append(want, k+" alpha "+subject)
	}
	if c := post("order.deleted", k3.ViaEvent, "alpha", "o1"); c != http.StatusNotFound {
		t.Fatalf("an undeclared kind: %d", c)
	}
	if c := post(EventAppUninstalled, k3.ViaEvent, "beta", "crm"); c != http.StatusForbidden {
		t.Fatalf("another tenant's event: %d", c)
	}
	if c := post(EventAppUninstalled, k3.ViaProvider, "alpha", "crm"); c != http.StatusUnauthorized {
		t.Fatalf("not an event call: %d", c)
	}
	if !slices.Equal(app.seen, want) {
		t.Fatalf("seen %v, want %v", app.seen, want)
	}

	// an app that declares only tenant.deleted is not handed customer.merged or app.uninstalled
	only := &eventsApp{receives: []string{EventTenantDeleted}}
	h2, err := Serve(only, Deps{}, sg.JWKS(), "1.0.0", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = h2
	for _, k := range []string{EventCustomerMerged, EventAppUninstalled} {
		if c := post(k, k3.ViaEvent, "alpha", "x"); c != http.StatusNotFound {
			t.Fatalf("%s undeclared: %d", k, c)
		}
	}
	if len(only.seen) != 0 {
		t.Fatalf("seen %v", only.seen)
	}
}
