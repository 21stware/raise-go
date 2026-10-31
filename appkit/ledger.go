package appkit

import "strings"

// The books (经营, docs/apps/biz.md §5). An app whose business moves money tells the books with one posting, keyed by source app +
// source reference + kind: the books record each combination once. The app does not call the books: it queues the posting in its
// business transaction (Deps.EnqueueLedger, outbox.go) and the outbox hands it to the platform (POST /app/v1/ledger/posts) — at once
// after the commit (Deps.DeliverNow), else later by the worker. The platform keeps it until the books take it.

// LedgerParty names who the money is with. Ref points at the record elsewhere (k3://membership/member/m_1): the books link to
// it, they do not copy it; the name is what the books show.
type LedgerParty struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	Kind string `json:"kind,omitempty"` // customer | supplier | both (default: in → customer, out → supplier)
}

// LedgerBill asks for a bill (a receivable for money in, a payable for money out) instead of an entry.
type LedgerBill struct {
	Title string `json:"title,omitempty"`
	DueOn string `json:"dueOn,omitempty"` // YYYY-MM-DD, "" = no due day
}

// PostReq is one posting.
type PostReq struct {
	// SourceApp, SourceRef and Kind are the idempotency key: the books record each combination once (a second Post with the
	// same key returns the first result with Duplicate set).
	SourceApp string `json:"sourceApp"`
	SourceRef string `json:"sourceRef"`         // k3://membership/package/pk_1
	Kind      string `json:"kind"`              // purchase | refund | approval | ...
	Direction string `json:"direction"`         // in | out
	Amount    int64  `json:"amountCents"`       // > 0, cents
	Currency  string `json:"currency"`          // "" = the tenant's; another currency is refused (no conversion)
	On        string `json:"occurredOn"`        // YYYY-MM-DD, "" = today in the tenant's zone
	Account   string `json:"account,omitempty"` // an account id of the books, if the caller knows one
	// Method is how it was paid in the caller's words (cash wechat alipay card transfer other): the books map it to an account.
	Method   string       `json:"method,omitempty"`
	Category string       `json:"category,omitempty"` // a category name (found or created under the direction)
	Party    *LedgerParty `json:"party,omitempty"`
	Memo     string       `json:"memo,omitempty"`
	Bill     *LedgerBill  `json:"bill,omitempty"`
	// Reverses names the kind of an earlier posting under the same source app and reference that this one reverses (a voided
	// sale: Kind "void", Reverses "purchase"). The books write the reversal of the record that posting made (append-only, a
	// record is reversed once); Amount must be that record's amount and Direction the opposite of it.
	Reverses string `json:"reverses,omitempty"`
	// Settles names the kind of an earlier posting under the same source app and reference that opened a bill (an approved
	// expense report: Kind "claim" with a Bill) which this payment settles (Kind "claim.pay"). The books record the money and
	// allocate it to that bill, at most what the bill still owes; the rest (a bill voided or settled meanwhile) stays an
	// unallocated prepayment. The bill not being in the books yet is a 503 (retried).
	Settles string `json:"settles,omitempty"`
	// CreatedBy / OwnerUnit are whom the record belongs to for data scopes ("" = the caller and the caller's department).
	CreatedBy string `json:"createdBy,omitempty"`
	OwnerUnit string `json:"ownerUnit,omitempty"`
}

// Validate checks what every ledger needs.
func (r PostReq) Validate() error {
	switch {
	case strings.TrimSpace(r.SourceApp) == "" || strings.TrimSpace(r.SourceRef) == "" || strings.TrimSpace(r.Kind) == "":
		return Invalid("a posting needs its source app, source reference and kind")
	case r.Direction != "in" && r.Direction != "out":
		return Invalid("direction must be in or out")
	case r.Amount <= 0:
		return Invalid("the amount must be more than zero")
	case r.Reverses != "" && (r.Reverses == r.Kind || r.Bill != nil):
		return Invalid("a reversal names another kind of the same source and is not a bill")
	case r.Settles != "" && (r.Settles == r.Kind || r.Bill != nil || r.Reverses != ""):
		return Invalid("a payment that settles a bill names another kind of the same source and is neither a bill nor a reversal")
	}
	return nil
}
