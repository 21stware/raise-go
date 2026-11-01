package appkit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/21stware/raise-go/provide"
)

// The /app/v1 clients the membership and order apps need to leave the platform's process (ADR-0130 P11 S3): the whole customer centre,
// the entitlement calls beyond H4, the org chart, the other apps' providers, the flat access lists and the deal handover. The wire is
// internal/remoteapp's; internal/remoteapp's appkit round trips (appkit_roundtrip_p11_test.go) drive these against the platform's own
// handler.

func TestCustomerCentrePathsAndBodies(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`{"items":null}`))
		case strings.HasSuffix(r.URL.Path, "/merge"):
			_, _ = w.Write([]byte(`{"primary":"cu_a"}`))
		case r.URL.Path == "/app/v1/customers" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"total":0}`))
		default:
			_, _ = w.Write([]byte(`{"id":"cu_a","name":"甲"}`))
		}
	})
	cc := cl.CustomerCentre()
	ctx := context.Background()
	check := func(step, method, path, query, body string) {
		t.Helper()
		if s.method != method || s.path != path || s.query != query || s.body != body || s.tenant != "t1" {
			t.Fatalf("%s: %+v\n want %s %s ?%s %s", step, *s, method, path, query, body)
		}
	}
	v, err := cc.Find(ctx, tc, IdentPhone, "13800000000")
	if err != nil || v.ID != "cu_a" {
		t.Fatalf("find: %+v %v", v, err)
	}
	check("find", "GET", "/app/v1/customers/find", "kind=phone&value=13800000000", "")
	p, err := cc.List(ctx, tc, CustomerQuery{})
	if err != nil || p.Items == nil || len(p.Items) != 0 {
		t.Fatalf("an empty list is an empty slice: %+v %v", p, err)
	}
	check("list", "GET", "/app/v1/customers", "", "")
	_, _ = cc.List(ctx, tc, CustomerQuery{Q: "乙", Stage: CustomerLead, Tag: "t", Owner: "bob", Source: "crm", Hint: true, WeComPending: true, Limit: 5, Offset: 10})
	check("list filtered", "GET", "/app/v1/customers", "hint=true&limit=5&offset=10&owner=bob&q=%E4%B9%99&source=crm&stage=lead&tag=t&wecomPending=true", "")
	ts, err := cc.Timeline(ctx, tc, "cu/a", 0, 0)
	if err != nil || ts == nil || len(ts) != 0 {
		t.Fatalf("an empty timeline is an empty slice: %+v %v", ts, err)
	}
	check("timeline", "GET", "/app/v1/customers/cu%2Fa/timeline", "", "")
	_, _ = cc.Timeline(ctx, tc, "cu_a", 2, 17)
	check("timeline paged", "GET", "/app/v1/customers/cu_a/timeline", "before=17&limit=2", "")
	_ = cc.Rename(ctx, tc, "cu_a", "甲二")
	check("rename", "POST", "/app/v1/customers/cu_a/rename", "", `{"name":"甲二"}`)
	_ = cc.Link(ctx, tc, "cu_a", "k3://order/o/1")
	check("link", "POST", "/app/v1/customers/cu_a/links", "", `{"ref":"k3://order/o/1"}`)
	_ = cc.SetOwner(ctx, tc, "cu_a", "mia", "u_sales")
	check("set owner", "POST", "/app/v1/customers/cu_a/owner", "", `{"owner":"mia","ownerUnit":"u_sales"}`)
	_ = cc.MarkDealt(ctx, tc, "cu_a", time.Date(2026, 10, 29, 14, 30, 0, 0, time.FixedZone("CST", 8*3600)))
	check("mark dealt", "POST", "/app/v1/customers/cu_a/dealt", "", `{"at":"2026-10-29T06:30:00Z"}`)
	_ = cc.MarkDealt(ctx, tc, "cu_a", time.Time{})
	check("mark dealt now", "POST", "/app/v1/customers/cu_a/dealt", "", `{}`)
	pr, err := cc.Merge(ctx, tc, "cu_b", "cu_a")
	if err != nil || pr != "cu_a" {
		t.Fatalf("merge: %q %v", pr, err)
	}
	check("merge", "POST", "/app/v1/customers/cu_b/merge", "", `{"other":"cu_a"}`)
	_ = cc.Erase(ctx, tc, "cu_a")
	check("erase", "POST", "/app/v1/customers/cu_a/erase", "", `{}`)

	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"not found"}}`))
	if v, err := cl.CustomerCentre().Find(ctx, tc, IdentPhone, "1"); v != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("find missing: %+v %v", v, err)
	}
	if ts, err := cl.CustomerCentre().Timeline(ctx, tc, "cu_x", 0, 0); ts != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("timeline missing: %+v %v", ts, err)
	}
	cl, _ = platform(t, status(403, `{"error":{"code":"forbidden","message":"no customer.read"}}`))
	if _, err := cl.CustomerCentre().List(ctx, tc, CustomerQuery{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("list refused: %v", err)
	}

	if (Deps{}).CustomerCentre() != nil {
		t.Fatal("a customer centre with nothing wired")
	}
	if (Deps{CustomerAPI: cl.Customers()}).CustomerCentre() == nil {
		t.Fatal("Client.Customers is the whole centre")
	}
	if (Deps{CustomerAPI: narrowCustomers{}}).CustomerCentre() != nil {
		t.Fatal("a CustomerClient that is not the whole centre")
	}
}

type narrowCustomers struct{ CustomerClient }

func TestEntitlementCentreCalls(t *testing.T) {
	var dry []string
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		dry = append(dry, r.Header.Get(HeaderDryRun))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/app/v1/entitlements/granters":
			_, _ = w.Write([]byte(`{"names":null}`))
		case strings.HasSuffix(r.URL.Path, "/choices"):
			_, _ = w.Write([]byte(`{"data":[{"param":"package","value":"p20","label":"20 次","priceCents":200000}]}`))
		case strings.HasSuffix(r.URL.Path, "/refunds"):
			_, _ = w.Write([]byte(`{"amountCents":1500,"effect":"退回"}`))
		case r.URL.Path == "/app/v1/entitlements/templates":
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			_, _ = w.Write([]byte(`{"id":"eg_1","code":"AB12"}`))
		}
	})
	ec := cl.EntitlementCentre()
	ctx := context.Background()
	check := func(step, method, path, query, body string) {
		t.Helper()
		if s.method != method || s.path != path || s.query != query || s.body != body || s.tenant != "t1" {
			t.Fatalf("%s: %+v\n want %s %s ?%s %s", step, *s, method, path, query, body)
		}
	}
	names, err := ec.GranterNames(ctx, tc)
	if err != nil || names == nil || len(names) != 0 {
		t.Fatalf("names: %v %v", names, err)
	}
	check("granter names", "GET", "/app/v1/entitlements/granters", "", "")
	ch, err := ec.GranterChoices(ctx, tc, "m.pkg")
	if err != nil || len(ch) != 1 || ch[0].PriceCents != 200000 {
		t.Fatalf("choices: %+v %v", ch, err)
	}
	check("choices", "GET", "/app/v1/entitlements/granters/m.pkg/choices", "", "")
	amt := int64(900)
	res, err := ec.GranterRefund(ctx, tc, "m.pkg", GranterRefund{Key: "rf:1", Ref: "k3://membership/package/p1", AmountCents: &amt, Reason: "退"})
	if err != nil || res.AmountCents != 1500 {
		t.Fatalf("refund: %+v %v", res, err)
	}
	check("granter refund", "POST", "/app/v1/entitlements/granters/m.pkg/refunds", "", `{"key":"rf:1","ref":"k3://membership/package/p1","amountCents":900,"reason":"退"}`)
	ts, err := ec.Templates(ctx, tc, TemplateFilter{})
	if err != nil || ts == nil || len(ts) != 0 {
		t.Fatalf("templates: %v %v", ts, err)
	}
	check("templates", "GET", "/app/v1/entitlements/templates", "", "")
	_, _ = ec.Templates(ctx, tc, TemplateFilter{OwnerApp: "membership", ActiveOnly: true})
	check("templates filtered", "GET", "/app/v1/entitlements/templates", "activeOnly=true&ownerApp=membership", "")
	g, err := ec.ByCode(ctx, tc, "AB 12")
	if err != nil || g.ID != "eg_1" {
		t.Fatalf("by code: %+v %v", g, err)
	}
	check("by code", "GET", "/app/v1/entitlements/codes/AB%2012", "", "")
	for i, d := range dry {
		if d != "" {
			t.Fatalf("call %d of a real client sent %s", i, HeaderDryRun)
		}
	}

	// a dry client previews a refund and marks the call; it is a centre too
	dry = nil
	d, ok := ec.Dry().(EntitlementCentre)
	if !ok {
		t.Fatal("the dry client is not an EntitlementCentre")
	}
	_, _ = d.GranterRefund(ctx, tc, "m.pkg", GranterRefund{Ref: "k3://membership/package/p1"})
	check("dry refund", "POST", "/app/v1/entitlements/granters/m.pkg/refunds", "", `{"preview":true,"ref":"k3://membership/package/p1"}`)
	if len(dry) != 1 || dry[0] != "1" {
		t.Fatalf("dry header: %q", dry)
	}

	// the platform's words for "nobody issues that" and "it does not take back"
	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"not found: no app issues m.none"}}`))
	if _, err := cl.EntitlementCentre().GranterRefund(ctx, tc, "m.none", GranterRefund{Ref: "x"}); !errors.Is(err, ErrNoGranter) || errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown granter: %v", err)
	}
	cl, _ = platform(t, status(409, `{"error":{"code":"conflict","message":"the granter does not take back what it issued"}}`))
	if _, err := cl.EntitlementCentre().GranterRefund(ctx, tc, "m.pkg", GranterRefund{Ref: "x"}); err != ErrNotRefundable {
		t.Fatalf("not refundable: %v", err)
	}
	cl, _ = platform(t, status(409, `{"error":{"code":"conflict","message":"already refunded"}}`))
	if _, err := cl.EntitlementCentre().GranterRefund(ctx, tc, "m.pkg", GranterRefund{Ref: "x"}); !errors.Is(err, ErrConflict) || errors.Is(err, ErrNotRefundable) {
		t.Fatalf("another conflict: %v", err)
	}
	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"not found"}}`))
	if _, err := cl.EntitlementCentre().ByCode(ctx, tc, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown code: %v", err)
	}

	if (Deps{}).EntitlementCentre() != nil || (Deps{EntitlementAPI: cl.Entitlements()}).EntitlementCentre() == nil {
		t.Fatal("Deps.EntitlementCentre")
	}
}

func TestOrgCalls(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/v1/org/units":
			_, _ = w.Write([]byte(`{"items":[{"id":"u_root","name":"总部","path":"/u_root/"}]}`))
		case "/app/v1/org/roles":
			_, _ = w.Write([]byte(`{"items":[{"id":"r_mgr","name":"店长"}]}`))
		default:
			_, _ = w.Write([]byte(`{"units":["u_sales","u_root"],"roles":["r_mgr"]}`))
		}
	})
	o := cl.Org()
	ctx := context.Background()
	us, err := o.Units(ctx, "t1")
	if err != nil || len(us) != 1 || us[0].Path != "/u_root/" || s.path != "/app/v1/org/units" || s.method != "GET" || s.tenant != "t1" {
		t.Fatalf("units: %+v %v %+v", us, err, *s)
	}
	rs, err := o.Roles(ctx, "t1")
	if err != nil || len(rs) != 1 || s.path != "/app/v1/org/roles" {
		t.Fatalf("roles: %+v %v %+v", rs, err, *s)
	}
	pu, pr, err := o.Placement(ctx, "t1", "mia")
	if err != nil || len(pu) != 2 || pr[0] != "r_mgr" || s.path != "/app/v1/org/placement" || s.query != "user=mia" {
		t.Fatalf("placement: %v %v %v %+v", pu, pr, err, *s)
	}
	// an answer for another tenant is refused
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderTenant, "other")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	other := &Client{Base: srv.URL, Token: func(context.Context, Caller) (string, error) { return "tok", nil }}
	if _, err := other.Org().Units(ctx, "t1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another tenant's answer: %v", err)
	}
	cl, _ = platform(t, status(503, `{"error":{"code":"unavailable","message":"org not wired"}}`))
	var he *HTTPError
	if _, err := cl.Org().Units(ctx, "t1"); !errors.As(err, &he) || he.Status != 503 {
		t.Fatalf("not wired: %v", err)
	}
}

func TestFlatGrantsCalls(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case "GET":
			_, _ = w.Write([]byte(`{"grants":null}`))
		case "POST":
			_, _ = w.Write([]byte(`{"kind":"user","principal":"bob","level":"edit","createdBy":"ann","createdAt":"2026-11-01T00:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	f := cl.Flat()
	ctx := context.Background()
	gs, err := f.Grants(ctx, "t1", "member.note", "n/1")
	if err != nil || gs == nil || len(gs) != 0 || s.method != "GET" || s.path != "/app/v1/access/member.note/n%2F1" || s.tenant != "t1" {
		t.Fatalf("grants: %+v %v %+v", gs, err, *s)
	}
	exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	g, err := f.Put(ctx, "t1", "member.note", "n1", FlatGrant{Kind: "user", Principal: "bob", Level: "edit", Note: "会诊", ExpiresAt: &exp, By: "ann"})
	if err != nil || g.By != "ann" || s.path != "/app/v1/access/member.note/n1/grants" ||
		s.body != `{"by":"ann","expiresAt":"2026-12-01T00:00:00Z","kind":"user","level":"edit","name":"","note":"会诊","principal":"bob"}` {
		t.Fatalf("put: %+v %v %+v", g, err, *s)
	}
	ok, err := f.Revoke(ctx, "t1", "member.note", "n1", "role", "platform:tenant.member")
	if err != nil || !ok || s.method != "DELETE" || s.path != "/app/v1/access/member.note/n1/grants/role/platform:tenant.member" {
		t.Fatalf("revoke: %v %v %+v", ok, err, *s)
	}
	_, _ = f.Revoke(ctx, "t1", "member.note", "n1", "everyone", "")
	if s.path != "/app/v1/access/member.note/n1/grants/everyone/-" {
		t.Fatalf("revoke everyone: %+v", *s)
	}
	if err := f.Drop(ctx, "t1", "member.note", "n1"); err != nil || s.method != "DELETE" || s.path != "/app/v1/access/member.note/n1" {
		t.Fatalf("drop: %v %+v", err, *s)
	}
	// 404: never registered, nothing to revoke, nothing to drop
	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"not found"}}`))
	if gs, err := cl.Flat().Grants(ctx, "t1", "member.note", "n9"); err != nil || gs == nil || len(gs) != 0 {
		t.Fatalf("grants of nothing: %v %v", gs, err)
	}
	if ok, err := cl.Flat().Revoke(ctx, "t1", "member.note", "n9", "user", "bob"); ok || err != nil {
		t.Fatalf("revoke nothing: %v %v", ok, err)
	}
	if err := cl.Flat().Drop(ctx, "t1", "member.note", "n9"); err != nil {
		t.Fatalf("drop nothing: %v", err)
	}
	cl, _ = platform(t, status(422, `{"error":{"code":"invalid","message":"bad level"}}`))
	if _, err := cl.Flat().Put(ctx, "t1", "member.note", "n1", FlatGrant{Kind: "user", Principal: "bob", Level: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad level: %v", err)
	}
}

func TestPeersLookupsAndCalls(t *testing.T) {
	avail := true
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/v1/peers/desk":
			_, _ = io.WriteString(w, `{"available":`+map[bool]string{true: "true", false: "false"}[avail]+`,"redeem":true,"void":false}`)
		case "/app/v1/peers/till":
			_, _ = io.WriteString(w, `{"available":`+map[bool]string{true: "true", false: "false"}[avail]+`,"sell":true}`)
		case "/app/v1/peers/holds", "/app/v1/peers/referral-codes":
			_, _ = io.WriteString(w, `{"available":`+map[bool]string{true: "true", false: "false"}[avail]+`}`)
		case "/app/v1/peers/holds/holdable", "/app/v1/peers/holds/free", "/app/v1/peers/holds/hold":
			_, _ = io.WriteString(w, `{"items":[{"item":"r1","id":"r1"}]}`)
		case "/app/v1/peers/holds/move":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"hold_conflict","message":"resource is taken in that time"}}`)
		case "/app/v1/peers/holds/release":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"no_provider","message":"no installed app offers resource holds"}}`)
		case "/app/v1/peers/referral-codes/code":
			_, _ = io.WriteString(w, `{"code":"AB3DE9","status":"active"}`)
		default:
			_, _ = io.WriteString(w, `{"customer":"cu_7","user":"bob"}`)
		}
	})
	ctx := context.Background()
	p := (Deps{PeersAPI: cl.Peers()}).Peers()
	d, ok := p.Desk(ctx, "t1")
	if !ok || s.path != "/app/v1/peers/desk" || s.method != "GET" || s.tenant != "t1" {
		t.Fatalf("desk lookup: %v %+v", ok, *s)
	}
	if r, v := d.DeskAccess(ctx, tc); !r || v {
		t.Fatalf("desk access: %v %v", r, v)
	}
	tl, ok := p.Till(ctx, "t1")
	if !ok || !tl.TillAccess(ctx, tc) || s.path != "/app/v1/peers/till" {
		t.Fatalf("till: %v %+v", ok, *s)
	}
	h, err := p.Holds(ctx, "t1")
	if err != nil || h == nil {
		t.Fatalf("holds: %v", err)
	}
	from, to := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	if _, err := h.Free(ctx, tc, []string{"r1"}, from, to, "k3://x/b/0"); err != nil || s.method != "POST" || s.path != "/app/v1/peers/holds/free" ||
		s.body != `{"items":["r1"],"from":"2026-11-02T09:00:00Z","to":"2026-11-02T10:00:00Z","except":"k3://x/b/0"}` {
		t.Fatalf("free: %v %+v", err, *s)
	}
	if _, err := h.Hold(ctx, tc, provide.HoldRequest{Source: "k3://x/b/1", Title: "诊疗", Items: []string{"r1"}, From: from, To: to}); err != nil ||
		s.body != `{"source":"k3://x/b/1","title":"诊疗","items":["r1"],"from":"2026-11-02T09:00:00Z","to":"2026-11-02T10:00:00Z"}` {
		t.Fatalf("hold: %v %+v", err, *s)
	}
	if err := h.Move(ctx, tc, "h1", from, to); err != provide.ErrHoldConflict || s.body != `{"holdId":"h1","from":"2026-11-02T09:00:00Z","to":"2026-11-02T10:00:00Z"}` {
		t.Fatalf("move: %v %+v", err, *s)
	}
	if err := h.Release(ctx, tc, "h1", ""); err != ErrNoResourceApp || s.body != `{"holdId":"h1"}` {
		t.Fatalf("release: %v %+v", err, *s)
	}
	rc := p.ReferralCodesFor(ctx, "t1")
	code, err := rc.CodeOf(ctx, tc, Promoter{Customer: "cu_1"}, "https://a.example.com")
	if err != nil || code.Code != "AB3DE9" || s.path != "/app/v1/peers/referral-codes/code" || s.body != `{"promoter":{"customer":"cu_1"},"origin":"https://a.example.com"}` {
		t.Fatalf("code of: %+v %v %+v", code, err, *s)
	}
	pr, err := rc.PromoterOf(ctx, tc, "AB3DE9")
	if err != nil || pr.User != "bob" || s.body != `{"code":"AB3DE9"}` {
		t.Fatalf("promoter of: %+v %v %+v", pr, err, *s)
	}

	// none installed: no desk, no till, ErrNoResourceApp, no codes
	avail = false
	if _, ok := p.Desk(ctx, "t1"); ok {
		t.Fatal("a desk that is not there")
	}
	if _, ok := p.Till(ctx, "t1"); ok {
		t.Fatal("a till that is not there")
	}
	if _, err := p.Holds(ctx, "t1"); err != ErrNoResourceApp {
		t.Fatalf("holds not there: %v", err)
	}
	if p.ReferralCodesFor(ctx, "t1") != nil {
		t.Fatal("codes that are not there")
	}
	// nothing wired
	np := (Deps{}).Peers()
	if _, ok := np.Desk(ctx, "t1"); ok {
		t.Fatal("a desk with nothing wired")
	}
	if _, err := np.Holds(ctx, "t1"); !errors.Is(err, ErrNoResourceApp) {
		t.Fatalf("holds with nothing wired: %v", err)
	}
}

func TestPeersHandOverThePagesRequest(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/v1/peers/desk":
			_, _ = io.WriteString(w, `{"available":true}`)
		case "/app/v1/peers/desk/http":
			_, _ = io.WriteString(w, `{"status":201,"contentType":"application/vnd.desk+json","body":"eyJvayI6dHJ1ZX0="}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no installed app serves it"}}`)
		}
	})
	ctx := context.Background()
	d, ok := cl.Peers().Desk(ctx, "t1")
	if !ok {
		t.Fatal("no desk")
	}
	h := d.DeskHandler()
	serve := func(ctx context.Context, c *Caller, method, target, body string) *httptest.ResponseRecorder {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, rd).WithContext(ctx)
		if c != nil {
			req.Header.Set(HeaderTenant, c.Tenant)
			req.Header.Set(HeaderUser, c.User)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	page := WithPersonToken(ctx, "person-token", "t1")
	rec := serve(page, &tc, "POST", "/api/desk/redeem?store=s1", `{"code":"C1"}`)
	if rec.Code != 201 || rec.Header().Get("Content-Type") != "application/vnd.desk+json" || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("handed over: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if s.method != "POST" || s.path != "/app/v1/peers/desk/http" || s.body != `{"method":"POST","path":"/api/desk/redeem","query":"store=s1","body":{"code":"C1"}}` {
		t.Fatalf("the hand-over: %+v", *s)
	}
	if rec := serve(ctx, &tc, "GET", "/api/desk/recent", ""); rec.Code != 403 {
		t.Fatalf("without the person's token: %d", rec.Code)
	}
	if rec := serve(WithPersonToken(ctx, "x", "t2"), &tc, "GET", "/api/desk/recent", ""); rec.Code != 403 {
		t.Fatalf("another tenant's token: %d", rec.Code)
	}
	if rec := serve(page, nil, "GET", "/api/desk/recent", ""); rec.Code != 401 {
		t.Fatalf("no caller: %d", rec.Code)
	}
	if rec := serve(page, &tc, "POST", "/api/desk/redeem", `{nope`); rec.Code != 400 {
		t.Fatalf("not JSON: %d", rec.Code)
	}
	// the platform's 404: no installed app serves it
	gone, _ := platform(t, status(404, `{"error":{"code":"not_found","message":"no installed app serves it"}}`))
	h = remoteDesk{gone}.DeskHandler()
	if rec := serve(page, &tc, "GET", "/api/desk/recent", ""); rec.Code != 404 {
		t.Fatalf("no provider: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDealsHandOverWithoutTheAPI(t *testing.T) {
	cl, s := platform(t, status(200, `{"key":"k","queued":2}`))
	ev := DealEvent{Kind: DealSale, Source: DealSource{App: "order", Ref: "k3://order/o/1"}, Amount: 100, At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	ev.Key = DealKey(ev.Source.App, ev.Source.Ref, ev.Kind)
	if err := cl.Deals(context.Background(), tc, ev); err != nil || s.method != "POST" || s.path != "/app/v1/deals" || !strings.Contains(s.body, `"key":"order:k3://order/o/1:sale"`) {
		t.Fatalf("deals: %v %+v", err, *s)
	}
	cl, _ = platform(t, status(403, `{"error":{"code":"forbidden","message":"another app's deal"}}`))
	if err := cl.Deals(context.Background(), tc, ev); !errors.Is(err, ErrForbidden) || !refused(err) {
		t.Fatalf("refused: %v", err)
	}
	if DealHandoverKey("a:b:sale") != "deal:a:b:sale|platform" {
		t.Fatal("DealHandoverKey")
	}
	// without DealsAPI an app's deal events go nowhere: nothing is queued, nothing checked (no transaction is touched)
	d := Deps{}
	if d.SellsDeals() {
		t.Fatal("sells deals with nothing wired")
	}
	if keys, err := d.EnqueueDeal(context.Background(), nil, tc, DealEvent{Kind: "gift"}); keys != nil || err != nil {
		t.Fatalf("enqueue with nothing wired: %v %v", keys, err)
	}
	// with it, an invalid event is refused before the outbox is touched
	d.DealsAPI = cl.Deals
	if !d.SellsDeals() {
		t.Fatal("DealsAPI sells deals")
	}
	if _, err := d.EnqueueDeal(context.Background(), nil, tc, DealEvent{Kind: "gift"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invalid event: %v", err)
	}
	if _, err := d.EnqueueDeal(context.Background(), nil, Caller{}, ev); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no tenant: %v", err)
	}
	if o := NewOutbox(d); o.DealsAPI == nil {
		t.Fatal("NewOutbox drops DealsAPI")
	}
}

func TestApprovalsWaitingLimitAndTheAppsOwnToken(t *testing.T) {
	var toks []string
	cl, s := platform(t, status(200, `{"items":[]}`))
	cl.Token = func(ctx context.Context, c Caller) (string, error) {
		if p := personToken(ctx); p.token != "" && p.tenant == c.Tenant {
			toks = append(toks, p.token)
			return p.token, nil
		}
		toks = append(toks, "app")
		return "app", nil
	}
	a := cl.Approvals()
	page := WithPersonToken(context.Background(), "mia-session", "t1")
	if _, err := a.Waiting(page, tc, "refund", 5); err != nil || s.query != "kind=refund&limit=5" {
		t.Fatalf("waiting 5: %v %+v", err, *s)
	}
	if _, _ = a.Waiting(page, tc, "refund", 0); s.query != "kind=refund" {
		t.Fatalf("waiting 0: %+v", *s)
	}
	// the app's own call (no person) goes with the app's token even inside the person's request
	_, _ = a.Withdraw(page, Caller{Tenant: "t1"}, "ap_1", "x")
	if strings.Join(toks, ",") != "mia-session,mia-session,app" {
		t.Fatalf("tokens: %v", toks)
	}
}
