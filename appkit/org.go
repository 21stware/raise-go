package appkit

import (
	"context"
	"net/url"
)

// OrgUnit is a department of the tenant (Path is "/a/b/c/": the ids from the root down).
type OrgUnit struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// OrgRole is one of the tenant's own roles.
type OrgRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OrgChart is the tenant's departments and roles as an app needs them for its own sharing rules: pickers that offer a department or a
// role, and access lists whose entries name one. It never decides a permission point (Authz does that).
type OrgChart interface {
	Units(ctx context.Context, tenant string) ([]OrgUnit, error)
	Roles(ctx context.Context, tenant string) ([]OrgRole, error)
	// Placement is the department chain of a person (their own department first, then each parent up to the root; then the same for
	// each department they are appointed at) and the ids of the tenant roles assigned to them. Both are empty for a person without a
	// department or a role.
	Placement(ctx context.Context, tenant, subject string) (units []string, roles []string, err error)
}

// Org is the tenant's org chart over /app/v1/org (ADR-0130 P11; Deps.Org). The calls are the app's own (no person behind them), for the
// tenant named; an answer for another tenant is ErrForbidden.
func (cl *Client) Org() OrgChart { return org{cl} }

type org struct{ cl *Client }

var _ OrgChart = org{}

// get calls one org route for the tenant and refuses an answer for another tenant.
func (o org) get(ctx context.Context, tenant, path string, q url.Values, out any) error {
	rep, err := o.cl.do(ctx, Caller{Tenant: tenant}, "GET", "/app/v1/org/"+path, q, nil)
	if err != nil {
		return err
	}
	if err := mapStatus(rep.status, rep.hdr, rep.body); err != nil {
		return err
	}
	if err := refuseOtherTenant(rep.hdr, tenant); err != nil {
		return err
	}
	return unmarshalJSON(rep.body, out)
}

func (o org) Units(ctx context.Context, tenant string) ([]OrgUnit, error) {
	var out struct {
		Items []OrgUnit `json:"items"`
	}
	if err := o.get(ctx, tenant, "units", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (o org) Roles(ctx context.Context, tenant string) ([]OrgRole, error) {
	var out struct {
		Items []OrgRole `json:"items"`
	}
	if err := o.get(ctx, tenant, "roles", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (o org) Placement(ctx context.Context, tenant, subject string) ([]string, []string, error) {
	var out struct {
		Units []string `json:"units"`
		Roles []string `json:"roles"`
	}
	if err := o.get(ctx, tenant, "placement", url.Values{"user": {subject}}, &out); err != nil {
		return nil, nil, err
	}
	return out.Units, out.Roles, nil
}
