package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The P11 calls served by Serve, with the standard library only (the platform's own callers drive the same in kwork's
// internal/remoteapp appkit_roundtrip_p11_serve_test.go, against remoteapp.Serve).

type p11App struct {
	paid []string
}

func (*p11App) ID() string { return "clubs" }

func (*p11App) Manifest(version, base string) *k3.Manifest {
	m := NewManifest("clubs", "会员", version, base)
	m.Providers = &k3.ProvidersDecl{Holds: true, CustomerAssert: true}
	m.Services = append(m.Services, PaymentService())
	m.Entitlements = &k3.EntitlementsDecl{Grantable: []k3.GrantableDecl{{Name: "clubs.package"}}}
	m.Approvals = map[string]k3.ApprovalKindDecl{"clubs.refund": {Name: "退款", Subject: "k3://clubs/refund/", Vars: []k3.ApprovalVar{}, Template: json.RawMessage(`{}`)}}
	AddOps(m, map[string]k3.Op{"touch": {Entity: "member", Kind: k3.OpUpdate, Risk: "low", Title: "碰", Description: "Touch.", Input: json.RawMessage(`{"type":"object"}`)}})
	return m
}

func (*p11App) Handler(Deps) http.Handler { return http.NotFoundHandler() }

func (a *p11App) Ops(Deps) []Op {
	f := func(context.Context, Caller, json.RawMessage) (OpResult, error) { return OpResult{Summary: "ok"}, nil }
	return []Op{{Name: "touch", DryRun: f, Execute: f}}
}

func (a *p11App) ResourceHolds(Deps) ResourceHolds       { return p11Holds{} }
func (a *p11App) CustomerAsserter(Deps) CustomerAsserter { return p11Asserter{} }
func (a *p11App) Settler(Deps) Settler                   { return p11Settler{a} }
func (a *p11App) ApprovalAnswers(Deps) ApprovalAnswerer  { return p11Answers{} }
func (a *p11App) Grantables(Deps) map[string]Granter {
	return map[string]Granter{"clubs.package": p11Granter{}, "clubs.secret": p11Granter{}}
}

type p11Holds struct{ ResourceHolds }

func (p11Holds) Move(_ context.Context, _ Caller, id string, _, _ time.Time) error {
	if id == "taken" {
		return ErrHoldConflict
	}
	return nil
}

type p11Asserter struct{}

func (p11Asserter) AssertCustomer(_ context.Context, _, phone string) (string, bool, error) {
	if phone == "x" {
		return "", false, ErrBadPhone
	}
	return "", false, &AmbiguousCustomerError{Candidates: []string{"a", "b"}}
}

func (p p11Asserter) AssertCustomerAs(ctx context.Context, tenant, phone, _ string) (string, bool, error) {
	return p.AssertCustomer(ctx, tenant, phone)
}

type p11Settler struct{ a *p11App }

func (s p11Settler) Paid(_ context.Context, o PayOrder) error {
	s.a.paid = append(s.a.paid, o.ID)
	return nil
}
func (p11Settler) Refunded(context.Context, PayOrder, PayRefund) error {
	return errors.New("not yet")
}

type p11Answers struct{}

func (p11Answers) PrepareApproval(_ context.Context, p k3.ApprovalPrepare) (k3.ApprovalPrepared, error) {
	if strings.HasSuffix(p.Subject, "/locked") {
		return k3.ApprovalPrepared{}, Status(http.StatusConflict, "已锁定")
	}
	return k3.ApprovalPrepared{Title: "退款 " + p.Requester}, nil
}

func (p11Answers) ApprovalDecided(_ context.Context, key string, _ k3.ApprovalEvent) (k3.ApprovalAck, error) {
	return k3.ApprovalAck{Message: key}, nil
}

type p11Granter struct{}

func (p11Granter) Grant(_ context.Context, _ Caller, item GrantItem) (string, error) {
	if item.Key == "none" {
		return "", &EntRefused{Message: "没有这个课程包"}
	}
	return "k3://clubs/package/" + item.Key, nil
}

func TestServeP11Calls(t *testing.T) {
	sg, err := k3.NewSigner("p11")
	if err != nil {
		t.Fatal(err)
	}
	a := &p11App{}
	h, err := Serve(a, Deps{}, sg.JWKS(), "1.0.0", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	post := func(path, via, op, tenant, sub, key, body string) (int, string) {
		t.Helper()
		signed := json.RawMessage(body)
		if via == k3.ViaService {
			var run struct {
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal([]byte(body), &run)
			signed = run.Input
		}
		digest, err := k3.InputDigest(signed)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		tok, err := sg.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Iss: "p", Sub: sub, Aud: k3.Audience{"clubs"}, Iat: now.Unix(),
			Exp: now.Add(time.Minute).Unix(), Jti: "j" + path + op}, Tenant: tenant, Via: via, Op: op, InputDigest: digest, IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		req.Header.Set(k3.HeaderAssertion, tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	for _, q := range []struct {
		what, path, via, op, tenant, sub, key, body string
		status                                      int
		answer                                      string
	}{
		{"a taken span", PathHoldsMove, k3.ViaProvider, "", "alpha", "mia", "", `{"holdId":"taken"}`, 409, `{"error":"resource is taken in that time"}`},
		{"a move", PathHoldsMove, k3.ViaProvider, "", "alpha", "mia", "", `{"holdId":"h1"}`, 204, ``},
		{"a move for nobody", PathHoldsMove, k3.ViaProvider, "", "alpha", "platform", "", `{"holdId":"h1"}`, 403, `{"error":"a provider answers for a person"}`},
		{"undeclared holdings are the app's routes'", PathHoldings, k3.ViaProvider, "", "alpha", "mia", "", `{}`, 404, `404 page not found`},
		{"a bad phone", PathCustomerAssert, k3.ViaProvider, "", "alpha", "platform", "", `{"tenant":"alpha","phone":"x"}`, 422, `{"error":"the phone number cannot be read","code":"bad_phone"}`},
		{"an ambiguous phone", PathCustomerAssert, k3.ViaProvider, "", "alpha", "platform", "", `{"tenant":"alpha","phone":"1"}`, 409, `{"error":"the phone number belongs to several customers: ask again with the customer meant (customerId)","code":"ambiguous_customer","candidates":["a","b"]}`},
		{"another tenant", PathCustomerAssert, k3.ViaProvider, "", "alpha", "platform", "", `{"tenant":"beta","phone":"1"}`, 403, `{"error":"another tenant"}`},
		{"a grant", PathGrant, k3.ViaEvent, "", "alpha", "mia", "", `{"caller":{"Tenant":"alpha"},"name":"clubs.package","item":{"key":"k1"}}`, 200, `{"ref":"k3://clubs/package/k1"}`},
		{"a refusal in words", PathGrant, k3.ViaEvent, "", "alpha", "mia", "", `{"caller":{"Tenant":"alpha"},"name":"clubs.package","item":{"key":"none"}}`, 422, `{"error":"没有这个课程包"}`},
		{"undeclared granter", PathGrant, k3.ViaEvent, "", "alpha", "mia", "", `{"caller":{"Tenant":"alpha"},"name":"clubs.secret","item":{"key":"k"}}`, 404, `{"error":"the app does not issue clubs.secret"}`},
		{"no refund", PathGrantRefund, k3.ViaEvent, "", "alpha", "mia", "", `{"caller":{"Tenant":"alpha"},"name":"clubs.package","refund":{"ref":"x"}}`, 422, `{"error":"the granter does not take back what it issued"}`},
		{"no choices", PathGrantChoices, k3.ViaEvent, "", "alpha", "mia", "", `{"caller":{"Tenant":"alpha"},"name":"clubs.package"}`, 200, `{"choices":[]}`},
		{"prepare", "/k3/apps/clubs/approvals/prepare", k3.ViaApproval, k3.ApprovalPrepareOp, "alpha", "mia", "", `{"tenant":"alpha","subject":"k3://clubs/refund/1","requester":"mia"}`, 200, `{"title":"退款 mia"}`},
		{"prepare refused", "/k3/apps/clubs/approvals/prepare", k3.ViaApproval, k3.ApprovalPrepareOp, "alpha", "mia", "", `{"tenant":"alpha","subject":"k3://clubs/refund/locked"}`, 409, `{"error":"已锁定"}`},
		{"prepare with the event's op", "/k3/apps/clubs/approvals/prepare", k3.ViaApproval, k3.ApprovalEventOp, "alpha", "mia", "", `{"tenant":"alpha"}`, 401, `{"error":"the platform's assertion is not valid"}`},
		{"prepare as a provider", "/k3/apps/clubs/approvals/prepare", k3.ViaProvider, k3.ApprovalPrepareOp, "alpha", "mia", "", `{"tenant":"alpha"}`, 401, `{"error":"the platform's assertion is not valid"}`},
		{"prepare, another tenant", "/k3/apps/clubs/approvals/prepare", k3.ViaApproval, k3.ApprovalPrepareOp, "alpha", "mia", "", `{"tenant":"beta"}`, 403, `{"error":"another tenant"}`},
		{"decided, the assertion's key", "/k3/apps/clubs/approvals/decided", k3.ViaApproval, k3.ApprovalEventOp, "alpha", "platform", "k9", `{"id":"a1","tenant":"alpha","result":"approved"}`, 200, `{"message":"k9"}`},
		{"decided, id:result", "/k3/apps/clubs/approvals/decided", k3.ViaApproval, k3.ApprovalEventOp, "alpha", "platform", "", `{"id":"a1","tenant":"alpha","result":"approved"}`, 200, `{"message":"a1:approved"}`},
		{"paid", "/k3/services/payment", k3.ViaService, "", "alpha", "service:payment", "", `{"input":{"kind":"paid","order":{"tenant":"alpha","id":"po1"}}}`, 200, `{"ok":true}`},
		{"refunded, not yet", "/k3/services/payment", k3.ViaService, "", "alpha", "service:payment", "", `{"input":{"kind":"refunded","order":{"tenant":"alpha","id":"po1"},"refund":{"id":"r1"}}}`, 500, `{"error":"not yet"}`},
		{"refunded without its refund", "/k3/services/payment", k3.ViaService, "", "alpha", "service:payment", "", `{"input":{"kind":"refunded","order":{"tenant":"alpha"}}}`, 422, `{"error":"a refund without its refund"}`},
		{"a payment of another tenant", "/k3/services/payment", k3.ViaService, "", "alpha", "service:payment", "", `{"input":{"kind":"paid","order":{"tenant":"beta"}}}`, 403, `{"error":"another tenant"}`},
	} {
		status, answer := post(q.path, q.via, q.op, q.tenant, q.sub, q.key, q.body)
		if status != q.status || answer != q.answer {
			t.Errorf("%s: %d %s, want %d %s", q.what, status, answer, q.status, q.answer)
		}
	}
	if len(a.paid) != 1 || a.paid[0] != "po1" {
		t.Fatalf("the Settler: %v", a.paid)
	}
}
