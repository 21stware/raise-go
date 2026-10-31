package appkit

import (
	"context"
	"net/url"
)

// OrgStore is a store of the tenant (ADR-0057 §4): a department of the store kind and its master data. An app refers to a store by
// its department id (ID), never by a copy of its name.
type OrgStore struct {
	ID       string `json:"id"`
	Code     string `json:"code,omitempty"`
	Name     string `json:"name"`
	ParentID string `json:"parentId,omitempty"`
	Path     string `json:"path"`
	// Region is the chain of departments above the store, the top first: 「山东省 / 鲁东区 / 济南」.
	Region   string   `json:"region,omitempty"`
	Province string   `json:"province,omitempty"`
	City     string   `json:"city,omitempty"`
	District string   `json:"district,omitempty"`
	Address  string   `json:"address,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lng      *float64 `json:"lng,omitempty"`
	Phone    string   `json:"phone,omitempty"`
	Hours    string   `json:"hours,omitempty"`
	// Status is planning, open, paused or closed; "" for a department marked as a store whose data is not filled in yet.
	Status   string `json:"status"`
	OpenedOn string `json:"openedOn,omitempty"`
}

// Selectable says whether a picker offers the store (a closed store's records stay, but nothing new is put there).
func (s OrgStore) Selectable() bool { return s.Status != "closed" }

// OrgStores is the tenant's stores as apps read them (where an entitlement is redeemed, where a person works). The platform keeps
// them; an app never writes them.
type OrgStores interface {
	// Stores are every store of the tenant, closed ones included, in the tree's order.
	Stores(ctx context.Context, tenant string) ([]OrgStore, error)
	// StoresOf are the stores a person works at or manages: those at or below each department they belong to or are appointed at
	// (a clerk: their store; a region's manager appointed at the region: every store under it).
	StoresOf(ctx context.Context, tenant, subject string) ([]OrgStore, error)
}

// Stores is the organisation's stores over /app/v1/stores (ADR-0130 P6): Deps.Stores = cl.Stores(). Each call is the app's own call
// in that tenant.
func (cl *Client) Stores() OrgStores { return stores{cl} }

type stores struct{ cl *Client }

var _ OrgStores = stores{}

func (s stores) Stores(ctx context.Context, tenant string) ([]OrgStore, error) {
	return s.list(ctx, tenant, nil)
}

func (s stores) StoresOf(ctx context.Context, tenant, subject string) ([]OrgStore, error) {
	if subject == "" {
		// the route reads an empty ?of= as "every store"; the meaning here is the stores of nobody
		return []OrgStore{}, nil
	}
	return s.list(ctx, tenant, url.Values{"of": {subject}})
}

func (s stores) list(ctx context.Context, tenant string, q url.Values) ([]OrgStore, error) {
	var out struct {
		Stores []OrgStore `json:"stores"`
	}
	err := s.cl.tenantCall(ctx, tenant, "GET", "/app/v1/stores", q, nil, &out)
	return out.Stores, err
}
