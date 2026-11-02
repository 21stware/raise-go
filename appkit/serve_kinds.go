package appkit

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The provider kinds an app in a process of its own answers since ADR-0130 P11; the platform's side is its internal/providers
// (remote_kinds.go, registry_customer_assert.go), whose paths and bodies these are. A call for a person carries the person in the
// assertion (sub, name, roles), never in the body.
//
//	POST /k3/providers/customer-assert   {tenant, phone, customer?}  → {"customer", "matched"} · 422 {"code":"bad_phone"} · 409 {"code":"ambiguous_customer","candidates"}
//	POST /k3/providers/people            {text, digits, all, limit}  → {"candidates": [candidate]}  (never the whole phone)
//	POST /k3/providers/backlinks         {ref, limit}                → {"items": [Backlink]}
//	POST /k3/providers/backlinks/count   {ref}                       → {"count": n}   (the platform's call, no person)
//	POST /k3/providers/holdings          {holders}                   → {"items": [Holding]}
//	POST /k3/providers/agenda            {user, from, to}            → {"items": [AgendaItem]}
//	POST /k3/providers/work/items        {query}                     → {"items": [k3.WorkItem]}
//	POST /k3/providers/work/card         {id, query}                 → {"card": k3.WorkCardData}   404: ErrNoCard
//	POST /k3/providers/desk/access       {}                          → {"redeem", "void"}
//	POST /k3/providers/desk/http         HTTPCall                    → HTTPAnswer
//	POST /k3/providers/till/access       {}                          → {"sell"}
//	POST /k3/providers/till/http         HTTPCall                    → HTTPAnswer
//	POST /k3/providers/handover          {subject}                   → {"items": [Responsibility]}
//	POST /k3/providers/handover/move     {from, to, kinds, dry}      → {"items": [Responsibility], "error": "…"}
//	POST /k3/providers/holds/holdable    {}                          → {"items": [HoldableItem]}
//	POST /k3/providers/holds/free        {items, from, to, except}   → {"items": [FreeItem]}
//	POST /k3/providers/holds/hold        HoldRequest                 → {"items": [HoldResult]}
//	POST /k3/providers/holds/move        {holdId, from, to}          → 204 · 409 ErrHoldConflict
//	POST /k3/providers/holds/release     {holdId, reason}            → 204
//	POST /k3/providers/holds/released    {caller, event}             → 204 · 422 refused · else retried
//
// Retrieve and embeddings are served by serveKnowledge (knowledge.go).
const (
	PathCustomerAssert = "/k3/providers/customer-assert"
	PathPeople         = "/k3/providers/people"
	PathBacklinks      = "/k3/providers/backlinks"
	PathBacklinksCount = "/k3/providers/backlinks/count"
	PathHoldings       = "/k3/providers/holdings"
	PathAgenda         = "/k3/providers/agenda"
	PathWorkItems      = "/k3/providers/work/items"
	PathWorkCard       = "/k3/providers/work/card"
	PathDeskAccess     = "/k3/providers/desk/access"
	PathDeskHTTP       = "/k3/providers/desk/http"
	PathTillAccess     = "/k3/providers/till/access"
	PathTillHTTP       = "/k3/providers/till/http"
	PathHandover       = "/k3/providers/handover"
	PathHandoverMove   = "/k3/providers/handover/move"
	PathHoldable       = "/k3/providers/holds/holdable"
	PathHoldsFree      = "/k3/providers/holds/free"
	PathHoldsHold      = "/k3/providers/holds/hold"
	PathHoldsMove      = "/k3/providers/holds/move"
	PathHoldsRelease   = "/k3/providers/holds/release"
	PathHoldsReleased  = "/k3/providers/holds/released"
)

// The codes of a CustomerAsserter's refusals (the platform's providers.CodeBadPhone / CodeAmbiguousCustomer).
const (
	CodeBadPhone          = "bad_phone"
	CodeAmbiguousCustomer = "ambiguous_customer"
)

// The wire forms of the bodies (the platform's providers.PeopleQueryWire, PersonCandidateWire, WorkQueryWire, HoldsMove,
// HoldReleasedCall, CustomerAssertCall / Answer / Refusal).
type (
	peopleQueryWire struct {
		Text   string `json:"text"`
		Digits string `json:"digits,omitempty"`
		All    bool   `json:"all,omitempty"`
		Limit  int    `json:"limit"`
	}
	personCandidateWire struct {
		PersonCandidate
		Phone4   string `json:"phone4,omitempty"`
		No       string `json:"no,omitempty"`
		Customer string `json:"customer,omitempty"`
		Detail   string `json:"detail,omitempty"`
		Matched  string `json:"matched,omitempty"`
	}
	workQueryWire struct {
		Lang string    `json:"lang,omitempty"`
		Now  time.Time `json:"now"`
		Zone string    `json:"zone,omitempty"`
	}
	holdsMove struct {
		HoldID string    `json:"holdId"`
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
	}
	holdReleasedCall struct {
		Caller Caller       `json:"caller"`
		Event  HoldReleased `json:"event"`
	}
	customerAssertCall struct {
		Tenant   string `json:"tenant"`
		Phone    string `json:"phone"`
		Customer string `json:"customer,omitempty"`
	}
	customerAssertAnswer struct {
		Customer string `json:"customer"`
		Matched  bool   `json:"matched"`
	}
	customerAssertRefusal struct {
		Error      string   `json:"error"`
		Code       string   `json:"code"`
		Candidates []string `json:"candidates,omitempty"`
	}
)

// query is the travelled work query back (an unknown zone is UTC).
func (w workQueryWire) query() WorkQuery {
	q := WorkQuery{Lang: w.Lang, Now: w.Now}
	if w.Zone != "" {
		if loc, err := time.LoadLocation(w.Zone); err == nil {
			q.Zone = loc
		}
	}
	return q
}

// serveKinds answers the P11 provider kinds, each only when the manifest declares it and the app provides it.
func serveKinds(mux *http.ServeMux, a App, decl *k3.ProvidersDecl, d Deps) {
	// person: a call made for a person (the assertion's); platform: the platform's own call (no person, the tenant only)
	person := func(path string, in func() any, answer func(r *http.Request, c Caller, in any) (any, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			v := in()
			c, ok := viewer(w, r, v)
			if !ok {
				return
			}
			out, err := answer(r, c, v)
			if err != nil {
				writeProviderErr(w, d, err)
				return
			}
			if out == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	platform := func(path, via string, in func() any, answer func(r *http.Request, tenant string, in any) (any, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			v := in()
			if !platformJSON(w, r, via, v) {
				return
			}
			tenant, _ := PlatformCall(r.Context())
			out, err := answer(r, tenant, v)
			if err != nil {
				writeProviderErr(w, d, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	type (
		refIn     struct{ Ref string }
		holdersIn struct{ Holders []string }
		workIn    struct {
			ID    string        `json:"id"`
			Query workQueryWire `json:"query"`
		}
	)

	if p, ok := a.(CustomerAsserting); ok && decl.Has("customerAssert") {
		if ca := p.CustomerAsserter(d); ca != nil {
			serveCustomerAssert(mux, ca, d)
		}
	}
	if p, ok := a.(PeopleSourcing); ok && decl.Has("people") {
		if ps := p.PeopleSource(d); ps != nil {
			person(PathPeople, func() any { return &peopleQueryWire{} }, func(r *http.Request, c Caller, in any) (any, error) {
				q := in.(*peopleQueryWire)
				cs, err := ps.PeopleCandidates(r.Context(), c, PeopleQuery{Text: q.Text, Digits: q.Digits, All: q.All, Limit: q.Limit})
				if err != nil {
					return nil, err
				}
				out := make([]personCandidateWire, 0, len(cs))
				for _, p := range cs {
					out = append(out, personCandidateWire{PersonCandidate: p, Phone4: p.Phone4, No: p.No, Customer: p.Customer, Detail: p.Detail, Matched: p.Matched})
				}
				return map[string]any{"candidates": out}, nil
			})
		}
	}
	if p, ok := a.(Backlinking); ok && decl.Has("backlinks") {
		if bl := p.BacklinkProvider(d); bl != nil {
			type backlinksIn struct {
				Ref   string
				Limit int
			}
			person(PathBacklinks, func() any { return &backlinksIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				v := in.(*backlinksIn)
				items, err := bl.Backlinks(r.Context(), c, v.Ref, v.Limit)
				if items == nil {
					items = []Backlink{}
				}
				return map[string]any{"items": items}, err
			})
			platform(PathBacklinksCount, k3.ViaProvider, func() any { return &refIn{} }, func(r *http.Request, tenant string, in any) (any, error) {
				n, err := countBacklinks(r, bl, tenant, in.(*refIn).Ref)
				return map[string]int{"count": n}, err
			})
		}
	}
	if p, ok := a.(Holdable); ok && decl.Has("holdings") {
		if hp := p.HoldingsProvider(d); hp != nil {
			person(PathHoldings, func() any { return &holdersIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				items, err := hp.Holdings(r.Context(), c, in.(*holdersIn).Holders)
				if items == nil {
					items = []Holding{}
				}
				return map[string]any{"items": items}, err
			})
		}
	}
	if p, ok := a.(Agendable); ok && decl.Has("agenda") {
		if ap := p.AgendaProvider(d); ap != nil {
			type agendaIn struct {
				User     string
				From, To time.Time
			}
			person(PathAgenda, func() any { return &agendaIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				v := in.(*agendaIn)
				items, err := ap.Agenda(r.Context(), c, v.User, v.From, v.To)
				if items == nil {
					items = []AgendaItem{}
				}
				return map[string]any{"items": items}, err
			})
		}
	}
	if p, ok := a.(Workable); ok && decl.Has("work") {
		if wp := p.WorkProvider(d); wp != nil {
			person(PathWorkItems, func() any { return &workIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				items, err := wp.WorkItems(r.Context(), c, in.(*workIn).Query.query())
				if items == nil {
					items = []k3.WorkItem{}
				}
				return map[string]any{"items": items}, err
			})
			person(PathWorkCard, func() any { return &workIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				v := in.(*workIn)
				card, err := wp.WorkCard(r.Context(), c, v.ID, v.Query.query())
				if errors.Is(err, ErrNoCard) {
					return nil, ErrNotFound
				}
				return map[string]any{"card": card}, err
			})
		}
	}
	if p, ok := a.(Deskable); ok && decl.Has("desk") {
		if dk := p.DeskProvider(d); dk != nil {
			person(PathDeskAccess, func() any { return &struct{}{} }, func(r *http.Request, c Caller, _ any) (any, error) {
				redeem, void := dk.DeskAccess(r.Context(), c)
				return map[string]bool{"redeem": redeem, "void": void}, nil
			})
			serveHandedOver(mux, PathDeskHTTP, dk.DeskHandler())
		}
	}
	if p, ok := a.(Tillable); ok && decl.Has("till") {
		if tl := p.TillProvider(d); tl != nil {
			person(PathTillAccess, func() any { return &struct{}{} }, func(r *http.Request, c Caller, _ any) (any, error) {
				return map[string]bool{"sell": tl.TillAccess(r.Context(), c)}, nil
			})
			serveHandedOver(mux, PathTillHTTP, tl.TillHandler())
		}
	}
	if p, ok := a.(HandingOver); ok && decl.Has("handover") {
		if hv := p.HandoverProvider(d); hv != nil {
			person(PathHandover, func() any { return &struct{ Subject string }{} }, func(r *http.Request, c Caller, in any) (any, error) {
				items, err := hv.Responsibilities(r.Context(), c, in.(*struct{ Subject string }).Subject)
				if items == nil {
					items = []Responsibility{}
				}
				return map[string]any{"items": items}, err
			})
			type moveIn struct {
				From, To string
				Kinds    []string
				Dry      bool
			}
			person(PathHandoverMove, func() any { return &moveIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				v := in.(*moveIn)
				items, err := hv.HandOver(r.Context(), c, v.From, v.To, v.Kinds, v.Dry)
				if items == nil {
					items = []Responsibility{}
				}
				out := map[string]any{"items": items}
				if err != nil {
					out["error"] = err.Error() // what it moved before it stopped is still the answer
				}
				return out, nil
			})
		}
	}
	if p, ok := a.(ResourceHolder); ok && decl.Has("holds") {
		if hs := p.ResourceHolds(d); hs != nil {
			person(PathHoldable, func() any { return &struct{}{} }, func(r *http.Request, c Caller, _ any) (any, error) {
				items, err := hs.Holdable(r.Context(), c)
				if items == nil {
					items = []HoldableItem{}
				}
				return map[string]any{"items": items}, err
			})
			type freeIn struct {
				Items    []string
				From, To time.Time
				Except   string
			}
			person(PathHoldsFree, func() any { return &freeIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				v := in.(*freeIn)
				items, err := hs.Free(r.Context(), c, v.Items, v.From, v.To, v.Except)
				if items == nil {
					items = []FreeItem{}
				}
				return map[string]any{"items": items}, err
			})
			person(PathHoldsHold, func() any { return &HoldRequest{} }, func(r *http.Request, c Caller, in any) (any, error) {
				items, err := hs.Hold(r.Context(), c, *in.(*HoldRequest))
				if items == nil {
					items = []HoldResult{}
				}
				return map[string]any{"items": items}, err
			})
			mux.HandleFunc("POST "+PathHoldsMove, func(w http.ResponseWriter, r *http.Request) {
				var in holdsMove
				c, ok := viewer(w, r, &in)
				if !ok {
					return
				}
				switch err := hs.Move(r.Context(), c, in.HoldID, in.From, in.To); {
				case errors.Is(err, ErrHoldConflict):
					writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				case err != nil:
					writeProviderErr(w, d, err)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			})
			person(PathHoldsRelease, func() any { return &struct{ HoldID, Reason string }{} }, func(r *http.Request, c Caller, in any) (any, error) {
				v := in.(*struct{ HoldID, Reason string })
				return nil, hs.Release(r.Context(), c, v.HoldID, v.Reason)
			})
		}
	}
	if p, ok := a.(HoldWatching); ok && decl.Has("holdWatch") {
		if hw := p.HoldWatcher(d); hw != nil {
			mux.HandleFunc("POST "+PathHoldsReleased, func(w http.ResponseWriter, r *http.Request) {
				var in holdReleasedCall
				if !platformJSON(w, r, k3.ViaEvent, &in) || !sameTenant(w, r, in.Caller.Tenant) {
					return
				}
				switch err := hw.HoldReleased(r.Context(), noPerson(in.Caller), in.Event); {
				case err == nil:
					w.WriteHeader(http.StatusNoContent)
				case refusal(err):
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
				default:
					d.Logger().Warn("appkit: a released hold failed", "hold", in.Event.HoldID, "err", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
				}
			})
		}
	}
}

// serveCustomerAssert answers providers.customerAssert: the platform's own call (no person), for the tenant of its assertion.
func serveCustomerAssert(mux *http.ServeMux, ca CustomerAsserter, d Deps) {
	mux.HandleFunc("POST "+PathCustomerAssert, func(w http.ResponseWriter, r *http.Request) {
		var in customerAssertCall
		if !platformJSON(w, r, k3.ViaProvider, &in) || !sameTenant(w, r, in.Tenant) {
			return
		}
		var cu string
		var matched bool
		var err error
		if in.Customer != "" {
			cu, matched, err = ca.AssertCustomerAs(r.Context(), in.Tenant, in.Phone, in.Customer)
		} else {
			cu, matched, err = ca.AssertCustomer(r.Context(), in.Tenant, in.Phone)
		}
		if err != nil {
			var amb *AmbiguousCustomerError
			switch {
			case errors.As(err, &amb):
				writeJSON(w, http.StatusConflict, customerAssertRefusal{Error: amb.Error(), Code: CodeAmbiguousCustomer, Candidates: amb.Candidates})
			case errors.Is(err, ErrBadPhone):
				writeJSON(w, http.StatusUnprocessableEntity, customerAssertRefusal{Error: ErrBadPhone.Error(), Code: CodeBadPhone})
			default:
				d.Logger().Warn("appkit: a customer assertion failed", "err", err) // never the phone number
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
			}
			return
		}
		if !matched {
			cu = ""
		}
		writeJSON(w, http.StatusOK, customerAssertAnswer{Customer: cu, Matched: matched})
	})
}

// countBacklinks is the count for delete protection: the provider's own when it keeps one, else its backlinks as a tenant
// administrator sees them (what the platform's registry does for an app in its process).
func countBacklinks(r *http.Request, bl BacklinkProvider, tenant, ref string) (int, error) {
	if bc, ok := bl.(BacklinkCounter); ok {
		return bc.CountBacklinks(r.Context(), tenant, ref)
	}
	admin := Caller{Tenant: tenant, User: "system:refcheck", Name: "system", Roles: []string{RoleTenantAdmin}}
	items, err := bl.Backlinks(r.Context(), admin, ref, 100)
	return len(items), err
}

// serveHandedOver replays a request handed over by the asking app's page (HTTPCall) on the provider's handler, as the person of the
// assertion: the gateway's headers say who it is, as they do for a request that came through the gateway (Inbound set them).
func serveHandedOver(mux *http.ServeMux, path string, h http.Handler) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var in HTTPCall
		c, ok := viewer(w, r, &in)
		if !ok {
			return
		}
		if !strings.HasPrefix(in.Path, "/api/") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a page's request"})
			return
		}
		target := in.Path
		if in.Query != "" {
			target += "?" + in.Query
		}
		req := httptest.NewRequestWithContext(r.Context(), in.Method, target, bytes.NewReader(in.Body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderTenant, c.Tenant)
		req.Header.Set(HeaderUser, c.User)
		req.Header.Set(HeaderRoles, strings.Join(c.Roles, ","))
		if c.Name != "" {
			req.Header.Set(HeaderName, c.Name)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		writeJSON(w, http.StatusOK, HTTPAnswer{Status: rec.Code, ContentType: rec.Header().Get("Content-Type"), Body: rec.Body.Bytes()})
	})
}
