package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/21stware/raise-go/k3"
)

// The agent's drafts, as an app sees them (ADR-0095): the app reviews its own items in place, on the page of the record they
// change (?agentReview=), and says on a record's page that drafts touch it and who wrote a record (provenance). It reaches the
// platform's draft service (internal/modules/k3control) through this interface (Deps.Drafts) and sees and touches only the items
// that are for itself; only the person's own, undelegated session edits, abandons and applies. The platform's draft service calls the
// app's ops itself, at the manifest's opsApi (OpsHandler).
type DraftService interface {
	List(ctx context.Context, a k3.DraftActor, f k3.DraftListFilter) ([]*k3.DraftView, error)
	Get(ctx context.Context, a k3.DraftActor, id string) (*k3.DraftView, error)
	Revalidate(ctx context.Context, a k3.DraftActor, id string) (*k3.DraftView, error)
	Review(ctx context.Context, a k3.DraftActor, id, item string, e k3.ItemEdit) (*k3.DraftItem, []string, error)
	Abandon(ctx context.Context, a k3.DraftActor, id, item string) error
	Rebase(ctx context.Context, a k3.DraftActor, id, item string) (*k3.DraftItem, error)
	Apply(ctx context.Context, a k3.DraftActor, id string, sel []k3.ApplyItem) (*k3.ApplyOutcome, error)
	Provenance(ctx context.Context, a k3.DraftActor, ref string) ([]k3.Provenance, error)
}

// statusError is a refusal of the draft service: the status it maps to and the extra fields it carries.
type statusError interface {
	HTTPStatus() int
	Details() map[string]any
}

// DraftsPath is where every L3 app serves the agent's drafts that touch it.
const DraftsPath = "/api/agent-drafts"

// MountDrafts serves the agent's drafts of one app on its router:
//
//	GET  /api/agent-drafts?status=&target=&session=       the person's open drafts that have an item for this app
//	GET  /api/agent-drafts/{id}?revalidate=1              one draft: this app's items in full (the review page, ADR-0095 §3)
//	PATCH /api/agent-drafts/{id}/items/{item}             {revision, fields?, pick?}: the person's values and choices
//	POST /api/agent-drafts/{id}/items/{item}/abandon      drop the item
//	POST /api/agent-drafts/{id}/items/{item}/rebase       go on from the record as it is now
//	POST /api/agent-drafts/{id}/apply                     {items:[{id, revision, confirmed?}]}: this app's items (and what they need)
//	GET  /api/agent-provenance?ref=                       who wrote a record (a draft the person applied, a policy, a confirmed action)
//
// The person is the caller the gateway verified; the writes refuse a delegated caller (the agent).
func MountDrafts(r Router, app string, d Deps) {
	svc := d.Drafts
	actor := func(c Caller) k3.DraftActor {
		return k3.DraftActor{Tenant: c.Tenant, Subject: c.User, Name: c.Name, Roles: c.Roles, App: app, Delegated: c.Agent != "", Agent: c.Agent}
	}
	h := func(f func(r *http.Request, a k3.DraftActor) (any, error)) http.HandlerFunc {
		return Auth(func(w http.ResponseWriter, r *http.Request, c Caller) {
			if svc == nil {
				Err(w, http.StatusServiceUnavailable, "the draft service is not available")
				return
			}
			out, err := f(r, actor(c))
			if err != nil {
				var se statusError
				if errors.As(err, &se) {
					body := map[string]any{"error": err.Error()}
					for k, v := range se.Details() {
						body[k] = v
					}
					JSON(w, se.HTTPStatus(), body)
					return
				}
				Fail(w, d.Logger(), err)
				return
			}
			JSON(w, http.StatusOK, out)
		})
	}
	r.HandleFunc("GET "+DraftsPath, h(func(r *http.Request, a k3.DraftActor) (any, error) {
		q := r.URL.Query()
		vs, err := svc.List(r.Context(), a, k3.DraftListFilter{Status: q.Get("status"), Target: q.Get("target"), Session: q.Get("session"), Limit: 50})
		return map[string]any{"drafts": vs}, err
	}))
	person := func(f func(r *http.Request, a k3.DraftActor) (any, error)) http.HandlerFunc {
		return h(func(r *http.Request, a k3.DraftActor) (any, error) {
			if a.Delegated {
				return nil, forbidden{"only the person reviews, abandons and applies a draft"}
			}
			return f(r, a)
		})
	}
	r.HandleFunc("GET "+DraftsPath+"/{id}", h(func(r *http.Request, a k3.DraftActor) (any, error) {
		if r.URL.Query().Get("revalidate") == "1" && !a.Delegated {
			return svc.Revalidate(r.Context(), a, r.PathValue("id"))
		}
		return svc.Get(r.Context(), a, r.PathValue("id"))
	}))
	r.HandleFunc("PATCH "+DraftsPath+"/{id}/items/{item}", person(func(r *http.Request, a k3.DraftActor) (any, error) {
		var e k3.ItemEdit
		if err := decodeJSON(r, &e); err != nil {
			return nil, err
		}
		it, _, err := svc.Review(r.Context(), a, r.PathValue("id"), r.PathValue("item"), e)
		return map[string]any{"item": it}, err
	}))
	r.HandleFunc("POST "+DraftsPath+"/{id}/items/{item}/abandon", person(func(r *http.Request, a k3.DraftActor) (any, error) {
		return map[string]any{"ok": true}, svc.Abandon(r.Context(), a, r.PathValue("id"), r.PathValue("item"))
	}))
	r.HandleFunc("POST "+DraftsPath+"/{id}/items/{item}/rebase", person(func(r *http.Request, a k3.DraftActor) (any, error) {
		it, err := svc.Rebase(r.Context(), a, r.PathValue("id"), r.PathValue("item"))
		return map[string]any{"item": it}, err
	}))
	r.HandleFunc("POST "+DraftsPath+"/{id}/apply", person(func(r *http.Request, a k3.DraftActor) (any, error) {
		var body struct {
			Items []k3.ApplyItem `json:"items"`
		}
		if err := decodeJSON(r, &body); err != nil {
			return nil, err
		}
		return svc.Apply(r.Context(), a, r.PathValue("id"), body.Items)
	}))
	r.HandleFunc("GET /api/agent-provenance", h(func(r *http.Request, a k3.DraftActor) (any, error) {
		ps, err := svc.Provenance(r.Context(), a, r.URL.Query().Get("ref"))
		return map[string]any{"provenance": ps}, err
	}))
}

// forbidden and badRequest are refusals of the draft routes themselves, mapped like the draft service's.
type forbidden struct{ msg string }

func (e forbidden) Error() string         { return e.msg }
func (forbidden) HTTPStatus() int         { return http.StatusForbidden }
func (forbidden) Details() map[string]any { return nil }

type badRequest struct{ msg string }

func (e badRequest) Error() string         { return e.msg }
func (badRequest) HTTPStatus() int         { return http.StatusBadRequest }
func (badRequest) Details() map[string]any { return nil }

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return badRequest{"the body is not valid JSON: " + err.Error()}
	}
	return nil
}
