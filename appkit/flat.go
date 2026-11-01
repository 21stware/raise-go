package appkit

import (
	"context"
	"errors"
	"time"
)

// FlatGrants is the platform's access kernel's flat lists as an app reaches them when the kernel keeps the list but the app keeps the
// rules over it (a member's sensitive records, ADR-0130 P11): the app decided who may grant, so it reads and writes the list as itself.
// Client.Flat is it over /app/v1/access/{type}/{id}…, with the app's own tenant token. Call it outside the app's own transactions: it is
// a service call.
type FlatGrants interface {
	// Grants is every grant on a resource (expired ones too: the app decides with its own clock); none when it was never registered.
	Grants(ctx context.Context, tenant, typ, id string) ([]FlatGrant, error)
	// Put adds or replaces the grant of g's principal (one per principal), registering the resource when it has no row.
	Put(ctx context.Context, tenant, typ, id string, g FlatGrant) (FlatGrant, error)
	// Revoke removes the principal's grant; false when there was none.
	Revoke(ctx context.Context, tenant, typ, id, kind, principal string) (bool, error)
	// Drop removes the resource, its list and its requests (a deleted resource).
	Drop(ctx context.Context, tenant, typ, id string) error
	// Naming is the grants of the type that name the subject (expired ones too), by resource: on ids, or on every resource of the
	// type when ids is nil. The app works out a person's level before its own transaction from these.
	Naming(ctx context.Context, tenant, typ string, sub FlatSubject, ids []string) (map[string][]FlatGrant, error)
	// Depart removes every grant naming the person on the type and withdraws their pending requests on it (decided by to): a
	// person who leaves the tenant's staff or hands their members over.
	Depart(ctx context.Context, tenant, typ, user, to string) error
}

// FlatSubject is whom FlatGrants.Naming looks for: a person, the departments they sit in (parents included: an entry for a
// department reaches below) and their roles (tenant role ids, platform roles written "platform:<role>"), as the app worked them out.
type FlatSubject struct {
	User  string   `json:"user"`
	Units []string `json:"units,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

// FlatGrant is one grant of a flat list: a person (user), a department and those below it (unit), a role (a tenant role id, or
// "platform:<role>") or everyone, with a level (view, comment, edit, manage).
type FlatGrant struct {
	Kind      string     `json:"kind"`
	Principal string     `json:"principal"`
	Level     string     `json:"level"`
	Name      string     `json:"name,omitempty"` // a display snapshot; decisions read kind and principal only
	Note      string     `json:"note,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil = until revoked
	By        string     `json:"createdBy,omitempty"` // who gave it
	At        time.Time  `json:"createdAt"`
}

// Flat is FlatGrants over the app API's access routes of a flat type whose rules the app keeps (Deps.Flat): every call goes with the
// app's own tenant token (it names no person).
func (cl *Client) Flat() FlatGrants { return flat{cl} }

type flat struct{ cl *Client }

var _ FlatGrants = flat{}

// accessPath is /app/v1/access/{type}/{id}/rest… (rest already escaped).
func accessPath(typ, id string, rest ...string) string {
	p := "/app/v1/access/" + pathEscape(typ)
	if id != "" {
		p += "/" + pathEscape(id)
	}
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

func (f flat) Grants(ctx context.Context, tenant, typ, id string) ([]FlatGrant, error) {
	var v struct {
		Grants []FlatGrant `json:"grants"`
	}
	err := f.cl.call(ctx, Caller{Tenant: tenant}, "GET", accessPath(typ, id), nil, nil, &v)
	if errors.Is(err, ErrNotFound) {
		return []FlatGrant{}, nil // never registered: no grants
	}
	if err != nil {
		return nil, err
	}
	if v.Grants == nil {
		v.Grants = []FlatGrant{}
	}
	return v.Grants, nil
}

func (f flat) Put(ctx context.Context, tenant, typ, id string, g FlatGrant) (FlatGrant, error) {
	body := map[string]any{"kind": g.Kind, "principal": g.Principal, "name": g.Name, "level": g.Level, "note": g.Note, "expiresAt": g.ExpiresAt, "by": g.By}
	var out FlatGrant
	if err := f.cl.call(ctx, Caller{Tenant: tenant}, "POST", accessPath(typ, id, "grants"), nil, body, &out); err != nil {
		return FlatGrant{}, err
	}
	return out, nil
}

func (f flat) Revoke(ctx context.Context, tenant, typ, id, kind, principal string) (bool, error) {
	if principal == "" {
		principal = "-" // everyone
	}
	err := f.cl.call(ctx, Caller{Tenant: tenant}, "DELETE", accessPath(typ, id, "grants", pathEscape(kind), pathEscape(principal)), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (f flat) Drop(ctx context.Context, tenant, typ, id string) error {
	err := f.cl.call(ctx, Caller{Tenant: tenant}, "DELETE", accessPath(typ, id), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (f flat) Naming(ctx context.Context, tenant, typ string, sub FlatSubject, ids []string) (map[string][]FlatGrant, error) {
	if ids != nil && len(ids) == 0 {
		return map[string][]FlatGrant{}, nil
	}
	body := map[string]any{"user": sub.User, "units": sub.Units, "roles": sub.Roles}
	if ids != nil {
		body["ids"] = ids
	}
	var v struct {
		Grants map[string][]FlatGrant `json:"grants"`
	}
	if err := f.cl.call(ctx, Caller{Tenant: tenant}, "POST", accessPath(typ, "naming"), nil, body, &v); err != nil {
		return nil, err
	}
	if v.Grants == nil {
		v.Grants = map[string][]FlatGrant{}
	}
	return v.Grants, nil
}

func (f flat) Depart(ctx context.Context, tenant, typ, user, to string) error {
	return f.cl.call(ctx, Caller{Tenant: tenant}, "POST", accessPath(typ, "departure"), nil, map[string]any{"user": user, "to": to}, nil)
}
