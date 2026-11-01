package appkit

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/21stware/raise-go/k3"
)

// What an app issues through the entitlement service (ADR-0130 §3.4, the manifest's entitlements.grantable): the entitlement service
// calls it with the platform's assertion (via event: at least once, keyed by the item's or the refund's key). The platform's side is
// its internal/providers/remote_granter.go.
//
//	POST /k3/entitlements/grant    {caller, name, item}     → {"ref": …}                  422 a refusal in words · else retried
//	POST /k3/entitlements/refund   {caller, name, refund}   → {"amountCents", "effect"}   422 a refusal in words
//	POST /k3/entitlements/choices  {caller, name}           → {"choices": [GranterChoice]}
const (
	PathGrant        = "/k3/entitlements/grant"
	PathGrantRefund  = "/k3/entitlements/refund"
	PathGrantChoices = "/k3/entitlements/choices"
)

// Granter is implemented by an app that can issue something ("membership.package": a course package) and declares it in its
// manifest (entitlements.grantable). It is called at least once, so it answers the same Key with the same reference (k3://…) and
// writes nothing the second time. An item with Dry set is a preview: it runs and answers, and nothing stays. A refusal a person reads
// is an *EntRefused (or an ErrInvalid / ErrConflict / ErrForbidden with words): 422 with the words.
type Granter interface {
	Grant(ctx context.Context, c Caller, item GrantItem) (ref string, err error)
}

// GranterCatalog is implemented by a Granter that can list what it issues.
type GranterCatalog interface {
	Choices(ctx context.Context, c Caller) ([]GranterChoice, error)
}

// GranterRefunder is implemented by a Granter whose issued things an order refunds (订单代收).
type GranterRefunder interface {
	RefundGranted(ctx context.Context, c Caller, req GranterRefund) (GranterRefundResult, error)
}

// Granting offers what the app issues through the entitlement service, by name ("<app>.<what>"); only the names the manifest's
// entitlements.grantable declares are answered.
type Granting interface {
	Grantables(d Deps) map[string]Granter
}

// grantCall is the body of the three calls (the platform's providers.GrantCall).
type grantCall struct {
	Caller Caller         `json:"caller"`
	Name   string         `json:"name"`
	Item   *GrantItem     `json:"item,omitempty"`
	Refund *GranterRefund `json:"refund,omitempty"`
}

// serveGranters answers grant, refund and choices of the app's own Granters, for the tenant of the platform's assertion.
func serveGranters(mux *http.ServeMux, a App, m *k3.Manifest, d Deps) {
	p, ok := a.(Granting)
	if !ok || m.Entitlements == nil {
		return
	}
	gs := p.Grantables(d)
	if len(gs) == 0 {
		return
	}
	find := func(w http.ResponseWriter, r *http.Request) (grantCall, Granter, bool) {
		var in grantCall
		if !platformJSON(w, r, k3.ViaEvent, &in) || !sameTenant(w, r, in.Caller.Tenant) {
			return in, nil, false
		}
		g, ok := gs[in.Name]
		if _, declared := m.Grantable(in.Name); !ok || g == nil || !declared {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "the app does not issue " + in.Name})
			return in, nil, false
		}
		return in, g, true
	}
	fail := func(w http.ResponseWriter, err error) {
		var refused *EntRefused
		if errors.As(err, &refused) || refusal(err) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": refusedText(err)})
			return
		}
		writeProviderErr(w, d, err)
	}
	mux.HandleFunc("POST "+PathGrant, func(w http.ResponseWriter, r *http.Request) {
		in, g, ok := find(w, r)
		if !ok {
			return
		}
		if in.Item == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "a grant without its item"})
			return
		}
		ref, err := g.Grant(r.Context(), in.Caller, *in.Item)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ref": ref})
	})
	mux.HandleFunc("POST "+PathGrantRefund, func(w http.ResponseWriter, r *http.Request) {
		in, g, ok := find(w, r)
		if !ok {
			return
		}
		rf, can := g.(GranterRefunder)
		if !can || in.Refund == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": ErrNotRefundable.Error()})
			return
		}
		res, err := rf.RefundGranted(r.Context(), in.Caller, *in.Refund)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"amountCents": res.AmountCents, "effect": res.Effect})
	})
	mux.HandleFunc("POST "+PathGrantChoices, func(w http.ResponseWriter, r *http.Request) {
		in, g, ok := find(w, r)
		if !ok {
			return
		}
		out := []GranterChoice{}
		if cat, can := g.(GranterCatalog); can {
			ch, err := cat.Choices(r.Context(), in.Caller)
			if err != nil {
				fail(w, err)
				return
			}
			if ch != nil {
				out = ch
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"choices": out})
	})
}

// refusedText is the words of a refusal (EntRefused's message as it is; another error without its sentinel's prefix).
func refusedText(err error) string {
	var refused *EntRefused
	if errors.As(err, &refused) {
		return refused.Message
	}
	for _, sent := range []error{ErrInvalid, ErrConflict, ErrForbidden} {
		if errors.Is(err, sent) {
			if msg := strings.TrimPrefix(err.Error(), sent.Error()+": "); msg != "" {
				return msg
			}
		}
	}
	return err.Error()
}
