package appkit

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The events apps publish to each other (opv2 J2 ②). A publisher declares its kinds in the manifest (events.emits, each {app}.{what})
// and sends one with Client.App.Events.Emit; the platform starts, at least once, a run of every installed app's service that declares
// the trigger event:{kind}, keyed by the event's id (the publisher's key). A subscriber declares the service (EventService) and its code
// answers it (AppEventHandling); Serve serves the one with the other.

// AppEvent is an event another app published, as the subscriber gets it. The customer is the customer centre's id.
type AppEvent struct {
	ID        string         `json:"id"` // the same on every delivery of this event: de-duplicate on it
	Type      string         `json:"type"`
	App       string         `json:"app"` // the publisher
	Tenant    string         `json:"tenant"`
	Customer  string         `json:"customer,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// AppEventFunc takes one event in. An error is retried with the platform's back-off, unless it is a refusal (ErrInvalid, ErrConflict,
// ErrForbidden, an HTTPError below 500): the run fails for good.
type AppEventFunc func(ctx context.Context, ev AppEvent) error

// AppEventHandling is implemented by an app that subscribes to other apps' events: the function of each declared service, by id.
type AppEventHandling interface {
	AppEvents(d Deps) map[string]AppEventFunc
}

// EventService declares a service at /k3/services/{id} that receives the given kinds (another app's {app}.{what}).
func EventService(id, title string, kinds ...string) k3.Service {
	sv := k3.Service{ID: id, Title: title, Endpoint: "/k3/services/" + id}
	for _, k := range kinds {
		sv.Trigger = append(sv.Trigger, k3.TriggerEvent+k)
	}
	return sv
}

// serveAppEvents mounts each declared service of event triggers that the app's code answers.
func serveAppEvents(mux *http.ServeMux, a App, d Deps, m *k3.Manifest) {
	h, ok := a.(AppEventHandling)
	if !ok {
		return
	}
	fns := h.AppEvents(d)
	for _, sv := range m.Services {
		fn := fns[sv.ID]
		if fn == nil {
			continue
		}
		mux.HandleFunc("POST "+sv.Endpoint, func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Input   AppEvent `json:"input"`
				Trigger string   `json:"trigger"`
			}
			if !platformJSON(w, r, k3.ViaService, &in) || !sameTenant(w, r, in.Input.Tenant) {
				return
			}
			if kind, ok := strings.CutPrefix(in.Trigger, k3.TriggerEvent); !ok || kind != in.Input.Type {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "not an event of this service"})
				return
			}
			err := fn(r.Context(), in.Input)
			switch {
			case err == nil:
				w.WriteHeader(http.StatusNoContent)
			case refusal(err):
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			default:
				d.Logger().Warn("appkit: an app event was not taken in", "service", sv.ID, "event", in.Input.ID, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
			}
		})
	}
}

// Emit is one event the app publishes (POST /app/v1/events/emit): a kind its manifest declares in events.emits, the key that makes it
// one event (the same key again is the same event, at most 200 characters), the customer it is about (the customer centre's id, may be
// empty) and what happened, by ids (at most 16 KiB of JSON).
type Emit struct {
	Type     string         `json:"type"`
	Key      string         `json:"key"`
	Customer string         `json:"customer,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// Emit publishes the event in the tenant as the app itself. It returns once the platform has queued it; the subscribers' runs follow,
// at least once. A kind the app does not declare is ErrForbidden.
func (cl *Client) Emit(ctx context.Context, tenant string, e Emit) error {
	return cl.tenantCall(ctx, tenant, "POST", "/app/v1/events/emit", nil, e, nil)
}
