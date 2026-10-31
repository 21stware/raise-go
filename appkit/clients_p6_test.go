package appkit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The /app/v1 clients the campaign app needed to leave the platform's process (ADR-0130 P6): entitlements, the secret store, the
// tenant's sites and the organisation's stores. The wire is internal/remoteapp's; internal/remoteapp's round trips drive these against
// the platform's own handler.

func TestEntitlementsPathsAndBodies(t *testing.T) {
	var dry []string
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		dry = append(dry, r.Header.Get(HeaderDryRun))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/app/v1/entitlements/grants" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"data":[{"id":"eg_1","holder":"cu_1","remaining":4}]}`))
		case strings.HasSuffix(r.URL.Path, "/granters/m.pkg/grants"):
			_, _ = w.Write([]byte(`{"ref":"k3://membership/package/p1"}`))
		case strings.HasSuffix(r.URL.Path, "/stores/relink"):
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"id":"x1","replay":true}`))
		}
	})
	ents := cl.Entitlements()
	ctx := context.Background()
	check := func(step, method, path, query, body string) {
		t.Helper()
		if s.method != method || s.path != path || s.query != query || s.body != body || s.tenant != "t1" {
			t.Fatalf("%s: %+v\n want %s %s ?%s %s", step, *s, method, path, query, body)
		}
	}

	tp, err := ents.CreateTemplate(ctx, tc, TemplateReq{Kind: EntCount, Name: "五次卡", Params: EntParams{TotalTimes: 5, ValidDays: 30}, Scope: []string{"spa"}})
	if err != nil || tp.ID != "x1" {
		t.Fatalf("create: %+v %v", tp, err)
	}
	check("create template", "POST", "/app/v1/entitlements/templates", "",
		`{"ownerApp":"","kind":"count","name":"五次卡","params":{"total_times":5,"valid_days":30},"scope":["spa"],"transferable":false,"freezable":false,"maxFreezeDays":0,"active":null}`)
	_, err = ents.UpdateTemplate(ctx, tc, "et/1", TemplateReq{Kind: EntVoucher, Name: "券"})
	check("update template", "PUT", "/app/v1/entitlements/templates/et%2F1", "",
		`{"ownerApp":"","kind":"voucher","name":"券","params":{},"scope":null,"transferable":false,"freezable":false,"maxFreezeDays":0,"active":null}`)
	_, err = ents.Template(ctx, tc, "et_1")
	check("template", "GET", "/app/v1/entitlements/templates/et_1", "", "")

	g, err := ents.Grant(ctx, tc, GrantReq{Key: "k:1", Template: "et_1", Holder: "cu_1", PaidCents: 500})
	if err != nil || !g.Replay {
		t.Fatalf("grant: %+v %v", g, err)
	}
	check("grant", "POST", "/app/v1/entitlements/grants", "", `{"key":"k:1","template":"et_1","holder":"cu_1","paidCents":500}`)
	ref, err := ents.GrantVia(ctx, tc, "m.pkg", GrantItem{Key: "o:1", Holder: "cu_1", Params: map[string]any{"package": "p1"}})
	if err != nil || ref != "k3://membership/package/p1" {
		t.Fatalf("grant via: %q %v", ref, err)
	}
	check("grant via", "POST", "/app/v1/entitlements/granters/m.pkg/grants", "", `{"key":"o:1","holder":"cu_1","params":{"package":"p1"}}`)

	gs, err := ents.Grants(ctx, tc, "cu_1", "")
	if err != nil || len(gs) != 1 || gs[0].Remaining != 4 {
		t.Fatalf("grants: %+v %v", gs, err)
	}
	check("grants", "GET", "/app/v1/entitlements/grants", "holder=cu_1", "")
	_, _ = ents.Grants(ctx, tc, "cu_1", "active")
	check("grants active", "GET", "/app/v1/entitlements/grants", "holder=cu_1&status=active", "")
	_, _ = ents.Get(ctx, tc, "eg_1")
	check("get", "GET", "/app/v1/entitlements/grants/eg_1", "", "")

	_, _ = ents.Redeem(ctx, tc, RedeemReq{Key: "r:1", Pick: "eg_1", Scope: []string{"spa"}, Ref: "o1"})
	check("redeem", "POST", "/app/v1/entitlements/redemptions", "", `{"key":"r:1","pick":"eg_1","scope":["spa"],"ref":"o1"}`)
	_, _ = ents.Void(ctx, tc, "rd_1", "mistake")
	check("void", "POST", "/app/v1/entitlements/redemptions/rd_1/void", "", `{"reason":"mistake"}`)

	_, _ = ents.Reserve(ctx, tc, ReserveReq{RedeemReq: RedeemReq{Key: "s:1", Holder: "cu_1"}, TTL: 10 * time.Minute, App: "ignored"})
	check("reserve", "POST", "/app/v1/entitlements/reservations", "", `{"key":"s:1","holder":"cu_1","ttlSeconds":600}`)
	_, _ = ents.Reserve(ctx, tc, ReserveReq{RedeemReq: RedeemReq{Key: "s:2"}})
	check("reserve without ttl", "POST", "/app/v1/entitlements/reservations", "", `{"key":"s:2"}`)
	_, _ = ents.Confirm(ctx, tc, "er_1")
	check("confirm", "POST", "/app/v1/entitlements/reservations/er_1/confirm", "", `{}`)
	_, _ = ents.Cancel(ctx, tc, "er_1", "changed")
	check("cancel", "POST", "/app/v1/entitlements/reservations/er_1/cancel", "", `{"reason":"changed"}`)
	_, _ = ents.Reservation(ctx, tc, "er_1")
	check("reservation", "GET", "/app/v1/entitlements/reservations/er_1", "", "")

	amt := int64(100)
	_, _ = ents.Refund(ctx, tc, EntRefundReq{Grant: "eg_1", Mode: "amount", AmountCents: &amt, Reason: "back"})
	check("refund", "POST", "/app/v1/entitlements/grants/eg_1/refund", "", `{"mode":"amount","amountCents":100,"reason":"back"}`)
	_, _ = ents.Transfer(ctx, tc, TransferReq{Grant: "eg_1", To: "cu_2", Reason: "gift"})
	check("transfer", "POST", "/app/v1/entitlements/grants/eg_1/transfer", "", `{"to":"cu_2","reason":"gift"}`)
	if err := ents.RelinkStore(ctx, tc, "campaigns", "legacy-1", "u_jn1"); err != nil {
		t.Fatalf("relink: %v", err)
	}
	check("relink", "POST", "/app/v1/entitlements/stores/relink", "", `{"from":"legacy-1","to":"u_jn1"}`)

	for i, d := range dry {
		if d != "" {
			t.Fatalf("call %d of a real client sent %s: %q", i, HeaderDryRun, d)
		}
	}

	// a dry client marks every call, reads included, and leaves the real one as it was
	dry = nil
	d := ents.Dry()
	_, _ = d.Grant(ctx, tc, GrantReq{Key: "k:dry"})
	_, _ = d.Get(ctx, tc, "eg_1")
	_, _ = d.GrantVia(ctx, tc, "m.pkg", GrantItem{Key: "o:2"})
	_, _ = ents.Grant(ctx, tc, GrantReq{Key: "k:real"})
	if len(dry) != 4 || dry[0] != "1" || dry[1] != "1" || dry[2] != "1" || dry[3] != "" {
		t.Fatalf("dry-run header per call = %q", dry)
	}
}

func TestEntitlementsRefusalsAndErrors(t *testing.T) {
	ctx := context.Background()
	cl, _ := platform(t, status(422, `{"error":{"code":"invalid","message":"「十次卡」剩余 0 次，不能核销","details":{"refused":true,"items":[{"grant":"eg_2","name":"期卡","reason":"已过期"}]}}}`))
	_, err := cl.Entitlements().Redeem(ctx, tc, RedeemReq{Key: "r"})
	var refused *EntRefused
	if !errors.As(err, &refused) || refused.Message != "「十次卡」剩余 0 次，不能核销" || len(refused.Items) != 1 || refused.Items[0] != (EntRefusal{"eg_2", "期卡", "已过期"}) {
		t.Fatalf("refused: %#v", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a refusal is ErrInvalid: %v", err)
	}

	cl, _ = platform(t, status(422, `{"error":{"code":"invalid","message":"key is required"}}`))
	_, err = cl.Entitlements().Grant(ctx, tc, GrantReq{})
	if !errors.Is(err, ErrInvalid) || errors.As(err, &refused) || !strings.Contains(err.Error(), "key is required") {
		t.Fatalf("plain 422: %v", err)
	}

	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"not found: no app issues m.none"}}`))
	if _, err = cl.Entitlements().GrantVia(ctx, tc, "m.none", GrantItem{Key: "k"}); err != ErrNoGranter {
		t.Fatalf("an unknown granter: %v, want ErrNoGranter", err)
	}
	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"no such package"}}`))
	if _, err = cl.Entitlements().GrantVia(ctx, tc, "m.pkg", GrantItem{Key: "k"}); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoGranter) {
		t.Fatalf("the granter's own 404: %v", err)
	}
	if _, err = cl.Entitlements().Get(ctx, tc, "eg_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	cl, _ = platform(t, status(409, `{"error":{"code":"conflict","message":"the reservation was cancelled"}}`))
	if _, err = cl.Entitlements().Confirm(ctx, tc, "er_1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("confirm after cancel: %v", err)
	}
	cl, _ = platform(t, status(403, `{"error":{"code":"forbidden","message":"another app's grant"}}`))
	if _, err = cl.Entitlements().Refund(ctx, tc, EntRefundReq{Grant: "eg_1", Reason: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another app's grant: %v", err)
	}
}

func TestEntitlementsHasGranter(t *testing.T) {
	cl, s := platform(t, status(200, `{"names":["campaign.prize","membership.package"]}`))
	ents := cl.Entitlements()
	if !ents.HasGranter("membership.package") || ents.HasGranter("membership.none") {
		t.Fatal("HasGranter answers the names the platform lists")
	}
	if s.method != "GET" || s.path != "/app/v1/entitlements/granters" || s.tenant != "" {
		t.Fatalf("the app's own call without a tenant: %+v", *s)
	}
	cl, _ = platform(t, status(500, `{"error":"down"}`))
	if cl.Entitlements().HasGranter("membership.package") {
		t.Fatal("an error reads as no")
	}
	if (Deps{}).EntitlementClient() != nil {
		t.Fatal("no entitlement client when it is not wired")
	}
	if d := (Deps{EntitlementAPI: ents}); d.EntitlementClient() != ents {
		t.Fatal("Deps.EntitlementClient answers EntitlementAPI")
	}
}

func TestVaultCalls(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/v1/secrets/encrypt":
			_, _ = w.Write([]byte(`{"ciphertext":"Y3Q="}`))
		case "/app/v1/secrets/decrypt":
			_, _ = w.Write([]byte(`{"plaintext":"cHQ="}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	v := cl.Vault()
	ctx := context.Background()
	ct, err := v.Encrypt(ctx, "t2", "recipient:f1", []byte("pt"))
	if err != nil || string(ct) != "ct" || s.method != "POST" || s.path != "/app/v1/secrets/encrypt" || s.tenant != "t2" ||
		s.body != `{"plaintext":"cHQ=","subject":"recipient:f1"}` {
		t.Fatalf("encrypt: %q %v %+v", ct, err, *s)
	}
	pt, err := v.Decrypt(ctx, "t2", "recipient:f1", []byte("ct"))
	if err != nil || string(pt) != "pt" || s.path != "/app/v1/secrets/decrypt" || s.body != `{"ciphertext":"Y3Q=","subject":"recipient:f1"}` {
		t.Fatalf("decrypt: %q %v %+v", pt, err, *s)
	}
	if err = v.Shred(ctx, "t2", "recipient:f1"); err != nil || s.path != "/app/v1/secrets/shred" || s.body != `{"subject":"recipient:f1"}` {
		t.Fatalf("shred: %v %+v", err, *s)
	}
}

func TestVaultErrors(t *testing.T) {
	ctx := context.Background()
	cl, _ := platform(t, status(413, `{"error":"the request is too large"}`))
	if _, err := cl.Vault().Encrypt(ctx, "t1", "recipient:big", []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("413: %v, want ErrInvalid", err)
	}
	cl, _ = platform(t, status(409, `{"error":{"code":"conflict","message":"cannot decrypt"}}`))
	if _, err := cl.Vault().Decrypt(ctx, "t1", "recipient:f1", []byte("x")); !errors.Is(err, ErrConflict) {
		t.Fatalf("409: %v, want ErrConflict", err)
	}
	cl, _ = platform(t, status(422, `{"error":{"code":"invalid","message":"subject must be <kind>:<id>"}}`))
	if err := cl.Vault().Shred(ctx, "t1", "nokind"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("422: %v, want ErrInvalid", err)
	}
	cl, _ = platform(t, status(503, `{"error":"no secret store"}`))
	var he *HTTPError
	if _, err := cl.Vault().Encrypt(ctx, "t1", "recipient:f1", nil); !errors.As(err, &he) || he.Status != 503 {
		t.Fatalf("503: %v", err)
	}
	cl, _ = platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderTenant, "other")
		_, _ = w.Write([]byte(`{"plaintext":"cHQ="}`))
	})
	if _, err := cl.Vault().Decrypt(ctx, "t1", "recipient:f1", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an answer about another tenant: %v, want ErrForbidden", err)
	}
}

func TestSitesSummaries(t *testing.T) {
	cl, s := platform(t, status(200, `{"summaries":[{"site":"st_1","name":"官网","requests":7,"state":"live","liveNumber":3},{"site":"st_x","name":"","requests":0}]}`))
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	got, err := cl.Sites().Summaries(context.Background(), tc, []SiteUsageReq{{Site: "st_1", From: from, To: &to}, {Site: "st_x", From: from}})
	if err != nil || len(got) != 2 || got[0].State != SiteLive || got[0].LiveNumber != 3 || got[0].Requests != 7 || got[1].Name != "" {
		t.Fatalf("summaries: %+v %v", got, err)
	}
	if s.method != "POST" || s.path != "/app/v1/sites/summaries" || s.tenant != "t1" ||
		s.body != `{"requests":[{"site":"st_1","from":"2026-10-01T00:00:00Z","to":"2026-10-03T00:00:00Z"},{"site":"st_x","from":"2026-10-01T00:00:00Z"}]}` {
		t.Fatalf("request: %+v", *s)
	}
	if _, err = cl.Sites().Summaries(context.Background(), tc, nil); err != nil || s.body != `{"requests":[]}` {
		t.Fatalf("no requests: %v %s", err, s.body)
	}
	cl, _ = platform(t, status(422, `{"error":{"code":"invalid","message":"at most 200 requests"}}`))
	if _, err = cl.Sites().Summaries(context.Background(), tc, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("422: %v", err)
	}
}

func TestStoresCalls(t *testing.T) {
	calls := 0
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stores":[{"id":"u_jn1","name":"济南一店","path":"/u_sd/u_jn1","lat":36.65,"status":"open"},{"id":"u_jn2","name":"济南二店","path":"/u_sd/u_jn2","status":"closed"}]}`))
	})
	ctx := context.Background()
	got, err := cl.Stores().Stores(ctx, "t1")
	if err != nil || len(got) != 2 || got[0].Lat == nil || *got[0].Lat != 36.65 || !got[0].Selectable() || got[1].Selectable() {
		t.Fatalf("stores: %+v %v", got, err)
	}
	if s.method != "GET" || s.path != "/app/v1/stores" || s.query != "" || s.tenant != "t1" {
		t.Fatalf("stores request: %+v", *s)
	}
	if _, err = cl.Stores().StoresOf(ctx, "t1", "mia"); err != nil || s.query != "of=mia" {
		t.Fatalf("stores of: %v %+v", err, *s)
	}
	n := calls
	got, err = cl.Stores().StoresOf(ctx, "t1", "")
	if err != nil || got == nil || len(got) != 0 || calls != n {
		t.Fatalf("nobody's stores are none, without a call: %+v %v (%d calls)", got, err, calls-n)
	}
	cl, _ = platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderTenant, "other")
		_, _ = w.Write([]byte(`{"stores":[]}`))
	})
	if _, err = cl.Stores().Stores(ctx, "t1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an answer about another tenant: %v", err)
	}
	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"no such tenant"}}`))
	if _, err = cl.Stores().Stores(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
}
