package appkit

import (
	"context"
	"net/http"
)

// Customer tags (opv2 J2 ④, /app/v1/customers/…/tags): the tenant's tag dictionary, and putting one of its manual tags on a customer
// of the tenant or taking it off — as the caller (the page session's person, or the app itself with Caller{Tenant: …}). The app
// itself reaches every customer of the tenant; a person only those within their scope in the customer app (others are ErrNotFound). The manifest
// uses app.customers:read to list and app.customers:tag to write. A tag another source owns (WeCom) is ErrInvalid, an unknown customer
// or tag ErrNotFound; every change is in the tenant's audit chain, and an app changes at most 3000 tags per tenant in ten minutes
// (an HTTPError of status 429 past that).
type CustomerTags interface {
	Tags(ctx context.Context, c Caller) ([]CustomerTag, error)
	Tag(ctx context.Context, c Caller, customer, tag string, on bool) error
}

// CustomerTag is a tag of the tenant's dictionary. Writable: an app may put it on and take it off (a manual tag).
type CustomerTag struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Source   string `json:"source"` // manual | wecom | rule
	Writable bool   `json:"writable"`
}

// ScopeCustomersTag is the scope an app declares (uses) to write customer tags.
const ScopeCustomersTag = "app.customers:tag"

// CustomerTags is the customer centre's tags over /app/v1.
func (cl *Client) CustomerTags() CustomerTags { return customerTags{cl} }

type customerTags struct{ cl *Client }

func (t customerTags) Tags(ctx context.Context, c Caller) ([]CustomerTag, error) {
	var out struct {
		Items []CustomerTag `json:"items"`
	}
	if err := t.cl.call(ctx, c, http.MethodGet, "/app/v1/customers/tags", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (t customerTags) Tag(ctx context.Context, c Caller, customer, tag string, on bool) error {
	method := http.MethodPut
	if !on {
		method = http.MethodDelete
	}
	return t.cl.call(ctx, c, method, "/app/v1/customers/"+pathEscape(customer)+"/tags/"+pathEscape(tag), nil, nil, nil)
}
