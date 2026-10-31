package appkit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

var tc = Caller{Tenant: "t1", User: "u1"}

func TestOptionsLoadSaveDryRun(t *testing.T) {
	var dry []string
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		dry = append(dry, r.Header.Get(HeaderDryRun))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"items":["a","b"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"old":["a"],"now":["a","c"]}`))
	})
	l := OptionList{Key: "candidate-source", Title: "T", MaxLen: 8, Defaults: []string{"a", "b"}}
	items, err := cl.Options().Load(context.Background(), tc, "app", l)
	if err != nil || len(items) != 2 || s.method != "GET" || s.path != "/app/v1/options/candidate-source" || s.query != "default=a&default=b" {
		t.Fatalf("load: %v %v %+v", items, err, *s)
	}
	old, now, err := cl.Options().Save(context.Background(), tc, "app", l, OptionsChange{Add: "c"}, false)
	if err != nil || len(old) != 1 || len(now) != 2 || s.method != "PUT" || s.tenant != "t1" ||
		s.body != `{"title":"T","maxLen":8,"defaults":["a","b"],"add":"c"}` {
		t.Fatalf("save: %v %v %+v", now, err, *s)
	}
	items2 := []string{"x"}
	if _, _, err := cl.Options().Save(context.Background(), tc, "app", l, OptionsChange{Items: &items2}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.body, `"items":["x"]`) {
		t.Fatalf("body %s", s.body)
	}
	if dry[0] != "" || dry[1] != "" || dry[2] != "1" {
		t.Fatalf("dry-run header per call = %q", dry)
	}
}

func TestOptionsErrorMapping(t *testing.T) {
	cl, _ := platform(t, status(422, `{"error":"too long"}`))
	_, _, err := cl.Options().Save(context.Background(), tc, "", OptionList{Key: "k"}, OptionsChange{Add: "x"}, false)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v", err)
	}
}

func TestModelCompleteReadStatus(t *testing.T) {
	cl, s := platform(t, status(200, `{"text":"hi","model":true,"transcribe":false,"serviceModel":"m1"}`))
	m := cl.Model()
	txt, err := m.Complete(context.Background(), tc, "scene_a", "sys", "usr")
	if err != nil || txt != "hi" || s.method != "POST" || s.path != "/app/v1/models/complete" || s.body != `{"scene":"scene_a","system":"sys","user":"usr"}` {
		t.Fatalf("complete: %q %v %+v", txt, err, *s)
	}
	txt, err = m.Read(context.Background(), tc, "scene_a", "", "look", []ModelImage{{MIME: "image/png", Data: []byte("PNG")}})
	if err != nil || txt != "hi" || s.path != "/app/v1/models/read" || s.body != `{"scene":"scene_a","user":"look","images":[{"mime":"image/png","data":"UE5H"}]}` {
		t.Fatalf("read: %q %v %+v", txt, err, *s)
	}
	st, err := m.Status(context.Background(), tc)
	if err != nil || !st.Model || st.Transcribe || st.ServiceModel != "m1" || s.method != "GET" || s.path != "/app/v1/models" {
		t.Fatalf("status: %+v %v %+v", st, err, *s)
	}
}

func TestModelOffAndRefusals(t *testing.T) {
	cl, _ := platform(t, status(503, `{"error":"no model is available","code":"model_off"}`))
	if _, err := cl.Model().Complete(context.Background(), tc, "s", "", "u"); !errors.Is(err, ErrModelOff) {
		t.Fatalf("complete err = %v", err)
	}
	if _, err := cl.Model().Transcribe(context.Background(), tc, []byte("a"), "audio/wav"); !errors.Is(err, ErrModelOff) {
		t.Fatalf("transcribe err = %v", err)
	}
	cl, _ = platform(t, status(429, `{"error":"今天的额度用完了"}`))
	_, err := cl.Model().Read(context.Background(), tc, "s", "", "u", nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 429 || he.Message != "今天的额度用完了" || errors.Is(err, ErrModelOff) {
		t.Fatalf("429 err = %v", err)
	}
	cl, _ = platform(t, status(503, `{"error":"upstream down"}`))
	if _, err := cl.Model().Complete(context.Background(), tc, "s", "", "u"); errors.Is(err, ErrModelOff) {
		t.Fatalf("another 503 must not be ErrModelOff: %v", err)
	}
}

func TestModelTranscribeSendsAudio(t *testing.T) {
	cl, s := platform(t, status(200, `{"text":"hello world"}`))
	txt, err := cl.Model().Transcribe(context.Background(), tc, []byte("RIFFdata"), "audio/wav")
	if err != nil || txt != "hello world" {
		t.Fatalf("%q %v", txt, err)
	}
	if s.method != "POST" || s.path != "/app/v1/models/transcribe" || s.body != "RIFFdata" || s.ctype != "audio/wav" || s.clen != 8 || s.tenant != "t1" {
		t.Fatalf("%+v", *s)
	}
}

func TestPayFlowAndLink(t *testing.T) {
	order := `{"id":"o1","tenant":"t1","channel":"wechat","amountCents":100,"status":"created","payToken":"tok1","link":"https://pay/x","payParams":"{}","expiresAt":"2026-10-31T00:00:00Z","createdAt":"2026-10-31T00:00:00Z","refundedCents":0}`
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/refunds") && r.Method == "GET":
			_, _ = w.Write([]byte(`{"items":[{"id":"r1","orderId":"o1","amountCents":50,"status":"ok","createdAt":"2026-10-31T00:00:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/refunds"):
			_, _ = w.Write([]byte(`{"id":"r1","orderId":"o1","amountCents":50,"status":"ok","createdAt":"2026-10-31T00:00:00Z"}`))
		case r.URL.Path == "/app/v1/pay/orders/o 2":
			_, _ = w.Write([]byte(`{"id":"o2","payToken":"tok2"}`))
		default:
			_, _ = w.Write([]byte(order))
		}
	})
	p := cl.Pay()
	ctx := context.Background()
	o, err := p.Create(ctx, tc, PayCreateReq{Channel: "wechat", AmountCents: 100, Subject: "S", SourceRef: "k3://a/b/1", IdemKey: "k1", TTL: 10 * time.Minute, OpenID: "oid"})
	if err != nil || o.ID != "o1" || o.PayParams != "{}" || s.method != "POST" || s.path != "/app/v1/pay/orders" ||
		s.body != `{"channel":"wechat","amountCents":100,"subject":"S","sourceRef":"k3://a/b/1","key":"k1","ttlSeconds":600,"openId":"oid"}` {
		t.Fatalf("create: %+v %v %+v", o, err, *s)
	}
	if got := p.Link(PayOrder{PayToken: "tok1"}); got != "https://pay/x" {
		t.Fatalf("link after create = %q", got)
	}
	if got := p.Link(PayOrder{PayToken: "tok2"}); got != "" {
		t.Fatalf("link before seen = %q", got)
	}
	if o, err = p.Get(ctx, tc, "o 2"); err != nil || o.ID != "o2" || s.method != "GET" || s.path != "/app/v1/pay/orders/o%202" {
		t.Fatalf("get: %+v %v %+v", o, err, *s)
	}
	if got := p.Link(o); got != "" { // o2 came without a link
		t.Fatalf("link of an order without one = %q", got)
	}
	if _, err = p.Get(ctx, tc, "o1"); err != nil || p.Link(PayOrder{PayToken: "tok1"}) != "https://pay/x" {
		t.Fatalf("get o1: %v", err)
	}
	if _, err = p.Cancel(ctx, tc, "o1"); err != nil || s.method != "POST" || s.path != "/app/v1/pay/orders/o1/cancel" || s.body != `{}` {
		t.Fatalf("cancel: %v %+v", err, *s)
	}
	r, err := p.Refund(ctx, tc, "o1", PayRefundReq{AmountCents: 50, Reason: "x", IdemKey: "rk"})
	if err != nil || r.ID != "r1" || s.path != "/app/v1/pay/orders/o1/refunds" || s.body != `{"amountCents":50,"reason":"x","key":"rk"}` {
		t.Fatalf("refund: %+v %v %+v", r, err, *s)
	}
	rs, err := p.Refunds(ctx, tc, "o1")
	if err != nil || len(rs) != 1 || s.method != "GET" || s.path != "/app/v1/pay/orders/o1/refunds" {
		t.Fatalf("refunds: %+v %v %+v", rs, err, *s)
	}
}

func TestPayErrors(t *testing.T) {
	cl, _ := platform(t, status(409, `{"error":"already paid"}`))
	if _, err := cl.Pay().Cancel(context.Background(), tc, "o1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	cl, _ = platform(t, status(404, `{"error":"no such order"}`))
	if _, err := cl.Pay().Get(context.Background(), tc, "o1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestPayLinksBounded(t *testing.T) {
	p := &payClient{}
	for i := 0; i <= maxPayLinks+5; i++ {
		p.seen(PayOrder{PayToken: "t" + string(rune('a'+i%26)) + strings.Repeat("x", i%7) + time.Duration(i).String(), Link: "l"})
	}
	if len(p.links) > maxPayLinks {
		t.Fatalf("links grew to %d", len(p.links))
	}
}

func TestApprovalsCalls(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/app/v1/approvals/waiting":
			_, _ = w.Write([]byte(`{"items":[{"approval":"a1","subject":"k3://x/y/1","title":"T","task":{"id":"tk1","version":2}}]}`))
		case r.URL.Path == "/app/v1/approvals/participates":
			_, _ = w.Write([]byte(`{"participates":true}`))
		case strings.Contains(r.URL.Path, "/tasks/"):
			_, _ = w.Write([]byte(`{"id":"a1","status":"approved"}`))
		case r.Method == "GET":
			_, _ = w.Write([]byte(`{"id":"a1","status":"running","subject":"k3://x/y/1"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"a1","number":"N1","status":"running"}`))
		}
	})
	a := cl.Approvals()
	ctx := context.Background()
	ref, err := a.Submit(ctx, tc, k3.ApprovalSubmit{Kind: "x.leave", Subject: "k3://x/y/1"})
	if err != nil || ref.ID != "a1" || ref.Number != "N1" || s.method != "POST" || s.path != "/app/v1/approvals" || s.body != `{"kind":"x.leave","subject":"k3://x/y/1"}` {
		t.Fatalf("submit: %+v %v %+v", ref, err, *s)
	}
	if _, err = a.Resubmit(ctx, tc, "a/1", k3.ApprovalSubmit{Kind: "x.leave", Subject: "s"}); err != nil || s.path != "/app/v1/approvals/a%2F1/resubmit" || s.method != "POST" {
		t.Fatalf("resubmit: %v %+v", err, *s)
	}
	if _, err = a.Withdraw(ctx, tc, "a1", "oops"); err != nil || s.path != "/app/v1/approvals/a1/withdraw" || s.body != `{"reason":"oops"}` {
		t.Fatalf("withdraw: %v %+v", err, *s)
	}
	v, err := a.Of(ctx, tc, "k3://x/y/1")
	if err != nil || v == nil || v.ID != "a1" || s.method != "GET" || s.path != "/app/v1/approvals" || s.query != "subject=k3%3A%2F%2Fx%2Fy%2F1" {
		t.Fatalf("of: %+v %v %+v", v, err, *s)
	}
	ws, err := a.Waiting(ctx, tc, "x.leave", 20)
	if err != nil || len(ws) != 1 || ws[0].Task.ID != "tk1" || s.path != "/app/v1/approvals/waiting" || s.query != "kind=x.leave" {
		t.Fatalf("waiting: %+v %v %+v", ws, err, *s)
	}
	v, err = a.Act(ctx, tc, "tk1", k3.ApprovalAct{Action: "approve", Version: 2, Comment: "ok"})
	if err != nil || v.Status != "approved" || s.method != "POST" || s.path != "/app/v1/approvals/tasks/tk1/actions" || s.body != `{"action":"approve","version":2,"comment":"ok"}` {
		t.Fatalf("act: %+v %v %+v", v, err, *s)
	}
	ok, err := a.Participates(ctx, tc, "k3://x/y/1", "u2")
	if err != nil || !ok || s.path != "/app/v1/approvals/participates" || s.query != "subject=k3%3A%2F%2Fx%2Fy%2F1&user=u2" {
		t.Fatalf("participates: %v %v %+v", ok, err, *s)
	}
}

func TestApprovalsOf404AndErrors(t *testing.T) {
	cl, _ := platform(t, status(404, `{"error":"none"}`))
	v, err := cl.Approvals().Of(context.Background(), tc, "k3://x/y/1")
	if v != nil || err != nil {
		t.Fatalf("404 must be nil, nil: %v %v", v, err)
	}
	if _, err = cl.Approvals().Act(context.Background(), tc, "tk", k3.ApprovalAct{Action: "approve"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("act 404 = %v", err)
	}
	cl, _ = platform(t, status(409, `{"error":"stale task"}`))
	if _, err = cl.Approvals().Act(context.Background(), tc, "tk", k3.ApprovalAct{Action: "approve"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("act 409 = %v", err)
	}
	if v, err = cl.Approvals().Of(context.Background(), tc, "s"); v != nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("of 409 = %v %v", v, err)
	}
	cl, _ = platform(t, status(422, `{"error":"bad kind"}`))
	if _, err = cl.Approvals().Submit(context.Background(), tc, k3.ApprovalSubmit{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("submit 422 = %v", err)
	}
}

func TestCalendarPush(t *testing.T) {
	cl, s := platform(t, status(200, `{"result":"cal_1"}`))
	start := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	got, err := cl.CalendarPush(context.Background(), "t1", "k3://a/booking/1", 42, &CalendarProjection{
		Ref: "k3://a/booking/1", Owner: "u1", Start: start, End: start.Add(time.Hour), Title: "Rehab"})
	if err != nil || got != "cal_1" || s.method != "POST" || s.path != "/app/v1/calendar/projections" || s.tenant != "t1" {
		t.Fatalf("%q %v %+v", got, err, *s)
	}
	want := `{"ref":"k3://a/booking/1","version":42,"item":{"ref":"k3://a/booking/1","owner":"u1","start":"2026-11-01T09:00:00Z","end":"2026-11-01T10:00:00Z","title":"Rehab"}}`
	if s.body != want {
		t.Fatalf("body %s", s.body)
	}
	// a cancel carries no item
	if _, err = cl.CalendarPush(context.Background(), "t1", "k3://a/booking/1", 43, nil); err != nil || s.body != `{"ref":"k3://a/booking/1","version":43,"item":null}` {
		t.Fatalf("cancel: %v %s", err, s.body)
	}
	cl, _ = platform(t, status(422, `{"error":"bad ref"}`))
	if _, err = cl.CalendarPush(context.Background(), "t1", "x", 1, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}
