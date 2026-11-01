package appkit

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// A customer's reach (opv2 J2 ⑤, /app/v1/outreach/customers/{id}/messages and /outreach/reachable; scope app.outreach:reach): the
// messages a customer of the tenant was sent on every channel, and whether the customer can be reached on a channel for a purpose now.
// Only a customer of the tenant (ErrNotFound for any other id; a merged id reads as its primary customer); for a page session's
// person, only a customer within their scope in the customer app.
type Reach interface {
	// History lists the customer's messages, newest first (limit 1–200; since zero: all).
	History(ctx context.Context, c Caller, customer string, since time.Time, limit int) ([]SentMessage, error)
	// Reachable answers before a send: the platform checks the same again when it sends.
	Reachable(ctx context.Context, c Caller, customer, channel, purpose string) (Reachability, error)
}

// ScopeOutreachReach is the scope an app declares (uses) for Reach.
const ScopeOutreachReach = "app.outreach:reach"

// SentMessage is one message to the customer: channel, purpose, state and times; the template and trigger only when Mine (this app's).
type SentMessage struct {
	ID          string     `json:"id"`
	Channel     string     `json:"channel"`
	Purpose     string     `json:"purpose"`
	State       string     `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	Mine        bool       `json:"mine"`
	Template    string     `json:"template,omitempty"`
	TriggerRef  string     `json:"triggerRef,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	SentAt      *time.Time `json:"sentAt,omitempty"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// Reachability is the answer: Reason is the first check that says no (channel_off, withdrawn, no_consent, no_identity, blocked,
// freq_day, freq_week, tenant_cap); Later is when a marketing message would go out instead of now (quiet hours).
type Reachability struct {
	Reachable bool       `json:"reachable"`
	Reason    string     `json:"reason,omitempty"`
	Later     *time.Time `json:"later,omitempty"`
}

// Reach is a customer's reach over /app/v1.
func (cl *Client) Reach() Reach { return reach{cl} }

type reach struct{ cl *Client }

func (r reach) History(ctx context.Context, c Caller, customer string, since time.Time, limit int) ([]SentMessage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out struct {
		Items []SentMessage `json:"items"`
	}
	if err := r.cl.call(ctx, c, http.MethodGet, "/app/v1/outreach/customers/"+pathEscape(customer)+"/messages", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (r reach) Reachable(ctx context.Context, c Caller, customer, channel, purpose string) (Reachability, error) {
	var out Reachability
	err := r.cl.call(ctx, c, http.MethodPost, "/app/v1/outreach/reachable", nil, map[string]string{"customer": customer, "channel": channel, "purpose": purpose}, &out)
	return out, err
}
