package appkit

import (
	"context"
	"slices"
	"strings"
)

// Directory resolves people of a tenant (names for pickers and timelines).
type Directory interface {
	// Members returns the active members of the tenant.
	Members(ctx context.Context, tenant string) ([]Member, error)
}

// Member is one person of a tenant as the platform's directory lists them (GET /app/v1/members).
type Member struct {
	UserID  string   `json:"userId"`
	Email   string   `json:"email"`
	Name    string   `json:"name"`
	State   string   `json:"state"` // active | inactive | invited
	Roles   []string `json:"roles"`
	Machine bool     `json:"machine,omitempty"`
	// AvatarURL is the absolute address of the person's picture ("" without one).
	AvatarURL string `json:"avatarUrl,omitempty"`
}

// Access evaluates the tenant's own permission model (the platform's internal/permissions, over GET /app/v1/access).
type Access interface {
	Access(ctx context.Context, tenant, subject string, platformRoles []string, permission, typeID string) (AccessResult, error)
	UnitOf(ctx context.Context, tenant, subject string) (string, error)
}

// AccessPoint is one question of AccessMany: a permission point and, for a read of an object type, the type id.
type AccessPoint struct {
	Point string `json:"point"`
	Type  string `json:"type,omitempty"`
}

// AccessBatcher is an Access that answers several points in one call (POST /app/v1/access/points; the Client's Access is one).
// It is apart from Access so that an Access written before it (a test double) stays one.
type AccessBatcher interface {
	AccessMany(ctx context.Context, tenant, subject string, platformRoles []string, points []AccessPoint) ([]AccessResult, error)
}

// AccessMany asks a for every point, results in the order of points: in batches when a is an AccessBatcher, one call per point
// otherwise.
func AccessMany(ctx context.Context, a Access, tenant, subject string, platformRoles []string, points []AccessPoint) ([]AccessResult, error) {
	if b, ok := a.(AccessBatcher); ok {
		return b.AccessMany(ctx, tenant, subject, platformRoles, points)
	}
	out := make([]AccessResult, len(points))
	for i, p := range points {
		r, err := a.Access(ctx, tenant, subject, platformRoles, p.Point, p.Type)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// AccessResult is the tenant's answer for one permission point (the platform's permissions.Access; its JSON is that type's: the field
// names as they are).
type AccessResult struct {
	// Managed is false in basic mode: the platform roles decide and nothing here applies.
	Managed bool
	Allowed bool
	All     bool     // every record
	Self    bool     // records the caller created
	Units   []string // records created in these departments (subtrees already resolved)
	Subject string
	// Personal: a personal grant of the caller gave (part of) it (ADR-0121). In basic mode (Managed false) Allowed and the scope
	// are then the personal grant's alone: the platform roles are the app's to add.
	Personal bool
	// Fields: for a read of an object type, the fields the caller sees masked or not at all.
	Fields map[string]string
}

// Grant is what a caller may do with one permission point of an app.
type Grant struct {
	Allowed bool
	All     bool     // every record
	Self    bool     // records the caller created
	Units   []string // records created in these departments (subtrees resolved)
	Subject string
}

// Sees reports whether a record (by who created it and the creator's department) is inside the grant's data scope.
func (g Grant) Sees(createdBy, ownerUnit string) bool {
	if !g.Allowed {
		return false
	}
	if g.All {
		return true
	}
	if g.Self && createdBy != "" && createdBy == g.Subject {
		return true
	}
	return ownerUnit != "" && slices.Contains(g.Units, ownerUnit)
}

// Authz decides what a caller may do in one app. The tenant either uses the platform roles alone ("basic" mode: Defaults
// says which platform roles hold each permission) or manages its own roles (the permission points the app declares in its
// manifest are granted to the tenant's roles, with data scopes). tenant.admin is never locked out; tenant.auditor can always
// read (a point whose id ends in ".read").
type Authz struct {
	App   string
	Perms Access
	// Defaults maps a permission id (without the "app.<id>." prefix) to the platform roles that hold it when the tenant has not
	// switched to managing its own roles. A role holds the point over every record; a role written with the suffix ":self"
	// ("tenant.member:self") holds it over the caller's own records only (Grant.Self: what the app counts as "mine", usually
	// created_by). A person with both kinds of role gets the wider grant.
	Defaults map[string][]string
}

// Point is the permission id the tenant's role editor shows and grants.
func (a Authz) Point(perm string) string { return "app." + a.App + "." + perm }

// Check evaluates one permission for a caller.
func (a Authz) Check(ctx context.Context, c Caller, perm string) (Grant, error) {
	if g, done := a.override(c, perm); done {
		return g, nil
	}
	return a.rules(ctx, c, perm, Grant{Subject: c.User})
}

// CheckMany is Check for several permissions with one question to the platform (Access.AccessMany: POST /app/v1/access/points when
// Perms answers a batch, one call per point otherwise): the grants are in the order of perms. A page that shows what the caller may
// do (a "me" endpoint) asks this once instead of once per point; nothing is remembered between calls.
func (a Authz) CheckMany(ctx context.Context, c Caller, perms []string) ([]Grant, error) {
	out := make([]Grant, len(perms))
	var ask []int
	for i, p := range perms {
		if g, done := a.override(c, p); done {
			out[i] = g
			continue
		}
		ask = append(ask, i)
	}
	var accs []AccessResult
	if a.Perms != nil && len(ask) > 0 {
		points := make([]AccessPoint, len(ask))
		for j, i := range ask {
			points[j] = AccessPoint{Point: a.Point(perms[i])}
		}
		var err error
		if accs, err = AccessMany(ctx, a.Perms, c.Tenant, c.User, c.Roles, points); err != nil {
			return nil, err
		}
	}
	for j, i := range ask {
		var acc *AccessResult
		if accs != nil {
			acc = &accs[j]
		}
		out[i] = a.apply(c, perms[i], acc, Grant{Subject: c.User})
	}
	return out, nil
}

// override is the platform's say before the app's rules: an agent only reads, tenant.admin may do everything, tenant.auditor reads
// everything. done is false when the rules decide.
func (a Authz) override(c Caller, perm string) (g Grant, done bool) {
	g = Grant{Subject: c.User}
	readOnly := perm == "read" || perm == "view" || strings.HasSuffix(perm, ".read") || strings.HasSuffix(perm, ".view")
	if c.Agent != "" && !readOnly {
		return g, true // an agent only reads, whoever it acts for (an administrator's agent included)
	}
	if c.Has(RoleTenantAdmin) {
		g.Allowed, g.All = true, true
		return g, true
	}
	if c.Has(RoleTenantAuditor) && readOnly {
		g.Allowed, g.All = true, true
		return g, true
	}
	return g, false
}

// rules is the app's own permission rules without the platform's overrides (administrator, auditor, agent): the tenant's roles
// once it manages its own, the platform roles of Defaults while it does not.
func (a Authz) rules(ctx context.Context, c Caller, perm string, g Grant) (Grant, error) {
	var acc *AccessResult
	if a.Perms != nil {
		r, err := a.Perms.Access(ctx, c.Tenant, c.User, c.Roles, a.Point(perm), "")
		if err != nil {
			return g, err
		}
		acc = &r
	}
	return a.apply(c, perm, acc, g), nil
}

// apply is rules on the platform's answer for the point (nil: no permission model wired).
func (a Authz) apply(c Caller, perm string, acc *AccessResult, g Grant) Grant {
	if acc != nil {
		if acc.Managed {
			return Grant{Allowed: acc.Allowed, All: acc.All, Self: acc.Self, Units: acc.Units, Subject: c.User}
		}
		if acc.Personal && acc.Allowed { // basic mode: the person's own grant (ADR-0121), widened below by the platform roles
			g.Allowed, g.Self, g.Units = true, acc.Self, acc.Units
		}
	}
	for _, r := range a.Defaults[perm] {
		role, self := strings.CutSuffix(r, SelfSuffix)
		if !c.Has(role) {
			continue
		}
		g.Allowed = true
		if !self {
			g.All, g.Self = true, false
			break
		}
		g.Self = true
	}
	return g
}

// SelfSuffix marks a role of Authz.Defaults that holds a point over the caller's own records only.
const SelfSuffix = ":self"

// Require is Check for the common case of "may do it at all"; the second result is the grant for scoping records.
func (a Authz) Require(ctx context.Context, c Caller, perm string) (Grant, error) {
	g, err := a.Check(ctx, c, perm)
	if err != nil {
		return g, err
	}
	if !g.Allowed {
		return g, Status(403, "your role does not allow this ("+perm+")")
	}
	return g, nil
}

// HoldsAny reports whether the caller holds at least one of perms, by Check's rules.
func (a Authz) HoldsAny(ctx context.Context, c Caller, perms []string) (bool, error) {
	for _, p := range perms {
		g, err := a.Check(ctx, c, p)
		if err != nil {
			return false, err
		}
		if g.Allowed {
			return true, nil
		}
	}
	return false, nil
}

// UnitOf is the department of the caller ("" when none or without a permission model): record creators keep it so data
// scopes can filter on it later.
func (a Authz) UnitOf(ctx context.Context, c Caller) string {
	if a.Perms == nil {
		return ""
	}
	u, _ := a.Perms.UnitOf(ctx, c.Tenant, c.User)
	return u
}
