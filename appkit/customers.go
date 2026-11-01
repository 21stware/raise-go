package appkit

import (
	"context"
	"encoding/json"
	"time"
)

// The customer centre (ADR-0046, docs/customer.md) as an app that runs on its own reaches it: the platform's
// /app/v1/customers (Client.Customers). The personal data (phone, WeChat ids) stays with the platform, encrypted; the app keeps the
// customer id and asks again when it needs more.

// The identity kinds (open: a new channel needs a new kind and a CHECK, not a new table) and the stages.
const (
	IdentPhone       = "phone"
	IdentEmail       = "email"
	IdentOpenID      = "openid"
	IdentUnionID     = "unionid"
	IdentWeComExtern = "wecom_external"

	CustomerLead     = "lead"
	CustomerCustomer = "customer"
	CustomerDormant  = "dormant"
)

// CustomerIdentity is one way to recognise a person. Value is the plain value (the service normalises phones to E.164 and
// lower-cases e-mail); Scope is the official account's appid of an openid and the corp id of a wecom_external. Verified says the
// caller has proof the person holds it (an SMS code, a WeChat phone authorisation, the OAuth openid of the visitor); a number a
// member of staff typed is not verified.
type CustomerIdentity struct {
	Kind     string `json:"kind"`
	Scope    string `json:"scope,omitempty"`
	Value    string `json:"value"`
	Verified bool   `json:"verified,omitempty"`
}

// CustomerResolveReq is what Resolve is given. Source is where the person first came from (the app is the calling app's id and
// cannot be set); Name, Owner and OwnerUnit are used when a customer is created (a staff-path call also fills an empty name
// and an empty owner of the customer it found).
type CustomerResolveReq struct {
	Identities []CustomerIdentity `json:"identities"`
	Name       string             `json:"name,omitempty"`
	Channel    string             `json:"channel,omitempty"`   // first source: channel
	SourceRef  string             `json:"sourceRef,omitempty"` // first source: k3:// reference to the campaign
	Owner      string             `json:"owner,omitempty"`
	OwnerUnit  string             `json:"ownerUnit,omitempty"`
	// Public marks an anonymous visitor's call (see Resolve).
	Public bool `json:"public,omitempty"`
	// Anonymous asks for a new customer without any identity (ADR-0072 §3: a member registered without a phone number still has a
	// customer). Identities must then be empty; nothing is matched, so the call always creates. A later Resolve that brings an
	// identity gets a customer of its own: the caller joins the two with Merge.
	Anonymous bool `json:"anonymous,omitempty"`
}

// CustomerRef is the answer of Resolve: the primary customer's id and whether the call created it.
type CustomerRef struct {
	ID      string `json:"id"`
	Created bool   `json:"created"`
	// Name is the customer's name as the centre keeps it (the master record of a person's name, ADR-0072 §3): the one the call
	// gave when it created the customer or when the customer had none, else the one it had.
	Name string `json:"name,omitempty"`
}

// CustomerTouchReq is one timeline entry. Kind is "{app}.{verb}" ("coupons.redeem"); Ref a k3:// reference to what happened;
// Key the idempotency key (unique per tenant: derive it from the source event, e.g. "coupons:claim:cl_17"); At defaults to now.
type CustomerTouchReq struct {
	Customer string         `json:"customer"`
	Key      string         `json:"key"`
	Kind     string         `json:"kind"`
	Ref      string         `json:"ref,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	At       time.Time      `json:"at,omitempty"`
}

// CustomerSummary is one row of the list. Stage is the effective one (a customer without touches for long is dormant); Phone is
// the masked first phone.
type CustomerSummary struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Stage         string    `json:"stage"`
	Owner         string    `json:"owner,omitempty"`
	OwnerUnit     string    `json:"ownerUnit,omitempty"`
	SourceApp     string    `json:"sourceApp,omitempty"`
	SourceChannel string    `json:"sourceChannel,omitempty"`
	Phone         string    `json:"phone,omitempty"`
	Tags          []string  `json:"tags,omitempty"` // tag ids
	LastTouchAt   time.Time `json:"lastTouchAt"`
	CreatedAt     time.Time `json:"createdAt"`
	// WeComBasis says how the customer's active 企业微信 outside contacts were tied to them, the weakest one: "none" (no unionid and
	// no phone: possibly the same person as another customer, shown as 待关联), "phone" (matched by a phone an employee noted), or
	// "unionid"; "" when the customer has no outside contact. WeComPending is WeComBasis == "none".
	WeComBasis   string `json:"wecomBasis,omitempty"`
	WeComPending bool   `json:"wecomPending,omitempty"`
}

// CustomerIdentityView is an identity as shown: Value only when the profile was asked with reveal.
type CustomerIdentityView struct {
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope,omitempty"`
	Masked    string    `json:"masked"`
	Value     string    `json:"value,omitempty"`
	Verified  bool      `json:"verified"`
	FirstSeen time.Time `json:"firstSeen"`
	Source    string    `json:"source,omitempty"`
}

// CustomerView is the profile. ID is the primary customer's (it differs from the id asked for when that one was merged);
// MergedFrom lists the customers merged into it.
type CustomerView struct {
	CustomerSummary
	SourceRef  string                 `json:"sourceRef,omitempty"`
	DealtAt    *time.Time             `json:"dealtAt,omitempty"`
	Identities []CustomerIdentityView `json:"identities"`
	Links      []string               `json:"links"`
	MergedFrom []string               `json:"mergedFrom,omitempty"`
	// TouchCount is the number of touches of the customer and of everyone merged into it.
	TouchCount int `json:"touchCount"`
}

// CustomerClient is the customer centre as an app uses it without sharing a transaction with the platform (ADR-0066 §2): each call
// is a transaction of its own in process, a request of its own when the app runs elsewhere. A customer Resolve made while the app's
// own transaction later rolls back stays (resolving is idempotent by identity, so the retry finds the same customer).
//
// Reads have the one scope the remote path will have, decided by the platform and not by the app: a customer's masked profile by id
// (ids cannot be enumerated). No list, no lookup by identity, no decryption. Grow it only with the remote route next to it.
type CustomerClient interface {
	Resolve(ctx context.Context, c Caller, req CustomerResolveReq) (CustomerRef, error)
	// Preview is what Resolve would answer, writing nothing: the call an op's dry-run makes (a preview must not create a customer
	// in another service). A customer it would create has an id no other call knows.
	Preview(ctx context.Context, c Caller, req CustomerResolveReq) (CustomerRef, error)
	// Get answers ErrNotFound for an id the centre does not have.
	Get(ctx context.Context, c Caller, id string) (*CustomerView, error)
	// Touch appends one entry to the customer's timeline, once per key (false: the key was there already).
	Touch(ctx context.Context, c Caller, req CustomerTouchReq) (bool, error)
}

// CustomerClient is the customer centre of the app's Deps (Client.Customers; nil when it is not wired).
func (d Deps) CustomerClient() CustomerClient { return d.CustomerAPI }

// CustomerQuery filters CustomerCentre.List. Q matches the name (contains), or the masked phone / e-mail of an identity, or a whole
// phone number; Stage is lead, customer or dormant (the effective stage); Tag a tag id; Owner a staff subject; Source the app that
// first brought the customer.
type CustomerQuery struct {
	Q      string `json:"q,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Source string `json:"source,omitempty"`
	// Hint keeps only the customers that are one side of an open merge suggestion.
	Hint bool `json:"hint,omitempty"`
	// WeComPending keeps only the customers with a 企业微信 outside contact that could not be tied to a person (待关联).
	WeComPending bool `json:"wecomPending,omitempty"`
	Limit        int  `json:"limit,omitempty"`
	Offset       int  `json:"offset,omitempty"`
}

// CustomerPage is a page of the list with the total over the same scope and filters.
type CustomerPage struct {
	Items []CustomerSummary `json:"items"`
	Total int               `json:"total"`
}

// CustomerTouch is one timeline entry as read. Customer is the customer it was written for (a merged one, perhaps).
type CustomerTouch struct {
	ID       int64           `json:"id"`
	Customer string          `json:"customer"`
	Kind     string          `json:"kind"`
	App      string          `json:"app,omitempty"`
	Ref      string          `json:"ref,omitempty"`
	Attrs    json.RawMessage `json:"attrs"`
	At       time.Time       `json:"at"`
}

// CustomerCentre is the whole customer centre an app reaches over /app/v1/customers (ADR-0130 P11): CustomerClient with the lookup,
// the list, the timeline and the writes (the platform's firstparty.CustomerClient). CustomerClient stays the narrow contract other
// implementations keep.
type CustomerCentre interface {
	CustomerClient
	// Find looks a customer up by one identity (kind "phone"; any verification state): the masked profile, ErrNotFound when none.
	Find(ctx context.Context, c Caller, kind, value string) (*CustomerView, error)
	// List pages the customers inside the calling person's customer.read grant (the app's own call: the whole tenant; a person
	// without the grant: ErrForbidden). Merged and erased customers are not listed.
	List(ctx context.Context, c Caller, q CustomerQuery) (CustomerPage, error)
	// Timeline is the touches of the customer and of everyone merged into it, newest first; before is the id of the last touch of
	// the previous page (0: from the newest).
	Timeline(ctx context.Context, c Caller, id string, limit int, before int64) ([]CustomerTouch, error)

	// Rename, Link, SetOwner, MarkDealt, Merge and Erase each take effect on their own (a merged id follows to its primary). A link's
	// reference must be one of the calling app's own records (k3://<app>/…).
	Rename(ctx context.Context, c Caller, id, name string) error
	Link(ctx context.Context, c Caller, customer, ref string) error
	SetOwner(ctx context.Context, c Caller, id, owner, ownerUnit string) error
	MarkDealt(ctx context.Context, c Caller, id string, at time.Time) error
	// Merge joins two customers and returns the primary id (the older is kept).
	Merge(ctx context.Context, c Caller, a, b string) (string, error)
	// Erase is final: the identity values are destroyed and every read of the customer answers ErrNotFound afterwards.
	Erase(ctx context.Context, c Caller, id string) error
}

// CustomerCentre is d.CustomerAPI when it is the whole centre (Client.Customers is one); nil otherwise.
func (d Deps) CustomerCentre() CustomerCentre {
	cc, _ := d.CustomerAPI.(CustomerCentre)
	return cc
}
