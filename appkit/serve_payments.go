package appkit

import (
	"context"
	"net/http"

	"github.com/21stware/raise-go/k3"
)

// Settling over the wire (ADR-0130 §2.4): an app that sells through the payment service declares a service with the trigger
// event:payment (PaymentService) and takes its payments and refunds in through it. The service runtime delivers each one at least
// once (keyed: "paid:<order>", "refund:<refund>"), retried with backoff while the app answers anything but 200. The platform's side
// is its internal/providers/settle.go (ServiceSettler).
//
//	POST {service endpoint of event:payment}   {input: PaymentCall, …}   → 200 {"ok": true} · 422 unknown kind · 500 not yet (retried)

// PaymentCall is the input of a run of the payment service: what was paid or refunded.
type PaymentCall struct {
	Kind   string     `json:"kind"` // PaymentPaid or PaymentRefunded
	Order  PayOrder   `json:"order"`
	Refund *PayRefund `json:"refund,omitempty"`
}

// The kinds of a PaymentCall.
const (
	PaymentPaid     = "paid"
	PaymentRefunded = "refunded"
)

// Settler takes the app's paid orders and refunds in. It is called at least once for each: it applies one once (by the order's or
// the refund's id) and answers nil again for one it already applied; an error is "not yet" and the run is retried.
type Settler interface {
	Paid(ctx context.Context, o PayOrder) error
	Refunded(ctx context.Context, o PayOrder, r PayRefund) error
}

// Settling is implemented by an app that sells through the payment service (its Settler hears of every order it created, the
// orders' source app).
type Settling interface {
	Settler(d Deps) Settler
}

// PaymentService is the service of event:payment an app that sells through the payment service declares (the platform's
// firstparty.PaymentService).
func PaymentService() k3.Service {
	return k3.Service{ID: "payment", Title: "收款入账", Trigger: []string{k3.TriggerEvent + k3.EventPayment}, Endpoint: "/k3/services/payment"}
}

// servePayments is the app's service of event:payment: a run's input is a PaymentCall, handed to the app's Settler.
func servePayments(mux *http.ServeMux, a App, m *k3.Manifest, d Deps) {
	p, ok := a.(Settling)
	if !ok {
		return
	}
	sv, declared := m.ServiceFor(k3.EventPayment)
	if !declared {
		return
	}
	st := p.Settler(d)
	if st == nil {
		return
	}
	mux.HandleFunc("POST "+sv.Endpoint, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input PaymentCall `json:"input"`
		}
		if !platformJSON(w, r, k3.ViaService, &in) || !sameTenant(w, r, in.Input.Order.Tenant) {
			return
		}
		var err error
		switch in.Input.Kind {
		case PaymentPaid:
			err = st.Paid(r.Context(), in.Input.Order)
		case PaymentRefunded:
			if in.Input.Refund == nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "a refund without its refund"})
				return
			}
			err = st.Refunded(r.Context(), in.Input.Order, *in.Input.Refund)
		default:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unknown kind"})
			return
		}
		if err != nil {
			d.Logger().Warn("appkit: a payment was not taken in", "order", in.Input.Order.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "not yet"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}
