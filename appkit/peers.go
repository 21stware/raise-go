package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/21stware/raise-go/provide"
)

// Peers is what an app asks of the other apps' providers through the platform (ADR-0130 §2.4, P11): the redemption desk and the till
// it serves on its own host with the same person, the resource holds, the promotion codes. Each answers only for an app the tenant has
// installed and switched on, and only as the caller sees it. The app never reaches the other app itself: Client.Peers asks the
// platform's /app/v1/peers. The contracts are raise-go/provide's.
type Peers interface {
	// Desk is the redemption desk (providers.desk), false when no installed app serves one.
	Desk(ctx context.Context, tenant string) (provide.DeskProvider, bool)
	// Till is the till (providers.till), false when no installed app serves one.
	Till(ctx context.Context, tenant string) (provide.TillProvider, bool)
	// Holds is the resource holds (providers.holds), provide.ErrNoResourceApp when no installed app offers them.
	Holds(ctx context.Context, tenant string) (provide.ResourceHolds, error)
	// ReferralCodesFor is the promotion codes (providers.referralCodes), nil when no installed app offers them.
	ReferralCodesFor(ctx context.Context, tenant string) ReferralCodes
}

// ErrNoResourceApp: the tenant has no resource app (or it offers no holds); the caller books without holds (raise-go/provide's).
var ErrNoResourceApp = provide.ErrNoResourceApp

// Peers is the other apps' providers of the app's Deps (Deps.PeersAPI); with none wired nothing is there.
func (d Deps) Peers() Peers {
	if d.PeersAPI != nil {
		return d.PeersAPI
	}
	return noPeers{}
}

type noPeers struct{}

func (noPeers) Desk(context.Context, string) (provide.DeskProvider, bool) { return nil, false }
func (noPeers) Till(context.Context, string) (provide.TillProvider, bool) { return nil, false }
func (noPeers) Holds(context.Context, string) (provide.ResourceHolds, error) {
	return nil, provide.ErrNoResourceApp
}
func (noPeers) ReferralCodesFor(context.Context, string) ReferralCodes { return nil }

// Peers is the other apps' providers over /app/v1/peers (ADR-0130 §2.4, P11 S1). Whether a provider is there is asked once per lookup
// (Desk, Till, Holds, ReferralCodesFor); a lookup the platform did not answer counts as "none" for the desk and the till (the page then
// offers nothing), but gives the holds and the codes anyway, so their calls fail as errors (a hold stays pending and is tried again)
// instead of reading as "no resource app".
func (cl *Client) Peers() Peers { return peers{cl} }

type peers struct{ cl *Client }

var _ Peers = peers{}

// The platform's answers to the lookups (its appapi.DeskAnswer, TillAnswer).
type deskAnswer struct {
	Available bool `json:"available"`
	Redeem    bool `json:"redeem"`
	Void      bool `json:"void"`
}

type tillAnswer struct {
	Available bool `json:"available"`
	Sell      bool `json:"sell"`
}

// peerLookup is a call made for the tenant only: the person's page session when the context carries one (a request of the page), the
// app's own token otherwise.
func peerLookup(tenant string) Caller { return Caller{Tenant: tenant} }

func (p peers) Desk(ctx context.Context, tenant string) (provide.DeskProvider, bool) {
	var out deskAnswer
	if err := p.cl.call(ctx, peerLookup(tenant), "GET", "/app/v1/peers/desk", nil, nil, &out); err != nil || !out.Available {
		return nil, false
	}
	return remoteDesk{p.cl}, true
}

func (p peers) Till(ctx context.Context, tenant string) (provide.TillProvider, bool) {
	var out tillAnswer
	if err := p.cl.call(ctx, peerLookup(tenant), "GET", "/app/v1/peers/till", nil, nil, &out); err != nil || !out.Available {
		return nil, false
	}
	return remoteTill{p.cl}, true
}

func (p peers) Holds(ctx context.Context, tenant string) (provide.ResourceHolds, error) {
	var out struct {
		Available bool `json:"available"`
	}
	err := p.cl.call(ctx, peerLookup(tenant), "GET", "/app/v1/peers/holds", nil, nil, &out)
	if err == nil && !out.Available {
		return nil, provide.ErrNoResourceApp
	}
	return remoteHolds{p.cl}, nil
}

func (p peers) ReferralCodesFor(ctx context.Context, tenant string) ReferralCodes {
	var out struct {
		Available bool `json:"available"`
	}
	err := p.cl.call(ctx, peerLookup(tenant), "GET", "/app/v1/peers/referral-codes", nil, nil, &out)
	if err == nil && !out.Available {
		return nil
	}
	return remoteReferrals{p.cl}
}

// ---- the desk and the till: the page's request, handed over as the same person ----

type remoteDesk struct{ cl *Client }

func (d remoteDesk) DeskHandler() http.Handler { return handOver(d.cl, "/app/v1/peers/desk/http") }

func (d remoteDesk) DeskAccess(ctx context.Context, c Caller) (redeem, void bool) {
	var out deskAnswer
	if err := d.cl.call(ctx, c, "GET", "/app/v1/peers/desk", nil, nil, &out); err != nil {
		return false, false // a desk that does not answer lets nobody in
	}
	return out.Redeem, out.Void
}

type remoteTill struct{ cl *Client }

func (t remoteTill) TillHandler() http.Handler { return handOver(t.cl, "/app/v1/peers/till/http") }

func (t remoteTill) TillAccess(ctx context.Context, c Caller) bool {
	var out tillAnswer
	if err := t.cl.call(ctx, c, "GET", "/app/v1/peers/till", nil, nil, &out); err != nil {
		return false
	}
	return out.Sell
}

// handOver hands the page's request to the platform at path, which replays it on the provider as the person of the page session (the
// token on the request's context, Inbound): the app never acts for the person with its own token here.
func handOver(cl *Client, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := CallerOf(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		if p := personToken(r.Context()); p.token == "" || p.tenant != c.Tenant {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only a request of the person's page is handed over"})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable"})
			return
		}
		in := HTTPCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		if len(bytes.TrimSpace(raw)) > 0 {
			if !json.Valid(raw) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable"})
				return
			}
			in.Body = raw
		}
		var ans HTTPAnswer
		if err := cl.call(r.Context(), c, "POST", path, nil, in, &ans); err != nil {
			status, msg := http.StatusBadGateway, "the provider did not answer"
			var he *HTTPError
			switch {
			case errors.Is(err, ErrNotFound):
				status, msg = http.StatusNotFound, "no installed app serves it"
			case errors.Is(err, ErrForbidden):
				status, msg = http.StatusForbidden, "forbidden"
			case errors.Is(err, ErrInvalid):
				status, msg = http.StatusBadRequest, "not a request the provider serves"
			case errors.As(err, &he) && he.Status == http.StatusRequestEntityTooLarge:
				status, msg = he.Status, "the request is too large"
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if ans.ContentType != "" {
			w.Header().Set("Content-Type", ans.ContentType)
		}
		if ans.Status == 0 {
			ans.Status = http.StatusOK
		}
		w.WriteHeader(ans.Status)
		_, _ = w.Write(ans.Body)
	})
}

// ---- resource holds ----

type remoteHolds struct{ cl *Client }

func (h remoteHolds) Holdable(ctx context.Context, c Caller) ([]provide.HoldableItem, error) {
	var out struct {
		Items []provide.HoldableItem `json:"items"`
	}
	err := h.cl.call(ctx, c, "GET", "/app/v1/peers/holds/holdable", nil, nil, &out)
	return out.Items, noProvider(err)
}

// The bodies of the hold routes (the platform's appapi.HoldsFreeReq, HoldsHoldReq, HoldsMoveReq, HoldsReleaseReq).
type (
	holdsFreeReq struct {
		Items  []string  `json:"items"`
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
		Except string    `json:"except,omitempty"`
	}
	holdsHoldReq struct {
		Source string    `json:"source"`
		Title  string    `json:"title,omitempty"`
		Items  []string  `json:"items"`
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
	}
	holdsMoveReq struct {
		HoldID string    `json:"holdId"`
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
	}
	holdsReleaseReq struct {
		HoldID string `json:"holdId"`
		Reason string `json:"reason,omitempty"`
	}
)

func (h remoteHolds) Free(ctx context.Context, c Caller, items []string, from, to time.Time, except string) ([]provide.FreeItem, error) {
	var out struct {
		Items []provide.FreeItem `json:"items"`
	}
	err := h.cl.call(ctx, c, "POST", "/app/v1/peers/holds/free", nil, holdsFreeReq{Items: items, From: from, To: to, Except: except}, &out)
	return out.Items, noProvider(err)
}

func (h remoteHolds) Hold(ctx context.Context, c Caller, r provide.HoldRequest) ([]provide.HoldResult, error) {
	var out struct {
		Items []provide.HoldResult `json:"items"`
	}
	err := h.cl.call(ctx, c, "POST", "/app/v1/peers/holds/hold", nil, holdsHoldReq{Source: r.Source, Title: r.Title, Items: r.Items, From: r.From, To: r.To}, &out)
	return out.Items, noProvider(err)
}

func (h remoteHolds) Move(ctx context.Context, c Caller, holdID string, from, to time.Time) error {
	err := h.cl.call(ctx, c, "POST", "/app/v1/peers/holds/move", nil, holdsMoveReq{HoldID: holdID, From: from, To: to}, nil)
	if errors.Is(err, ErrConflict) && strings.Contains(err.Error(), provide.ErrHoldConflict.Error()) { // 409 hold_conflict, not another conflict
		return provide.ErrHoldConflict
	}
	return noProvider(err)
}

func (h remoteHolds) Release(ctx context.Context, c Caller, holdID, reason string) error {
	err := h.cl.call(ctx, c, "POST", "/app/v1/peers/holds/release", nil, holdsReleaseReq{HoldID: holdID, Reason: reason}, nil)
	return noProvider(err)
}

// noProvider turns the platform's "no installed app offers it" (404 no_provider) back into provide.ErrNoResourceApp.
func noProvider(err error) error {
	if errors.Is(err, ErrNotFound) && strings.Contains(err.Error(), "no installed app offers") {
		return provide.ErrNoResourceApp
	}
	return err
}

// ---- promotion codes ----

type remoteReferrals struct{ cl *Client }

func (x remoteReferrals) CodeOf(ctx context.Context, c Caller, p Promoter, origin string) (ReferralCode, error) {
	var out ReferralCode
	body := struct {
		Promoter Promoter `json:"promoter"`
		Origin   string   `json:"origin,omitempty"`
	}{p, origin}
	err := x.cl.call(ctx, c, "POST", "/app/v1/peers/referral-codes/code", nil, body, &out)
	return out, err
}

func (x remoteReferrals) PromoterOf(ctx context.Context, c Caller, code string) (Promoter, error) {
	var out Promoter
	body := struct {
		Code string `json:"code"`
	}{code}
	err := x.cl.call(ctx, c, "POST", "/app/v1/peers/referral-codes/promoter", nil, body, &out)
	return out, err
}
