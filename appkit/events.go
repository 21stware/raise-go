package appkit

import (
	"context"
	"io/fs"
	"net/http"
	"slices"
	"strings"

	"github.com/21stware/raise-go/k3"
)

// The platform's data events (ADR-0130 §2.5 data 1 and 7, T11): an app whose tables live in its own schema points at the platform's
// tenants, sites and customers by id only (no foreign key leaves the schema), so the platform tells it, at least once, when one of
// them is gone or merged, or when the tenant uninstalls the app. Any app that declares a kind in events.receives receives it.
const (
	EventTenantDeleted = "tenant.deleted" // subject: the tenant
	EventSiteDeleted   = "site.deleted"   // subject: the site's id
	// EventCustomerMerged: the customer centre merged one customer into another; subject: MergedSubject. An app that keeps customer
	// ids moves what pointed at the merged one to the kept one.
	EventCustomerMerged = "customer.merged"
	// EventAppUninstalled: the tenant removed this app's installation; subject: the app's own id. Only the app that was uninstalled
	// hears it. Uninstalling deletes nothing: the app decides what to keep (the tenant may install it again).
	EventAppUninstalled = "app.uninstalled"
)

// DataEvents are the kinds of DataEvent an app may declare in its manifest (events.receives).
var DataEvents = []string{EventTenantDeleted, EventSiteDeleted, EventCustomerMerged, EventAppUninstalled}

// MergedSubject is the subject of a customer.merged event: "<merged id>><kept id>".
func MergedSubject(merged, into string) string { return merged + ">" + into }

// ParseMergedSubject reads a customer.merged subject; ok is false when it is not one.
func ParseMergedSubject(subject string) (merged, into string, ok bool) {
	merged, into, ok = strings.Cut(subject, ">")
	return merged, into, ok && merged != "" && into != "" && merged != into
}

// DataEventHandler is an app that keeps its own data right when the platform removes what it pointed at. The same event may come
// more than once: the handler leaves the data as it is when there is nothing left to do. It receives it at POST /k3/events/{kind}
// with the platform's assertion (Serve).
type DataEventHandler interface {
	App
	DataEvent(ctx context.Context, d Deps, kind, tenant, subject string) error
}

// SchemaOwner is an app with tables of its own: its migrations travel with it and the platform runs them as the app's role in the
// app's schema (the manifest's data.migrations names them).
type SchemaOwner interface {
	App
	Migrations() fs.FS
}

// PathEventFmt is where the platform delivers its data events to an app that runs on its own (ADR-0130 §2.5 data 1; the platform's
// side is internal/appevents): POST /k3/events/{kind} with {"tenant", "subject"}, as the platform (via event), at least once.
const PathEventFmt = "/k3/events/"

// DataEventCall is the body of a data event. Test is set on a developer's `k3 trigger` delivery in a test enterprise: the platform
// changed nothing (the tenant, site or customer is still there, the app still installed).
type DataEventCall struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
	Test    bool   `json:"test,omitempty"`
}

// serveEvents answers the data events a's manifest declares it receives (events.receives).
func serveEvents(mux *http.ServeMux, a App, d Deps, m *k3.Manifest) {
	h, ok := a.(DataEventHandler)
	if !ok || m.Events == nil || len(m.Events.Receives) == 0 {
		return
	}
	mux.HandleFunc("POST "+PathEventFmt+"{kind}", func(w http.ResponseWriter, r *http.Request) {
		var in DataEventCall
		if !platformJSON(w, r, k3.ViaEvent, &in) || !sameTenant(w, r, in.Tenant) {
			return
		}
		kind := r.PathValue("kind")
		if !slices.Contains(m.Events.Receives, kind) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "the app does not receive this event"})
			return
		}
		if err := h.DataEvent(r.Context(), d, kind, in.Tenant, in.Subject); err != nil {
			d.Logger().Warn("appkit: a data event failed", "kind", kind, "tenant", in.Tenant, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
