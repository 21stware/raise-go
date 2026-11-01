package appkit

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The platform's calls to an app that runs on its own (ADR-0130 §2.4), the app's side; the platform's side is its internal/providers
// (Remote), whose paths and bodies these are. Every /k3/providers, /k3/site-actions, /k3/services and /k3/events call must be the
// platform's (Inbound verified its assertion) and for the tenant the assertion names; ops (/k3/apps/{id}/ops/…) check the draft
// service's assertion themselves; the rest of the paths are the app's own routes.
//
//	POST /k3/providers/sources                  {}                         → {"sources": [Source]}                  viewer's permissions
//	POST /k3/providers/rows                     {source, limit}            → {"rows": [{…}]}
//	POST /k3/providers/referrer                 {site, visitor}            → {"customer": id}
//	POST /k3/providers/links/search             {query, limit}             → {"items": [Link]}
//	POST /k3/providers/links/resolve            {ref}                      → {"found": bool, "link": Link}
//	POST /k3/providers/referral-codes/code      {promoter, origin}         → ReferralCode
//	POST /k3/providers/referral-codes/promoter  {code}                     → Promoter
//	POST /k3/providers/deals                    DealCall                   → 204 · 422 refused · 423 waits for its sale · else retried
//	POST {service endpoint of event:deal}       {input: DealCall, …}       → the same, 423 with Retry-After
//	POST /k3/site-actions/{name}                SiteActionReq              → SiteActionAnswer
//	POST /k3/events/{kind}                      DataEventCall              → 204
const (
	PathSources          = "/k3/providers/sources"
	PathRows             = "/k3/providers/rows"
	PathReferrer         = "/k3/providers/referrer"
	PathLinksSearch      = "/k3/providers/links/search"
	PathLinksResolve     = "/k3/providers/links/resolve"
	PathReferralCode     = "/k3/providers/referral-codes/code"
	PathReferralPromoter = "/k3/providers/referral-codes/promoter"
	PathDeals            = "/k3/providers/deals"
	PathSiteActionFmt    = "/k3/site-actions/"
)

// Serve is the whole handler of app a running on its own, behind Inbound. What it answers for the platform is what a's manifest
// declares (Providers, Services, SiteActions, Events) of what its code provides (DataProviding, LinkProviding, Attributing,
// DealHandling, SiteActing, DataEventHandler, Opped, TableProviding, the P11 kinds' ...ing interfaces of providers.go, Granting, ApprovalAnswering,
// Settling); a provider a implements but does not declare is not served.
func Serve(a App, d Deps, keys k3.KeySource, version, base string) (http.Handler, error) {
	mux := http.NewServeMux()
	m := a.Manifest(version, base)
	if o, ok := a.(Opped); ok {
		oh, err := OpsHandler(m, o.Ops(d), keys, PGIdempotency{DB: d.DB, Schema: d.Schema})
		if err != nil {
			return nil, err
		}
		mux.Handle("/k3/apps/"+a.ID()+"/", oh)
	}
	serveEvents(mux, a, d, m)    // tenant.deleted, site.deleted (ADR-0130 §2.5)
	serveSchedules(mux, a, d, m) // the services that run on the platform's clock
	serveAppEvents(mux, a, d, m) // the services of other apps' events
	serveConsent(mux, a, d, m)   // the service of the customers' consent changes
	decl := m.Providers
	if p, ok := a.(DealHandling); ok && decl.Has("deals") {
		if dh := p.HandlesDeals(d); dh != nil {
			mux.HandleFunc("POST "+PathDeals, func(w http.ResponseWriter, r *http.Request) { // without the service runtime (P1)
				var in DealCall
				if !platformJSON(w, r, k3.ViaEvent, &in) || !sameTenant(w, r, in.Caller.Tenant) {
					return
				}
				err := dh.HandleDeal(r.Context(), in.Caller, in.Event)
				switch {
				case err == nil:
					w.WriteHeader(http.StatusNoContent)
				case errors.Is(err, ErrDealPending):
					writeJSON(w, http.StatusLocked, map[string]string{"error": err.Error()})
				case refusal(err):
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
				default:
					d.Logger().Warn("appkit: a deal event failed", "key", in.Event.Key, "err", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
				}
			})
			if sv, declared := m.ServiceFor(k3.EventDeal); declared {
				serveDeals(mux, sv.Endpoint, dh, d)
			}
		}
	}
	if sa, ok := a.(SiteActing); ok && len(m.SiteActions) > 0 {
		p := sa.SiteActions(d)
		declared := map[string]bool{}
		for _, n := range m.SiteActions {
			declared[n] = true
		}
		mux.HandleFunc("POST "+PathSiteActionFmt+"{name}", func(w http.ResponseWriter, r *http.Request) {
			var in SiteActionReq
			if !platformJSON(w, r, k3.ViaSiteAction, &in) || !sameTenant(w, r, in.Tenant) {
				return
			}
			in.Action = r.PathValue("name")
			if !declared[in.Action] {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such site action"})
				return
			}
			res, err := p.SiteAction(r.Context(), in)
			if err != nil {
				d.Logger().Warn("appkit: a site action failed", "action", in.Action, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
				return
			}
			body, err := json.Marshal(res.Body)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
				return
			}
			writeJSON(w, http.StatusOK, SiteActionAnswer{Status: res.Status, Body: body, CacheSeconds: res.CacheSeconds})
		})
	}
	if p, ok := a.(Attributing); ok {
		rf := p.Referrers(d)
		if rf != nil && decl.Has("referrer") {
			mux.HandleFunc("POST "+PathReferrer, func(w http.ResponseWriter, r *http.Request) {
				var in struct{ Site, Visitor string }
				if !platformJSON(w, r, k3.ViaProvider, &in) {
					return
				}
				tenant, _ := PlatformCall(r.Context())
				id, err := rf.ReferrerOf(r.Context(), Caller{Tenant: tenant, User: "visitor", Name: "站点访客"}, in.Site, in.Visitor)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"customer": id})
			})
		}
		if rc, ok := rf.(ReferralCodes); ok && rc != nil && decl.Has("referralCodes") {
			serveReferralCodes(mux, rc, d)
		}
	}
	if p, ok := a.(DataProviding); ok && decl.Has("sources") {
		if dp := p.DataProvider(d); dp != nil {
			mux.HandleFunc("POST "+PathSources, func(w http.ResponseWriter, r *http.Request) {
				var in struct{}
				c, ok := viewer(w, r, &in)
				if !ok {
					return
				}
				src, err := dp.Sources(r.Context(), c)
				if err != nil {
					writeProviderErr(w, d, err)
					return
				}
				if src == nil {
					src = []Source{}
				}
				writeJSON(w, http.StatusOK, map[string]any{"sources": src})
			})
			mux.HandleFunc("POST "+PathRows, func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Source string `json:"source"`
					Limit  int    `json:"limit"`
				}
				c, ok := viewer(w, r, &in)
				if !ok {
					return
				}
				rows, err := dp.Rows(r.Context(), c, in.Source, in.Limit)
				if err != nil {
					writeProviderErr(w, d, err)
					return
				}
				if rows == nil {
					rows = []map[string]any{}
				}
				writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
			})
		}
	}
	if p, ok := a.(LinkProviding); ok && decl.Has("links") {
		if lp := p.LinkProvider(d); lp != nil {
			mux.HandleFunc("POST "+PathLinksSearch, func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				c, ok := viewer(w, r, &in)
				if !ok {
					return
				}
				items, err := lp.SearchLinks(r.Context(), c, in.Query, in.Limit)
				if err != nil {
					writeProviderErr(w, d, err)
					return
				}
				if items == nil {
					items = []Link{}
				}
				writeJSON(w, http.StatusOK, map[string]any{"items": items})
			})
			mux.HandleFunc("POST "+PathLinksResolve, func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Ref string `json:"ref"`
				}
				c, ok := viewer(w, r, &in)
				if !ok {
					return
				}
				l, found, err := lp.ResolveLink(r.Context(), c, in.Ref)
				if err != nil {
					writeProviderErr(w, d, err)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"found": found, "link": l})
			})
		}
	}
	serveKinds(mux, a, decl, d)        // customerAssert, people, backlinks, holdings, agenda, work, desk, till, handover, holds, holdWatch (serve_kinds.go)
	serveTables(mux, a, decl, d)       // providers.tables (tables.go)
	serveGranters(mux, a, m, d)        // entitlements.grantable (serve_granters.go)
	serveApprovals(mux, a, m, d, keys) // PREPARE and DECIDED of the kinds the manifest declares (serve_approvals.go)
	servePayments(mux, a, m, d)        // the service of event:payment (serve_payments.go)
	mux.Handle("/", a.Handler(d))
	return Inbound(keys, a.ID(), mux), nil
}

// serveReferralCodes answers providers.referralCodes for a person (the assertion's).
func serveReferralCodes(mux *http.ServeMux, rc ReferralCodes, d Deps) {
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
	type codeIn struct {
		Promoter Promoter `json:"promoter"`
		Origin   string   `json:"origin"`
	}
	person(PathReferralCode, func() any { return &codeIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
		v := in.(*codeIn)
		return rc.CodeOf(r.Context(), c, v.Promoter, v.Origin)
	})
	person(PathReferralPromoter, func() any { return &struct{ Code string }{} }, func(r *http.Request, c Caller, in any) (any, error) {
		return rc.PromoterOf(r.Context(), c, in.(*struct{ Code string }).Code)
	})
}

// serveDeals is the app's service of event:deal (the platform's providers.ServiceDeals): a run's input is a DealCall, handed to the
// app's deal handler; the answer is the platform's DealAnswer's (423 + Retry-After for a refund whose sale has not arrived).
func serveDeals(mux *http.ServeMux, endpoint string, dh DealHandler, d Deps) {
	mux.HandleFunc("POST "+endpoint, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input DealCall `json:"input"`
		}
		if !platformJSON(w, r, k3.ViaService, &in) || !sameTenant(w, r, in.Input.Caller.Tenant) {
			return
		}
		err := dh.HandleDeal(r.Context(), in.Input.Caller, in.Input.Event)
		if err != nil && !errors.Is(err, ErrDealPending) {
			d.Logger().Warn("appkit: a deal event was not taken in", "key", in.Input.Event.Key, "err", err)
		}
		status, retry, body := dealAnswer(err)
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
		}
		if len(body) == 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

// viewer reads a provider call made for a person (a 分析 viewer, a picker's user): the person is the assertion's, never the body's.
func viewer(w http.ResponseWriter, r *http.Request, v any) (Caller, bool) {
	if !platformJSON(w, r, k3.ViaProvider, v) {
		return Caller{}, false
	}
	p, _ := PlatformOf(r.Context())
	if p.Caller.User == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a provider answers for a person"})
		return Caller{}, false
	}
	return p.Caller, true
}

func writeProviderErr(w http.ResponseWriter, d Deps, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	default:
		d.Logger().Warn("appkit: a provider failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
	}
}

// platformJSON reads the body of a platform call of the kind via, refusing anyone else.
func platformJSON(w http.ResponseWriter, r *http.Request, via string, v any) bool {
	p, ok := PlatformOf(r.Context())
	if !ok || p.Via != via {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "only the platform calls this"})
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable"})
		return false
	}
	return true
}

// sameTenant refuses a call whose body names another tenant than the platform's assertion.
func sameTenant(w http.ResponseWriter, r *http.Request, tenant string) bool {
	if t, _ := PlatformCall(r.Context()); t != tenant {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "another tenant"})
		return false
	}
	return true
}

// refusal reports an error the platform must not retry (a 4xx of the app's own words).
func refusal(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status < 500
	}
	return errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
