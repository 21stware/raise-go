package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/21stware/raise-go/k3"
)

// Deal events (docs/deal-events.md, ADR-0048 §1, §2, §5). A deal — a sale, a refund, a void — is told to whoever cares (the
// customer centre, a campaign, a distribution scheme) through ONE contract, not through each app reading the seller's tables. An app
// that hears of deals implements DealHandling and declares a service of event:deal (DealService) and providers.deals: the platform
// delivers each event at least once, outside any transaction of its own, to Serve's endpoints.
//
// A deal event is NOT the books. Money is whatever 经营 holds (EnqueueLedger); an event answers "who closed a deal, from where". A
// subscriber must not derive accounts from events.

// DealPairWait is how long a refund or void may wait for its sale to reach a subscriber (see PairRefund).
const DealPairWait = 7 * 24 * time.Hour

// Deal kinds.
const (
	DealSale   = "sale"
	DealRefund = "refund"
	DealVoid   = "void"
)

// DealSource says which app made the deal and what it came from.
type DealSource struct {
	App string `json:"app"`
	Ref string `json:"ref"` // k3://order/o_1, k3://membership/package/pk_1 ...
}

// DealLine is one line of what was sold. SKU is an opaque product id.
type DealLine struct {
	SKU    string `json:"sku"`
	Name   string `json:"name"`
	Qty    int    `json:"qty"`
	Amount int64  `json:"amount_cents"`
}

// DealAttribution is where the deal came from. Every value is an internal reference or a campaign label, never a person's
// phone number, openid or e-mail (Validate refuses what it can recognise).
type DealAttribution struct {
	Channel  string            `json:"channel,omitempty"`
	Campaign string            `json:"campaign,omitempty"` // k3://campaign/<id>
	Site     string            `json:"site,omitempty"`
	Referrer string            `json:"referrer,omitempty"` // the distributor's customer id (cu_…)
	UTM      map[string]string `json:"utm,omitempty"`
	Voucher  string            `json:"voucher,omitempty"` // which voucher was used
}

// DealEvent is one deal. The seller fills it; Key and At default (EnqueueDeal) when left empty.
type DealEvent struct {
	// Key is the idempotency key, DealKey(source app, source ref, kind). A refund's Source.Ref names the refund itself
	// (k3://order/o_1/refunds/1), not the order, so that two partial refunds are two events.
	Key      string          `json:"key"`
	Kind     string          `json:"kind"` // sale | refund | void
	Source   DealSource      `json:"source"`
	Customer string          `json:"customer,omitempty"` // cu_…; empty for an anonymous walk-in
	Amount   int64           `json:"amount_cents"`       // paid after discounts; for a refund, the amount given back
	Currency string          `json:"currency,omitempty"` // "" = the tenant's
	Lines    []DealLine      `json:"lines,omitempty"`
	Attrib   DealAttribution `json:"attribution"`
	// Reverses names the sale a refund or void takes back (its Key).
	Reverses string `json:"reverses,omitempty"`
	// At is when the deal happened (business time, not delivery time).
	At time.Time `json:"at"`
}

// DealKey is the key of a deal event: source app, source reference, kind.
func DealKey(app, ref, kind string) string { return app + ":" + ref + ":" + kind }

const (
	dealMaxText  = 200
	dealMaxLines = 200
	dealMaxUTM   = 8
)

var (
	// A mainland mobile number (optionally +86), an international number, an e-mail, an 18-digit id card number.
	rePhone  = regexp.MustCompile(`(^|\D)(\+?86[- ]?)?1[3-9]\d[- ]?\d{4}[- ]?\d{4}(\D|$)`)
	reIntl   = regexp.MustCompile(`\+\d[\d -]{7,}\d`)
	reIDCard = regexp.MustCompile(`(^|\D)\d{17}[\dXx](\D|$)`)
	// A WeChat openid / unionid: 28 characters starting with o, mixing cases and digits.
	reOpenID = regexp.MustCompile(`[A-Za-z0-9_-]{28}`)
	reUTMKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// Internal references the platform makes: a customer id (cu_ + opaque characters) and a k3:// reference. Their opaque
	// parts are random and can contain a run of digits shaped like a mobile number (cu_…a13912345678b…), so they are checked
	// for their shape instead of with the personal-data net.
	reCustomerID = regexp.MustCompile(`^cu_[A-Za-z0-9]{1,64}$`)
	reK3Ref      = regexp.MustCompile(`^k3://[a-z][a-z0-9-]{0,39}(/[A-Za-z0-9_.:-]{1,80}){1,6}$`)
)

// checkRef accepts an internal reference by its shape; anything else is free text and goes through the personal-data net.
func checkRef(field, s string) error {
	if s == "" || reK3Ref.MatchString(s) || reCustomerID.MatchString(s) {
		return nil
	}
	return checkText(field, s)
}

// looksPersonal reports whether free text carries something that looks like a phone number, openid, e-mail or id card number.
// It is a cheap net, not a guarantee: the rule is that an event carries internal references and labels only, and the seller
// is responsible for that; this refuses the obvious slips (a customer's phone typed into a campaign name, an openid used as a
// referrer).
func looksPersonal(s string) bool {
	if rePhone.MatchString(s) || reIntl.MatchString(s) || reIDCard.MatchString(s) {
		return true
	}
	if strings.Contains(s, "@") {
		for _, f := range strings.Fields(s) {
			if a, err := mail.ParseAddress(strings.Trim(f, "<>,;()")); err == nil && strings.Contains(a.Address, "@") {
				return true
			}
		}
	}
	for _, m := range reOpenID.FindAllString(s, -1) {
		if m[0] == 'o' && strings.ToLower(m) != m && strings.ToUpper(m) != m && strings.ContainsAny(m, "0123456789") {
			return true
		}
	}
	return false
}

func checkText(field, s string) error {
	if len([]rune(s)) > dealMaxText {
		return Invalid("deal event: %s is longer than %d characters", field, dealMaxText)
	}
	if looksPersonal(s) {
		return Invalid("deal event: %s looks like personal data (phone, openid, e-mail or id number); an event carries references and labels only", field)
	}
	return nil
}

// Validate checks the event: its shape, the key, and that no free text looks like personal data.
func (e DealEvent) Validate() error {
	switch e.Kind {
	case DealSale, DealRefund, DealVoid:
	default:
		return Invalid("deal event: kind must be sale, refund or void")
	}
	if strings.TrimSpace(e.Source.App) == "" || !strings.HasPrefix(e.Source.Ref, "k3://") {
		return Invalid("deal event: the source needs its app and a k3:// reference")
	}
	if e.Key != DealKey(e.Source.App, e.Source.Ref, e.Kind) {
		return Invalid("deal event: the key must be source app:source ref:kind")
	}
	if e.Customer != "" && !reCustomerID.MatchString(e.Customer) {
		return Invalid("deal event: customer must be a customer id (cu_…)")
	}
	if e.Attrib.Referrer != "" && !reCustomerID.MatchString(e.Attrib.Referrer) {
		return Invalid("deal event: the referrer must be a customer id (cu_…)")
	}
	if e.Amount < 0 || (e.Kind == DealRefund && e.Amount == 0) {
		return Invalid("deal event: the amount is negative, or a refund of nothing")
	}
	if e.Currency != "" && (len(e.Currency) != 3 || strings.ToUpper(e.Currency) != e.Currency) {
		return Invalid("deal event: currency is empty (the tenant's) or a 3-letter code")
	}
	if e.At.IsZero() {
		return Invalid("deal event: at is when it happened")
	}
	if e.Kind == DealSale {
		if e.Reverses != "" {
			return Invalid("deal event: a sale reverses nothing")
		}
	} else if e.Reverses == "" || e.Reverses == e.Key || !strings.HasSuffix(e.Reverses, ":"+DealSale) {
		return Invalid("deal event: a refund or void names the sale it reverses")
	}
	if len(e.Lines) > dealMaxLines {
		return Invalid("deal event: at most %d lines", dealMaxLines)
	}
	for i, l := range e.Lines {
		if strings.TrimSpace(l.SKU) == "" || l.Qty <= 0 || l.Amount < 0 {
			return Invalid("deal event: line %d needs a sku, a quantity above zero and a non-negative amount", i+1)
		}
		if err := checkText(fmt.Sprintf("line %d sku", i+1), l.SKU); err != nil {
			return err
		}
		if err := checkText(fmt.Sprintf("line %d name", i+1), l.Name); err != nil {
			return err
		}
	}
	// references by their shape (customer, referrer, source ref; campaign, site and voucher when they are references), the rest
	// as free text: a random id must not be refused for looking like a phone number
	for field, v := range map[string]string{"source ref": e.Source.Ref, "campaign": e.Attrib.Campaign, "site": e.Attrib.Site,
		"referrer": e.Attrib.Referrer, "voucher": e.Attrib.Voucher, "customer": e.Customer} {
		if err := checkRef(field, v); err != nil {
			return err
		}
	}
	if err := checkText("channel", e.Attrib.Channel); err != nil {
		return err
	}
	if len(e.Attrib.UTM) > dealMaxUTM {
		return Invalid("deal event: at most %d utm entries", dealMaxUTM)
	}
	for k, v := range e.Attrib.UTM {
		if !reUTMKey.MatchString(k) {
			return Invalid("deal event: utm keys are lower-case words (utm_source …)")
		}
		if err := checkText("utm "+k, v); err != nil {
			return err
		}
	}
	return nil
}

// DealHandler receives deal events outside any transaction of the platform's: the shape a subscriber has when it runs in a process of
// its own (ADR-0066 §2, ADR-0065 §5), and the one an app moving there uses already. Delivery is at least once — the handler may see
// an event again after it applied it (the delivery mark failed, the claim moved to another worker) — so it applies each ev.Key once
// in its own store, in the same transaction as the writes the event causes. Errors: a refusal (ErrInvalid, ErrConflict, ErrForbidden,
// an *HTTPError below 500) is permanent, ErrDealPending holds a refund for its sale, anything else is retried with backoff.
type DealHandler interface {
	HandleDeal(ctx context.Context, c Caller, ev DealEvent) error
}

// DealHandlerFunc adapts a function.
type DealHandlerFunc func(ctx context.Context, c Caller, ev DealEvent) error

func (f DealHandlerFunc) HandleDeal(ctx context.Context, c Caller, ev DealEvent) error {
	return f(ctx, c, ev)
}

// DealHandling is implemented by an App whose deal subscriber keeps its own transaction (DealHandler).
type DealHandling interface {
	HandlesDeals(d Deps) DealHandler
}

// ErrDealPending is what a handler returns (through PairRefund) when a refund or void arrived before its sale: the delivery waits,
// without using up its attempts (423 + Retry-After), and fails for good after the platform's wait.
var ErrDealPending = errors.New("the sale this reverses has not arrived yet")

// PairRefund is the out-of-order helper for subscribers. A refund (or void) can reach a subscriber before its sale (the sale's
// delivery is retrying). Call it first thing in HandleDeal with a lookup of "has this handler applied the sale ev.Reverses?":
// it returns nil for a sale, or for a refund whose sale is there; otherwise ErrDealPending, which the platform answers by delivering
// the refund again in minutes, until its wait has passed (then it is failed and listed for a person).
func PairRefund(ev DealEvent, sawSale func() (bool, error)) error {
	if ev.Kind == DealSale {
		return nil
	}
	ok, err := sawSale()
	if err != nil {
		return err
	}
	if !ok {
		return ErrDealPending
	}
	return nil
}

// DealService is the service an app that hears of deals declares (trigger event:deal, ADR-0130 §3.2 H4).
func DealService() k3.Service {
	return k3.Service{ID: "deals", Title: "成交事件", Trigger: []string{k3.TriggerEvent + k3.EventDeal}, Endpoint: "/k3/services/deals"}
}

// DealCall is the body of a deal event delivered to an app (and the input of a run of its service of event:deal).
type DealCall struct {
	Caller Caller    `json:"caller"`
	Event  DealEvent `json:"event"`
}

// dealWait is how long a refund that came before its sale waits before it is delivered again (the platform's providers.dealWait).
const dealWait = 5 * time.Minute

// dealAnswer is what a deal handler's error means to the platform's service runtime (the platform's providers.DealAnswer): done, not
// yet (the refund's sale has not arrived), refused, or retried.
func dealAnswer(err error) (status int, retryAfter time.Duration, body []byte) {
	switch {
	case err == nil:
		return http.StatusNoContent, 0, nil
	case errors.Is(err, ErrDealPending):
		return http.StatusLocked, dealWait, errBody(err)
	case refused(err):
		return http.StatusUnprocessableEntity, 0, errBody(err)
	}
	return http.StatusInternalServerError, 0, errBody(err)
}

func errBody(err error) []byte {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}
