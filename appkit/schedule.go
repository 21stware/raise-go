package appkit

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/21stware/raise-go/k3"
)

// The schedule trigger (ADR-0065 §5): the platform's clock starts a run of a service the manifest declares with `schedule` at every
// time of its cron expression, for every tenant that has the app installed and on, at least once, keyed "schedule:" + the time. An app
// declares the service (ScheduleService) and its code answers it (Scheduling); Serve serves the one with the other.

// ScheduleFunc runs one time of a schedule for the tenant. An error is retried (with the platform's back-off), unless it is a refusal
// (ErrInvalid, ErrConflict, ErrForbidden, an HTTPError below 500): the run fails for good. The key in.ScheduledAt names is the same on
// every retry; a round that may be delivered twice must be safe to run twice.
type ScheduleFunc func(ctx context.Context, tenant string, in k3.ScheduleInput) error

// Scheduling is implemented by an app whose services run on a schedule: the function of each declared service, by service id.
type Scheduling interface {
	Schedules(d Deps) map[string]ScheduleFunc
}

// ScheduleService declares a service with the schedule trigger at /k3/services/{id}: cron is five fields (minute hour day-of-month
// month day-of-week) read in the zone (empty: Asia/Shanghai); missed is k3.MissedSkip (the default) or k3.MissedOnce.
func ScheduleService(id, title, cron, zone, missed string) k3.Service {
	return k3.Service{ID: id, Title: title, Trigger: []string{k3.TriggerSchedule}, Endpoint: "/k3/services/" + id,
		Schedule: &k3.Schedule{Cron: cron, Timezone: zone, Missed: missed}}
}

// serveSchedules mounts each declared service with the schedule trigger that the app's code answers.
func serveSchedules(mux *http.ServeMux, a App, d Deps, m *k3.Manifest) {
	sa, ok := a.(Scheduling)
	if !ok {
		return
	}
	fns := sa.Schedules(d)
	for _, sv := range m.Services {
		fn := fns[sv.ID]
		if fn == nil || sv.Schedule == nil || !slices.Contains(sv.Trigger, k3.TriggerSchedule) {
			continue
		}
		mux.HandleFunc("POST "+sv.Endpoint, func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Input   k3.ScheduleInput `json:"input"`
				Trigger string           `json:"trigger"`
			}
			if !platformJSON(w, r, k3.ViaService, &in) {
				return
			}
			if in.Trigger != k3.TriggerSchedule {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "this service runs on its schedule"})
				return
			}
			tenant, _ := PlatformCall(r.Context())
			err := fn(r.Context(), tenant, in.Input)
			switch {
			case err == nil:
				w.WriteHeader(http.StatusNoContent)
			case refusal(err):
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			case errors.Is(err, context.DeadlineExceeded):
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "timed out"})
			default:
				d.Logger().Warn("appkit: a scheduled run failed", "service", sv.ID, "at", in.Input.ScheduledAt, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
			}
		})
	}
}
