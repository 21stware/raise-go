package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// EntitlementClient is the entitlement service as an app uses it without sharing a transaction with the platform (ADR-0066 §2,
// ADR-0130 §3.2 H4): each call is a request of its own. The writes are idempotent where the service makes them so (Grant and Redeem
// by Key, Void of a void redemption, a Granter by its item's Key); an app orders its own writes around them (its own record first,
// marked pending, then the call with a key derived from that record, then the confirmation).
//
// Dry returns a client whose writes are previews: they run and answer as the real ones would, and nothing stays (an op's dry-run must
// not write in another service). Reads are the same in both.
type EntitlementClient interface {
	CreateTemplate(ctx context.Context, c Caller, req TemplateReq) (EntTemplate, error)
	UpdateTemplate(ctx context.Context, c Caller, id string, req TemplateReq) (EntTemplate, error)
	Template(ctx context.Context, c Caller, id string) (EntTemplate, error)

	Grant(ctx context.Context, c Caller, req GrantReq) (GrantResult, error)
	// GrantVia issues through the Granter registered under name (a course package by the membership app); ErrNoGranter when none.
	GrantVia(ctx context.Context, c Caller, granter string, item GrantItem) (string, error)
	// HasGranter says whether a Granter is registered under name.
	HasGranter(name string) bool

	Get(ctx context.Context, c Caller, id string) (EntGrant, error)
	// Grants lists a holder's grants (status "" for all).
	Grants(ctx context.Context, c Caller, holder, status string) ([]EntGrant, error)
	Redeem(ctx context.Context, c Caller, req RedeemReq) (RedeemResult, error)
	Void(ctx context.Context, c Caller, redemptionID, reason string) (RedeemResult, error)

	// Reserve, Confirm and Cancel are a redemption in two steps (ADR-0130 §3.2 H4): the use is kept aside, the app writes its own record,
	// then confirms; what it never confirms is released when the reservation expires.
	Reserve(ctx context.Context, c Caller, req ReserveReq) (EntReservation, error)
	Confirm(ctx context.Context, c Caller, reservationID string) (RedeemResult, error)
	Cancel(ctx context.Context, c Caller, reservationID, reason string) (EntReservation, error)
	Reservation(ctx context.Context, c Caller, reservationID string) (EntReservation, error)

	Refund(ctx context.Context, c Caller, req EntRefundReq) (EntRefundResult, error)
	Transfer(ctx context.Context, c Caller, req TransferReq) (EntGrant, error)
	// RelinkStore rewrites a legacy store id to the organisation's store (unit) in the calling app's own templates and grants and in
	// their redemptions (ADR-0057). Over the app API the platform names the owner, so ownerApp is not sent.
	RelinkStore(ctx context.Context, c Caller, ownerApp, old, unit string) error

	Dry() EntitlementClient
}

// EntitlementCentre is the whole entitlement service an app reaches over /app/v1/entitlements (ADR-0130 P11): EntitlementClient with
// the Granters' names, choices and refunds, the template list and the voucher lookup (the platform's firstparty.EntitlementClient).
// EntitlementClient stays the narrow contract other implementations keep.
type EntitlementCentre interface {
	EntitlementClient
	// GranterNames lists the names Granters are registered under, sorted.
	GranterNames(ctx context.Context, c Caller) ([]string, error)
	// GranterChoices lists what the Granter registered under name can issue; empty when it is unknown or does not list.
	GranterChoices(ctx context.Context, c Caller, name string) ([]GranterChoice, error)
	// GranterRefund asks the Granter registered under name to take back what it issued: ErrNoGranter when none, ErrNotRefundable
	// when it does not take things back. Preview answers the amount and takes nothing back (a dry client always previews); Key makes
	// it idempotent.
	GranterRefund(ctx context.Context, c Caller, name string, req GranterRefund) (GranterRefundResult, error)
	// Templates lists the tenant's templates (f.OwnerApp: one app's; f.ActiveOnly: the active ones).
	Templates(ctx context.Context, c Caller, f TemplateFilter) ([]EntTemplate, error)
	// ByCode finds a voucher by its code (offline redemption); an unknown and a malformed code answer the same ErrNotFound.
	ByCode(ctx context.Context, c Caller, code string) (EntGrant, error)
}

// EntitlementClient answers d.EntitlementAPI (Client.Entitlements); nil when it is not wired.
func (d Deps) EntitlementClient() EntitlementClient { return d.EntitlementAPI }

// EntitlementCentre is d.EntitlementAPI when it is the whole service (Client.Entitlements is one); nil otherwise.
func (d Deps) EntitlementCentre() EntitlementCentre {
	ec, _ := d.EntitlementAPI.(EntitlementCentre)
	return ec
}

// Entitlements is the entitlement service over /app/v1/entitlements (ADR-0130 §3.2 H4), with the meaning of the platform's in-process
// client: a refusal in words comes back as *EntRefused, a dry client sends K3-Dry-Run: 1.
func (cl *Client) Entitlements() EntitlementClient { return ents{cl: cl} }

// EntitlementCentre is Entitlements with the P11 calls (EntitlementCentre); its Dry client is one too.
func (cl *Client) EntitlementCentre() EntitlementCentre { return ents{cl: cl} }

type ents struct {
	cl  *Client
	dry bool
}

var _ EntitlementCentre = ents{}

func (e ents) Dry() EntitlementClient { e.dry = true; return e }

// call is Client.call with the dry-run header and the refusals of the entitlement service.
func (e ents) call(ctx context.Context, c Caller, method, path string, q url.Values, in, out any) error {
	if e.dry {
		ctx = withHeader(ctx, HeaderDryRun, "1")
	}
	rep, err := e.cl.do(ctx, c, method, path, q, in)
	if err != nil {
		return err
	}
	if rep.status == http.StatusUnprocessableEntity {
		if r := refusalOf(rep.body); r != nil {
			return r
		}
	}
	if err := mapStatus(rep.status, rep.hdr, rep.body); err != nil {
		return err
	}
	if out == nil || len(rep.body) == 0 {
		return nil
	}
	return unmarshalJSON(rep.body, out)
}

// refusalOf reads the envelope of a refusal in words ({error: {message, details: {refused, items}}}).
func refusalOf(body []byte) *EntRefused {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Details struct {
				Refused bool         `json:"refused"`
				Items   []EntRefusal `json:"items"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || !env.Error.Details.Refused {
		return nil
	}
	return &EntRefused{Message: env.Error.Message, Items: env.Error.Details.Items}
}

const entBase = "/app/v1/entitlements"

func (e ents) CreateTemplate(ctx context.Context, c Caller, req TemplateReq) (t EntTemplate, err error) {
	err = e.call(ctx, c, "POST", entBase+"/templates", nil, req, &t)
	return
}

func (e ents) UpdateTemplate(ctx context.Context, c Caller, id string, req TemplateReq) (t EntTemplate, err error) {
	err = e.call(ctx, c, "PUT", entBase+"/templates/"+pathEscape(id), nil, req, &t)
	return
}

func (e ents) Template(ctx context.Context, c Caller, id string) (t EntTemplate, err error) {
	err = e.call(ctx, c, "GET", entBase+"/templates/"+pathEscape(id), nil, nil, &t)
	return
}

func (e ents) Grant(ctx context.Context, c Caller, req GrantReq) (r GrantResult, err error) {
	err = e.call(ctx, c, "POST", entBase+"/grants", nil, req, &r)
	return
}

func (e ents) GrantVia(ctx context.Context, c Caller, granter string, item GrantItem) (string, error) {
	var out struct {
		Ref string `json:"ref"`
	}
	err := e.call(ctx, c, "POST", entBase+"/granters/"+pathEscape(granter)+"/grants", nil, item, &out)
	if errors.Is(err, ErrNotFound) && strings.Contains(err.Error(), "no app issues") {
		return "", ErrNoGranter
	}
	return out.Ref, err
}

func (e ents) HasGranter(name string) bool {
	var out struct {
		Names []string `json:"names"`
	}
	// the app's own call (no person): which Granters the platform has (a short call; an error reads as "no")
	if err := e.call(context.Background(), Caller{}, "GET", entBase+"/granters", nil, nil, &out); err != nil {
		return false
	}
	for _, n := range out.Names {
		if n == name {
			return true
		}
	}
	return false
}

func (e ents) GranterNames(ctx context.Context, c Caller) ([]string, error) {
	var out struct {
		Names []string `json:"names"`
	}
	if err := e.call(ctx, c, "GET", entBase+"/granters", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Names == nil {
		out.Names = []string{}
	}
	return out.Names, nil
}

func (e ents) GranterChoices(ctx context.Context, c Caller, name string) ([]GranterChoice, error) {
	var out struct {
		Data []GranterChoice `json:"data"`
	}
	if err := e.call(ctx, c, "GET", entBase+"/granters/"+pathEscape(name)+"/choices", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		out.Data = []GranterChoice{}
	}
	return out.Data, nil
}

// GranterRefund: a dry client previews (the platform's answer to K3-Dry-Run is a preview too); the platform's words for "nobody
// issues that" and "it does not take back" come back as ErrNoGranter and ErrNotRefundable.
func (e ents) GranterRefund(ctx context.Context, c Caller, name string, req GranterRefund) (r GranterRefundResult, err error) {
	req.Preview = req.Preview || e.dry
	err = e.call(ctx, c, "POST", entBase+"/granters/"+pathEscape(name)+"/refunds", nil, req, &r)
	switch {
	case errors.Is(err, ErrNotFound) && strings.Contains(err.Error(), "no app issues"):
		return r, fmt.Errorf("%w: %s", ErrNoGranter, name)
	case errors.Is(err, ErrConflict) && strings.Contains(err.Error(), ErrNotRefundable.Error()):
		return r, ErrNotRefundable
	}
	return r, err
}

func (e ents) Templates(ctx context.Context, c Caller, f TemplateFilter) ([]EntTemplate, error) {
	q := url.Values{}
	if f.OwnerApp != "" {
		q.Set("ownerApp", f.OwnerApp)
	}
	if f.ActiveOnly {
		q.Set("activeOnly", "true")
	}
	var out struct {
		Data []EntTemplate `json:"data"`
	}
	if err := e.call(ctx, c, "GET", entBase+"/templates", q, nil, &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		out.Data = []EntTemplate{}
	}
	return out.Data, nil
}

func (e ents) ByCode(ctx context.Context, c Caller, code string) (g EntGrant, err error) {
	err = e.call(ctx, c, "GET", entBase+"/codes/"+pathEscape(code), nil, nil, &g)
	return
}

func (e ents) Get(ctx context.Context, c Caller, id string) (g EntGrant, err error) {
	err = e.call(ctx, c, "GET", entBase+"/grants/"+pathEscape(id), nil, nil, &g)
	return
}

func (e ents) Grants(ctx context.Context, c Caller, holder, status string) ([]EntGrant, error) {
	q := url.Values{"holder": {holder}}
	if status != "" {
		q.Set("status", status)
	}
	var out struct {
		Data []EntGrant `json:"data"`
	}
	err := e.call(ctx, c, "GET", entBase+"/grants", q, nil, &out)
	return out.Data, err
}

func (e ents) Redeem(ctx context.Context, c Caller, req RedeemReq) (r RedeemResult, err error) {
	err = e.call(ctx, c, "POST", entBase+"/redemptions", nil, req, &r)
	return
}

func (e ents) Void(ctx context.Context, c Caller, redemptionID, reason string) (r RedeemResult, err error) {
	err = e.call(ctx, c, "POST", entBase+"/redemptions/"+pathEscape(redemptionID)+"/void", nil, map[string]string{"reason": reason}, &r)
	return
}

func (e ents) Reserve(ctx context.Context, c Caller, req ReserveReq) (r EntReservation, err error) {
	body := struct {
		RedeemReq
		TTLSeconds int `json:"ttlSeconds,omitempty"`
	}{req.RedeemReq, int(req.TTL.Seconds())}
	err = e.call(ctx, c, "POST", entBase+"/reservations", nil, body, &r)
	return
}

func (e ents) Confirm(ctx context.Context, c Caller, id string) (r RedeemResult, err error) {
	err = e.call(ctx, c, "POST", entBase+"/reservations/"+pathEscape(id)+"/confirm", nil, struct{}{}, &r)
	return
}

func (e ents) Cancel(ctx context.Context, c Caller, id, reason string) (r EntReservation, err error) {
	err = e.call(ctx, c, "POST", entBase+"/reservations/"+pathEscape(id)+"/cancel", nil, map[string]string{"reason": reason}, &r)
	return
}

func (e ents) Reservation(ctx context.Context, c Caller, id string) (r EntReservation, err error) {
	err = e.call(ctx, c, "GET", entBase+"/reservations/"+pathEscape(id), nil, nil, &r)
	return
}

func (e ents) Refund(ctx context.Context, c Caller, req EntRefundReq) (r EntRefundResult, err error) {
	id := req.Grant
	req.Grant = ""
	err = e.call(ctx, c, "POST", entBase+"/grants/"+pathEscape(id)+"/refund", nil, req, &r)
	return
}

// RelinkStore: the platform names the owner (the calling app), so ownerApp is not sent.
func (e ents) RelinkStore(ctx context.Context, c Caller, _ string, old, unit string) error {
	return e.call(ctx, c, "POST", entBase+"/stores/relink", nil, map[string]string{"from": old, "to": unit}, nil)
}

func (e ents) Transfer(ctx context.Context, c Caller, req TransferReq) (g EntGrant, err error) {
	id := req.Grant
	req.Grant = ""
	err = e.call(ctx, c, "POST", entBase+"/grants/"+pathEscape(id)+"/transfer", nil, req, &g)
	return
}
