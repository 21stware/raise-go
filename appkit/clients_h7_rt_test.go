package appkit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/21stware/raise-go/k3"
)

// Found by the round trips against the platform's handler (internal/remoteapp/appkit_roundtrip_h7_test.go): the kit's clients answer as
// the platform's own client of the same routes does.

// A key with an empty, "." or ".." segment names another object once a URL resolves the segment away: the client refuses it as the
// platform does, and nothing goes out.
func TestBlobsRefuseDotSegmentKeys(t *testing.T) {
	ctx := context.Background()
	called := false
	cl, _ := platform(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	b := cl.Blobs("t1")
	for _, key := range []string{"apps/a/./x", "apps/a/y/../x", "apps/a//x", "apps/a/"} {
		if err := b.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); !errors.Is(err, ErrInvalid) {
			t.Errorf("put %q: %v", key, err)
		}
		if _, _, err := b.Get(ctx, key); !errors.Is(err, ErrInvalid) {
			t.Errorf("get %q: %v", key, err)
		}
		if _, err := b.Head(ctx, key); !errors.Is(err, ErrInvalid) {
			t.Errorf("head %q: %v", key, err)
		}
		if err := b.Delete(ctx, key); !errors.Is(err, ErrInvalid) {
			t.Errorf("delete %q: %v", key, err)
		}
	}
	if called {
		t.Fatal("a refused key went out")
	}
}

// A page session is a person's, never the agent's: a delegated actor is refused before anything goes out (the agent reads a draft on
// the platform, where it is redacted), as the platform's client of the draft routes does.
func TestDraftsRefuseADelegatedActor(t *testing.T) {
	ctx := context.Background()
	called := false
	cl, _ := platform(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	a := k3.DraftActor{Tenant: "t1", Subject: "u1", Delegated: true, Agent: "agent:kwork"}
	_, err := cl.Drafts().Get(ctx, a, "d_1")
	var de *DraftError
	if !errors.As(err, &de) || de.Status != http.StatusForbidden {
		t.Fatalf("delegated get: %#v", err)
	}
	if _, err := cl.Drafts().List(ctx, a, k3.DraftListFilter{}); !errors.As(err, &de) || de.Status != http.StatusForbidden {
		t.Fatalf("delegated list: %#v", err)
	}
	if called {
		t.Fatal("a delegated actor's call went out")
	}
}

// An answer of /app/v1/pay that stands for a payment refusal matches it (as the platform's own client keeps payments.ErrPending …), and
// still matches the kit's error of its status.
func TestPayKeepsThePaymentRefusals(t *testing.T) {
	ctx := context.Background()
	c := Caller{Tenant: "t1", User: "mia"}
	for _, tc := range []struct {
		status int
		msg    string
		want   error
		kit    func(error) bool
	}{
		{503, "the payment gateway did not answer; try again in a moment", ErrPayPending, func(err error) bool { var he *HTTPError; return errors.As(err, &he) && he.Status == 503 }},
		{409, "collecting money is not set up: the administrator must set up a payment channel first", ErrPayNotConfigured, func(err error) bool { return errors.Is(err, ErrConflict) }},
		{409, "this payment channel is switched off", ErrPayDisabled, func(err error) bool { return errors.Is(err, ErrConflict) }},
		{409, "the order is paid", nil, func(err error) bool { return errors.Is(err, ErrConflict) }},
	} {
		cl, _ := platform(t, status(tc.status, `{"error":{"message":"`+tc.msg+`"}}`))
		_, err := cl.Pay().Create(ctx, c, PayCreateReq{Subject: "x", IdemKey: "k"})
		if !tc.kit(err) {
			t.Errorf("%d %q: the kit's error is lost: %#v", tc.status, tc.msg, err)
		}
		for _, s := range []error{ErrPayPending, ErrPayNotConfigured, ErrPayDisabled} {
			if errors.Is(err, s) != (s == tc.want) {
				t.Errorf("%d %q: errors.Is(%v) = %v", tc.status, tc.msg, s, errors.Is(err, s))
			}
		}
	}
	cl, _ := platform(t, status(502, `{"error":{"message":"the payment gateway refused: 余额不足"}}`))
	_, err := cl.Pay().Refund(ctx, c, "po_1", PayRefundReq{IdemKey: "k"})
	var ge *PayGatewayError
	if !errors.As(err, &ge) || ge.Message != "余额不足" {
		t.Fatalf("gateway: %#v", err)
	}
}
