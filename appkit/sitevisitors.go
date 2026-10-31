package appkit

import (
	"context"
	"time"
)

// SiteVisitors is the site visitor identity as apps use it (ADR-0066 §1: a platform capability; docs/handover/handover-distribution-pilot.md
// #3). A visitor of a tenant's site is tied to a customer of the customer centre by the site (a form submit, the official account's
// authorisation); apps ask who a visitor is, or read the visitors identified since a feed position, instead of joining the site's
// table into their own queries. Client.Visitors answers it over the platform's GET/PUT /app/v1/site-visitors….
//
// There are two kinds of tie, and a caller must pick the one it needs (PR #143, finding 9). A VERIFIED tie is one the visitor proved:
// WeChat's authorisation, an SMS code. A LINKED tie is any tie, including the one a form submit writes from a phone number typed into
// it, which nobody proved: anyone can type anyone's number. Whatever shows or changes a customer's own things (their memberships and
// bookings, their orders, their rows of a table) goes by the verified tie; touches and attribution (which campaign or which promoter
// brought the visitor, a visit written to the CRM) may go by the linked one.
type SiteVisitors interface {
	// VerifiedCustomerOf is the customer the visitor of the site proved to be ("" when none): what access to a customer's own things
	// goes by.
	VerifiedCustomerOf(ctx context.Context, c Caller, site, visitor string) (string, error)
	// LinkedCustomerOf is the customer the visitor of the site is tied to, proved or not ("" when none): for touches and attribution,
	// never for access.
	LinkedCustomerOf(ctx context.Context, c Caller, site, visitor string) (string, error)
	// Identified lists visitors of the caller's tenant tied to a customer (or to a different one) at a feed position after `after`,
	// in that order, at most limit (1..500).
	//
	// A position is handed out when the tie is written, but becomes visible when its transaction commits, so a row can appear below
	// the newest position already read. A reader keeps its stored position behind a settle window (rows whose At is older than
	// IdentifiedSettle) and reads the rows after it again next time, which costs nothing when applying a row is idempotent.
	Identified(ctx context.Context, c Caller, after int64, limit int) ([]IdentifiedVisitor, error)
	// The feed carries linked ties; Verified says which of them were proved.
	//
	// Identify ties the visitor of the site to a customer when it is tied to nobody yet, and answers the customer it is tied to (an
	// earlier tie wins: two first visits racing each other agree on one customer). The tie it writes is linked, not verified.
	Identify(ctx context.Context, c Caller, site, visitor, customer string) (string, error)
}

// IdentifiedVisitor is one row of the feed.
type IdentifiedVisitor struct {
	Site     string    `json:"site"`
	Visitor  string    `json:"visitor"`
	Customer string    `json:"customer"`
	Seq      int64     `json:"seq"`
	At       time.Time `json:"at"`
	// Verified: the visitor proved who they are (WeChat, an SMS code); false for a tie a form or Identify wrote.
	Verified bool `json:"verified"`
}

// IdentifiedSettle is how far behind the clock a feed reader keeps its stored position (see SiteVisitors.Identified).
const IdentifiedSettle = time.Minute
