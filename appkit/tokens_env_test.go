package appkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	raise "github.com/21stware/raise-go"
	"github.com/21stware/raise-go/k3"
)

// envPlatform is the platform's token endpoint as far as environments go (internal/modules/openv1 envAllows): the live key reaches
// real tenants, the test key the developer's test enterprises, anything else is refused with wrong_environment. It records which key
// asked for which tenant. Its refusals are in the shape the platform's public listener writes them (openv1.Front).
type envPlatform struct {
	srv  *httptest.Server
	mu   sync.Mutex
	asks []string // "live:acme", "test:sbx", …
}

const (
	liveID, liveSecret = "kwo_live_id", "kwo_live_secret"
	testID, testSecret = "kwo_test_id", "kwo_test_secret"
)

func newEnvPlatform(t *testing.T, test ...string) *envPlatform {
	t.Helper()
	isTest := map[string]bool{}
	for _, tn := range test {
		isTest[tn] = true
	}
	p := &envPlatform{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/open/v1/oauth/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var in struct {
			Tenant string `json:"tenant"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		id, secret, _ := r.BasicAuth()
		var key string
		switch {
		case id == liveID && secret == liveSecret:
			key = "live"
		case id == testID && secret == testSecret:
			key = "test"
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_client","message":"the credential is not valid"}}`))
			return
		}
		p.mu.Lock()
		p.asks = append(p.asks, key+":"+in.Tenant)
		p.mu.Unlock()
		if (key == "test") != isTest[in.Tenant] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"wrong_environment","message":"a live credential cannot be used for a test enterprise"}}`))
			return
		}
		env := "live"
		if key == "test" {
			env = "test"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": key + "-token-" + in.Tenant, "token_type": "Bearer", "expires_in": 300,
			"tenant": in.Tenant, "environment": env})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *envPlatform) asked() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := strings.Join(p.asks, ",")
	p.asks = nil
	return out
}

// ADR-0130 P14 note (T16): a hosted app's process has its live key and its test key. A test enterprise's tenant token is asked for
// with the test key when the request says the tenant is one (the platform's assertion or App token, env "test"), a real tenant's with
// the live key; the platform refuses a live key in a test enterprise, so without this an op that calls back fails there.
func TestTheTestKeyServesATestEnterprise(t *testing.T) {
	p := newEnvPlatform(t, "sbx")
	tok := &Tokens{Base: p.srv.URL, ClientID: liveID, ClientSecret: liveSecret, TestClientID: testID, TestClientSecret: testSecret, App: "membership"}
	src := tok.Source()

	got, err := src(withTenantEnv(context.Background(), "sbx", k3.EnvTest), Caller{Tenant: "sbx"})
	if err != nil || got != "test-token-sbx" || p.asked() != "test:sbx" {
		t.Fatalf("a test enterprise: %q %v", got, err)
	}
	got, err = src(withTenantEnv(context.Background(), "acme", k3.EnvLive), Caller{Tenant: "acme"})
	if err != nil || got != "live-token-acme" || p.asked() != "live:acme" {
		t.Fatalf("a real tenant: %q %v", got, err)
	}
	// the env of one tenant's request says nothing of another tenant's call made while serving it
	got, err = src(withTenantEnv(context.Background(), "sbx", k3.EnvTest), Caller{Tenant: "acme"})
	if err != nil || got != "live-token-acme" {
		t.Fatalf("another tenant inside a test enterprise's request: %q %v", got, err)
	}
	p.asked()
	// the app's own work later, with no request behind it: the tenant is remembered as a test enterprise (the token is cached too)
	tok2 := &Tokens{Base: p.srv.URL, ClientID: liveID, ClientSecret: liveSecret, TestClientID: testID, TestClientSecret: testSecret}
	if _, err := tok2.Source()(withTenantEnv(context.Background(), "sbx", k3.EnvTest), Caller{Tenant: "sbx"}); err != nil {
		t.Fatal(err)
	}
	p.asked()
	if env := tok2.envOf(context.Background(), "sbx"); env != k3.EnvTest {
		t.Fatalf("remembered %q", env)
	}
}

// Nobody said which environment the tenant is in (the app's own work after a restart, or a platform older than the env claim): the
// live key is asked first, and the platform's wrong_environment answer sends the app to its test key, which it then keeps for that
// tenant. A real tenant never gets that far.
func TestAnUnknownTenantFallsBackToTheTestKeyOnWrongEnvironment(t *testing.T) {
	p := newEnvPlatform(t, "sbx")
	src := (&Tokens{Base: p.srv.URL, ClientID: liveID, ClientSecret: liveSecret, TestClientID: testID, TestClientSecret: testSecret}).Source()
	got, err := src(context.Background(), Caller{Tenant: "sbx"})
	if err != nil || got != "test-token-sbx" || p.asked() != "live:sbx,test:sbx" {
		t.Fatalf("unknown test enterprise: %q %v", got, err)
	}
	got, err = src(context.Background(), Caller{Tenant: "acme"})
	if err != nil || got != "live-token-acme" || p.asked() != "live:acme" {
		t.Fatalf("unknown real tenant: %q %v", got, err)
	}
	// a tenant the platform called live is not tried with the test key: its refusal stands
	got, err = src(withTenantEnv(context.Background(), "sbx2", k3.EnvLive), Caller{Tenant: "sbx2"})
	if err != nil || got != "live-token-sbx2" {
		t.Fatalf("named live: %q %v", got, err)
	}
}

// Without a test key — neither on Tokens nor in K3_APP_TEST_CLIENT_ID / _SECRET — every tenant gets the live key, as before 0.7.7: a
// test enterprise's call is refused with wrong_environment, a real tenant's succeeds. With the variables set, they are the test key.
func TestNoTestKeyBehavesAsBefore(t *testing.T) {
	t.Setenv("K3_APP_TEST_CLIENT_ID", "")
	t.Setenv("K3_APP_TEST_CLIENT_SECRET", "")
	p := newEnvPlatform(t, "sbx")
	src := (&Tokens{Base: p.srv.URL, ClientID: liveID, ClientSecret: liveSecret}).Source()
	_, err := src(withTenantEnv(context.Background(), "sbx", k3.EnvTest), Caller{Tenant: "sbx"})
	if !raise.IsCode(err, "wrong_environment") || p.asked() != "live:sbx" {
		t.Fatalf("a test enterprise without a test key: %v", err)
	}
	if _, err := src(context.Background(), Caller{Tenant: "sbx"}); !raise.IsCode(err, "wrong_environment") || p.asked() != "live:sbx" {
		t.Fatalf("unknown, without a test key: %v", err)
	}
	if got, err := src(context.Background(), Caller{Tenant: "acme"}); err != nil || got != "live-token-acme" {
		t.Fatalf("a real tenant: %q %v", got, err)
	}
	if _, err := (&Tokens{Base: p.srv.URL}).Source()(context.Background(), Caller{Tenant: "acme"}); err == nil || !strings.Contains(err.Error(), "no app key") {
		t.Fatalf("no key at all: %v", err)
	}

	t.Setenv("K3_APP_TEST_CLIENT_ID", testID)
	t.Setenv("K3_APP_TEST_CLIENT_SECRET", testSecret)
	src = (&Tokens{Base: p.srv.URL, ClientID: liveID, ClientSecret: liveSecret}).Source()
	p.asked()
	if got, err := src(withTenantEnv(context.Background(), "sbx", k3.EnvTest), Caller{Tenant: "sbx"}); err != nil || got != "test-token-sbx" || p.asked() != "test:sbx" {
		t.Fatalf("the test key from the environment: %q %v", got, err)
	}
	// a process with only a test key serves its test enterprises
	src = (&Tokens{Base: p.srv.URL}).Source()
	if got, err := src(withTenantEnv(context.Background(), "sbx", k3.EnvTest), Caller{Tenant: "sbx"}); err != nil || got != "test-token-sbx" {
		t.Fatalf("only a test key: %q %v", got, err)
	}
}

// Inbound reads the tenant's environment from what the platform signed: the assertion of an op (or of any other call of the
// platform's) and the App token of a person's page session. An assertion that is not the platform's, or not for this app, says nothing.
func TestInboundCarriesTheTenantsEnvironment(t *testing.T) {
	plat, _ := k3.NewSigner("plat")
	stranger, _ := k3.NewSigner("plat") // the same kid, another key
	var seen string
	h := Inbound(plat.JWKS(), "membership", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, _ := r.Context().Value(envKey{}).(tenantEnv)
		seen = e.tenant + "/" + e.env
	}))
	exp := time.Now().Add(time.Minute).Unix()
	assertion := func(s *k3.Signer, aud, env string) string {
		tok, _ := s.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Sub: "mia", Aud: k3.Audience{aud}, Exp: exp},
			Tenant: "sbx", Via: k3.ViaDraft, Op: "membership.grant", IdempotencyKey: "k", Env: env})
		return tok
	}
	call := func(path, header, tok string) string {
		seen = ""
		r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.Header.Set(header, tok)
		h.ServeHTTP(httptest.NewRecorder(), r)
		return seen
	}
	if got := call("/k3/apps/membership/ops/membership.grant/execute", k3.HeaderAssertion, assertion(plat, "membership", k3.EnvTest)); got != "sbx/test" {
		t.Fatalf("an op's assertion: %q", got)
	}
	for name, tok := range map[string]string{
		"another app's":         assertion(plat, "order", k3.EnvTest),
		"not the platform's":    assertion(stranger, "membership", k3.EnvTest),
		"an older platform's":   assertion(plat, "membership", ""),
		"a page session as one": func() string { tok, _ := plat.Sign(TypAppToken, AppClaims{Tenant: "sbx", Env: k3.EnvTest}); return tok }(),
	} {
		if got := call("/k3/apps/membership/ops/membership.grant/execute", k3.HeaderAssertion, tok); got != "/" {
			t.Fatalf("%s assertion: %q", name, got)
		}
	}
	page, _ := plat.Sign(TypAppToken, AppClaims{Registered: k3.Registered{Sub: "mia", Aud: k3.Audience{"membership"}, Exp: exp}, Tenant: "sbx", Env: k3.EnvTest})
	if got := call("/members", HeaderToken, page); got != "sbx/test" {
		t.Fatalf("a page session: %q", got)
	}
}
