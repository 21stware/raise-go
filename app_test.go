package raise

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppRootFollowsTheOpenRoot(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.example.com/open/v1":  "https://api.example.com/app/v1",
		"https://api.example.com/open/v1/": "https://api.example.com/app/v1",
		"http://127.0.0.1:8010/open/v1":    "http://127.0.0.1:8010/app/v1",
		"https://api.example.com":          "https://api.example.com/app/v1",
	} {
		if got := appRoot(in); got != want {
			t.Errorf("appRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppAPICallsTheAppRootWithTheTenantToken(t *testing.T) {
	var gotPath, gotAuth, gotKey, gotDry string
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","token_type":"Bearer","expires_in":300,"scope":"app.customers:resolve","tenant":"t1","environment":"test"}`))
	})
	mux.HandleFunc("POST /app/v1/customers/resolve", func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotKey, gotDry = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.URL.Query().Get("dryRun")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cu_1","created":true,"name":"甲"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("cid", "secret", WithBaseURL(srv.URL+"/open/v1"), WithTenant("t1"))
	ref, err := c.App.Customers.Resolve(context.Background(),
		&AppCustomerResolveRequest{Identities: []AppCustomerIdentity{{Kind: "phone", Value: "13800000000"}}},
		&AppCustomersResolveParams{DryRun: "1"}, WithIdempotencyKey("k-1"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Id != "cu_1" || !ref.Created || ref.Name != "甲" {
		t.Errorf("ref = %+v", ref)
	}
	if gotPath != "/app/v1/customers/resolve" || gotAuth != "Bearer tok-1" || gotKey != "k-1" || gotDry != "1" {
		t.Errorf("path %q auth %q key %q dryRun %q", gotPath, gotAuth, gotKey, gotDry)
	}
	if ids, _ := gotBody["identities"].([]any); len(ids) != 1 {
		t.Errorf("body = %v", gotBody)
	}
}

// WithDryRun (generated from the spec's K3-Dry-Run header parameter) sends K3-Dry-Run: 1 on that call only; a call without it sends none.
func TestAppDryRunSendsTheHeader(t *testing.T) {
	var dry []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","token_type":"Bearer","expires_in":300,"scope":"app.entitlements:write","tenant":"t1","environment":"test"}`))
	})
	mux.HandleFunc("POST /app/v1/entitlements/templates", func(w http.ResponseWriter, r *http.Request) {
		dry = append(dry, r.Header.Get("K3-Dry-Run"))
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("a dry run still carries the Idempotency-Key")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tpl_1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("cid", "secret", WithBaseURL(srv.URL+"/open/v1"), WithTenant("t1"))
	ctx := context.Background()
	if _, err := c.App.Entitlements.CreateTemplate(ctx, &AppEntitlementTemplateRequest{}, WithDryRun()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.App.Entitlements.CreateTemplate(ctx, &AppEntitlementTemplateRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(dry) != 2 || dry[0] != "1" || dry[1] != "" {
		t.Errorf("K3-Dry-Run per call = %q, want [1 \"\"]", dry)
	}
}
