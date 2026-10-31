package appkit

import (
	"errors"
	"time"
)

// The entitlement service's types as an app reaches them over /app/v1/entitlements (ADR-0047, ADR-0130 §3.2 H4): the platform's
// firstparty types with the same names and JSON, so an app changes only its import path. Something a customer holds that can be
// redeemed, expires and can be refunded — a count card, a period card, a voucher, a prize. The service owns the tables and the
// invariants; an app uses it through EntitlementClient and never writes its tables.

// Entitlement kinds.
const (
	EntCount       = "count"        // 次卡
	EntPeriod      = "period"       // 期卡: unlimited within the validity
	EntCountPeriod = "count_period" // 次卡 + 有效期
	EntVoucher     = "voucher"      // 券: one use
	EntPrize       = "prize"        // 奖品: redeemed once ("已兑奖")
)

// Voucher forms.
const (
	VoucherCash    = "cash"    // 满减
	VoucherPercent = "percent" // 折扣
	VoucherService = "service" // 兑换服务
	VoucherCode    = "code"    // 不透明券码
)

// EntParams are the kind-specific parameters of a template (and the copy on its grants); which fields a kind reads is checked by
// the service when a template is saved.
type EntParams struct {
	TotalTimes int    `json:"total_times,omitempty"` // count, count_period
	ValidDays  int    `json:"valid_days,omitempty"`  // validity length in days (period and count_period: required)
	ValidFrom  string `json:"valid_from,omitempty"`  // "grant" (default) | "first_use"
	DailyCap   int    `json:"daily_cap,omitempty"`   // period kinds
	WeeklyCap  int    `json:"weekly_cap,omitempty"`
	// Fixed validity (vouchers; overrides ValidDays), "YYYY-MM-DD" in the store's time zone, last day inclusive.
	StartsOn string `json:"starts_on,omitempty"`
	EndsOn   string `json:"ends_on,omitempty"`

	Form            string `json:"form,omitempty"`              // voucher: cash | percent | service | code
	AmountOffCents  int64  `json:"amount_off_cents,omitempty"`  // cash
	Percent         int    `json:"percent,omitempty"`           // percent: how many percent off (1–99)
	ThresholdCents  int64  `json:"threshold_cents,omitempty"`   // cash / percent: the order must reach this
	MaxOffCents     int64  `json:"max_off_cents,omitempty"`     // percent: the most it takes off (0 = no cap)
	RestoreOnRefund bool   `json:"restore_on_refund,omitempty"` // the order's refund gives the voucher back (the caller reads this and calls Void)

	Description string `json:"description,omitempty"` // prize
	Fulfilment  string `json:"fulfilment,omitempty"`  // prize: how it is handed over (opaque here, ADR-0050)
}

// TemplateReq creates or changes a template. Over the app API the platform names the owner (the calling app): OwnerApp may be left
// empty, and another app's name is refused.
type TemplateReq struct {
	OwnerApp      string    `json:"ownerApp"` // who creates it ("campaigns"); fixed once created
	Kind          string    `json:"kind"`     // fixed once created
	Name          string    `json:"name"`
	Params        EntParams `json:"params"`
	Scope         []string  `json:"scope"`
	Transferable  bool      `json:"transferable"`
	Freezable     bool      `json:"freezable"`
	MaxFreezeDays int       `json:"maxFreezeDays"`
	Active        *bool     `json:"active"` // nil: active on create, unchanged on update
}

// EntTemplate is a stored template.
type EntTemplate struct {
	ID            string    `json:"id"`
	OwnerApp      string    `json:"ownerApp"`
	Kind          string    `json:"kind"`
	Name          string    `json:"name"`
	Params        EntParams `json:"params"`
	Scope         []string  `json:"scope"`
	Transferable  bool      `json:"transferable"`
	Freezable     bool      `json:"freezable"`
	MaxFreezeDays int       `json:"maxFreezeDays"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Grant sources.
const (
	EntSourcePurchase = "purchase"
	EntSourceGift     = "gift"
	EntSourceClaim    = "claim"
	EntSourceDraw     = "draw"
	EntSourceImport   = "import"
	EntSourceTransfer = "transfer"
)

// GrantReq issues grants from a template.
type GrantReq struct {
	// Key is the idempotency key ("{source app}:{source ref}:{kind}"); required.
	Key         string         `json:"key,omitempty"`
	Template    string         `json:"template,omitempty"`
	Holder      string         `json:"holder,omitempty"`    // the customer (cu_…)
	Source      string         `json:"source,omitempty"`    // EntSource…; default purchase (gift when PaidCents is 0 and none is given)
	Origin      string         `json:"origin,omitempty"`    // k3:// reference: who issued it and why
	PaidCents   int64          `json:"paidCents,omitempty"` // what the holder paid for each grant
	Attribution map[string]any `json:"attribution,omitempty"`
	Count       int            `json:"count,omitempty"` // how many grants (1–1000, default 1); each is its own voucher with its own code
	// Code is the voucher code to give the grant instead of a generated one (a campaign's code pool: the code was printed or
	// imported before anyone held it). Only with Count 1 and only for a voucher or prize; unique within the tenant.
	Code string `json:"code,omitempty"`
}

// GrantResult is the grants of a Grant call. Replay: the same Key was already done; these are its grants.
type GrantResult struct {
	Grants []EntGrant `json:"grants"`
	Replay bool       `json:"replay"`
}

// RedeemReq uses a grant.
type RedeemReq struct {
	// Key is the idempotency key; repeating a request with it returns the first result. Required.
	Key    string `json:"key,omitempty"`
	Holder string `json:"holder,omitempty"` // whose grants to pick from; may be empty when Pick names a grant
	Pick   string `json:"pick,omitempty"`   // the grant to use; empty: the service picks
	// Scope says what the use is for (opaque tags). A grant applies when its own scope is empty, this is empty, or every tag
	// here is in the grant's scope.
	Scope []string `json:"scope,omitempty"`
	Times int      `json:"times,omitempty"` // default 1 (a voucher or prize is always used once)
	By    string   `json:"by,omitempty"`    // the staff member or system that redeems (default: the caller)
	Where string   `json:"where,omitempty"` // store / venue of an offline redemption
	Ref   string   `json:"ref,omitempty"`   // the order or booking it pays for
	// OrderCents is the amount a cash or percent voucher is applied to (required for those: the threshold and the discount
	// depend on it).
	OrderCents int64 `json:"orderCents,omitempty"`
}

// EntRedemption is one use of a grant (核销单).
type EntRedemption struct {
	ID            string     `json:"id"`
	No            string     `json:"no"`
	Grant         string     `json:"grant"`
	Holder        string     `json:"holder"`
	Times         int        `json:"times"`
	DiscountCents int64      `json:"discountCents"`
	State         string     `json:"state"` // ok | void
	VoidReason    string     `json:"voidReason,omitempty"`
	VoidedAt      *time.Time `json:"voidedAt,omitempty"`
	By            string     `json:"by"`
	ByName        string     `json:"byName"`
	Where         string     `json:"where,omitempty"`
	Ref           string     `json:"ref,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// RedeemResult is a redemption and the grant after it.
type RedeemResult struct {
	Redemption EntRedemption `json:"redemption"`
	Grant      EntGrant      `json:"grant"`
	Replay     bool          `json:"replay"` // the same Key was already done: nothing was deducted again
}

// EntRefundReq refunds a grant.
type EntRefundReq struct {
	Grant       string `json:"grant,omitempty"`
	Mode        string `json:"mode,omitempty"` // "" / "times": by the share left; "amount": AmountCents
	AmountCents *int64 `json:"amountCents,omitempty"`
	Reason      string `json:"reason,omitempty"` // required
}

// EntRefundResult is what a refund gave back and the grant after it. The money itself is the caller's (the service does not
// touch money, ADR-0047 §5).
type EntRefundResult struct {
	AmountCents int64    `json:"amountCents"`
	Grant       EntGrant `json:"grant"`
}

// TransferReq gives a grant to another holder.
type TransferReq struct {
	Grant  string `json:"grant,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// EntHold is a live hold: an unpaid order holds a voucher or prize, nobody else can redeem it meanwhile.
type EntHold struct {
	ID        string    `json:"id"`
	Grant     string    `json:"grant"`
	Ref       string    `json:"ref"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ReserveReq keeps a use aside: what a RedeemReq asks for (Key is the reservation's own idempotency key), for TTL (default 15
// minutes, 5 seconds to 24 hours). App is the app that asks (over the app API the platform names it: the token's).
type ReserveReq struct {
	RedeemReq
	TTL time.Duration
	App string
}

// Reservation states. A live reservation past ExpiresAt reads as expired.
const (
	ReservationLive      = "live"
	ReservationConfirmed = "confirmed"
	ReservationCancelled = "cancelled"
	ReservationExpired   = "expired"
)

// EntReservation is a use kept aside.
type EntReservation struct {
	ID            string    `json:"id"`
	Grant         string    `json:"grant"`
	Holder        string    `json:"holder"`
	Times         int       `json:"times"`
	Scope         []string  `json:"scope"`
	OrderCents    int64     `json:"orderCents"`
	DiscountCents int64     `json:"discountCents"` // what the voucher takes off OrderCents
	Key           string    `json:"key"`
	Ref           string    `json:"ref,omitempty"`
	Where         string    `json:"where,omitempty"`
	By            string    `json:"by"`
	ByName        string    `json:"byName"`
	App           string    `json:"app,omitempty"`
	State         string    `json:"state"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Redemption    string    `json:"redemption,omitempty"` // when confirmed
	Reason        string    `json:"reason,omitempty"`     // when cancelled
	CreatedAt     time.Time `json:"createdAt"`
	Replay        bool      `json:"replay"`
}

// EntLedgerEntry is one change of a grant.
type EntLedgerEntry struct {
	ID           int64     `json:"id"`
	Kind         string    `json:"kind"`
	Delta        int       `json:"delta"`
	Balance      int       `json:"balance"`
	ActorName    string    `json:"actorName"`
	Reason       string    `json:"reason"`
	Ref          string    `json:"ref,omitempty"`
	RedemptionID string    `json:"redemptionId,omitempty"`
	At           time.Time `json:"at"`
}

// EntGrant is a grant as an application sees it. Status is what a person is told (active not_started frozen expired used_up
// used refunded voided transferred) as of today; State is what was done to it.
type EntGrant struct {
	ID              string         `json:"id"`
	Template        string         `json:"template"`
	OwnerApp        string         `json:"ownerApp"`
	Kind            string         `json:"kind"`
	Name            string         `json:"name"`
	Params          EntParams      `json:"params"`
	Scope           []string       `json:"scope"`
	Transferable    bool           `json:"transferable"`
	Freezable       bool           `json:"freezable"`
	MaxFreezeDays   int            `json:"maxFreezeDays"`
	Holder          string         `json:"holder"`
	Total           int            `json:"total"`
	Remaining       int            `json:"remaining"`
	StartsOn        string         `json:"startsOn"`
	EndsOn          string         `json:"endsOn"`
	State           string         `json:"state"`
	Status          string         `json:"status"`
	FrozenFrom      string         `json:"frozenFrom,omitempty"`
	FrozenUntil     string         `json:"frozenUntil,omitempty"`
	FrozenDaysUsed  int            `json:"frozenDaysUsed"`
	Source          string         `json:"source"`
	PaidCents       int64          `json:"paidCents"`
	RefundedCents   int64          `json:"refundedCents"`
	Origin          string         `json:"origin,omitempty"`
	Attribution     map[string]any `json:"attribution,omitempty"`
	Code            string         `json:"code,omitempty"`
	TransferredFrom string         `json:"transferredFrom,omitempty"`
	// Held is the live hold, if an unpaid order holds this grant now.
	Held      *EntHold         `json:"held,omitempty"`
	CreatedBy string           `json:"createdBy"`
	CreatedAt time.Time        `json:"createdAt"`
	Ledger    []EntLedgerEntry `json:"ledger,omitempty"` // Get only
}

// GrantItem is one thing an application is asked to issue through a Granter (EntitlementClient.GrantVia): the order or campaign
// fulfils through the registered Granter instead of writing the other application's tables.
type GrantItem struct {
	// Key is the idempotency key: a fulfilment is at-least-once (it goes through the outbox), so the Granter must return the same
	// result for the same Key and write nothing the second time.
	Key         string         `json:"key,omitempty"`
	Holder      string         `json:"holder,omitempty"`
	Source      string         `json:"source,omitempty"`
	Origin      string         `json:"origin,omitempty"`
	PaidCents   int64          `json:"paidCents,omitempty"`
	Count       int            `json:"count,omitempty"`
	Attribution map[string]any `json:"attribution,omitempty"`
	// Params are what the Granter needs to know what to issue (the template or product id): its own vocabulary.
	Params map[string]any `json:"params,omitempty"`
	// Dry asks for a preview (an op's dry-run): the Granter answers as it would and keeps nothing.
	Dry bool `json:"dry,omitempty"`
}

// ErrNoGranter is returned for a name nobody registered.
var ErrNoGranter = errors.New("no such granter")

// EntRefused is a refusal in words a person can read ("「十次卡」剩余 0 次，不能核销"). It is an ErrInvalid (422) with the words.
type EntRefused struct {
	Message string
	// Items explains each grant that could not be used (the holder's other grants), when there were several.
	Items []EntRefusal
}

// EntRefusal says why one grant cannot be used.
type EntRefusal struct {
	Grant  string `json:"grant"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func (e *EntRefused) Error() string { return e.Message }
func (e *EntRefused) Unwrap() error { return ErrInvalid }
