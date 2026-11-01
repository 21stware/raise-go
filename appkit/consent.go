package appkit

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The customers' consent changes (opv2 J2 ③): when a customer of the tenant withdraws a consent (an unsubscribe link, a reply, a staff
// change, an erasure) or gives one, the platform starts a run of the service an app declares with the trigger
// event:customer.consent.changed, at least once, keyed by the change's id — only for an app whose manifest uses ScopeConsentEvents.
// The customer's queued messages of a withdrawn channel and purpose are already cancelled by then.

// The kind of a consent change, and the scope an app declares (uses) to receive it.
const (
	KindConsentChanged = "customer.consent.changed"
	ScopeConsentEvents = "app.customers:consent_events"
)

// ConsentChange is one change of a customer's consent: the customer centre's id, the channel (sms, wechat_template, wecom or any),
// the purpose (service or marketing) and the new state (granted or withdrawn). Nothing else about the customer.
type ConsentChange struct {
	ID       string    `json:"id"` // the same on every delivery of this change: de-duplicate on it
	Type     string    `json:"type"`
	Tenant   string    `json:"tenant"`
	Customer string    `json:"customer"`
	Channel  string    `json:"channel"`
	Purpose  string    `json:"purpose"`
	State    string    `json:"state"`
	At       time.Time `json:"at"`
}

// ConsentFunc takes one change in; an error is retried, a refusal (ErrInvalid, ErrConflict, ErrForbidden) fails the run for good.
type ConsentFunc func(ctx context.Context, c ConsentChange) error

// ConsentHandling is implemented by an app that acts on consent changes (the journey stops a customer's marketing runs).
type ConsentHandling interface {
	ConsentChanges(d Deps) ConsentFunc
}

// ConsentService declares the service of consent changes at /k3/services/{id}; the manifest must also use ScopeConsentEvents.
func ConsentService(id, title string) k3.Service {
	return k3.Service{ID: id, Title: title, Trigger: []string{k3.TriggerEvent + KindConsentChanged}, Endpoint: "/k3/services/" + id}
}

// serveConsent mounts the declared service of consent changes when the app's code answers it.
func serveConsent(mux *http.ServeMux, a App, d Deps, m *k3.Manifest) {
	h, ok := a.(ConsentHandling)
	if !ok {
		return
	}
	fn := h.ConsentChanges(d)
	sv, declared := m.ServiceFor(KindConsentChanged)
	if fn == nil || !declared || !slices.Contains(m.Uses, ScopeConsentEvents) {
		return
	}
	mux.HandleFunc("POST "+sv.Endpoint, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input ConsentChange `json:"input"`
		}
		if !platformJSON(w, r, k3.ViaService, &in) || !sameTenant(w, r, in.Input.Tenant) {
			return
		}
		err := fn(r.Context(), in.Input)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case refusal(err):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		default:
			d.Logger().Warn("appkit: a consent change was not taken in", "change", in.Input.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
		}
	})
}
