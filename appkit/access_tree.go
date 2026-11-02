package appkit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	sdk "github.com/21stware/raise-go/access"
	"github.com/21stware/raise-go/provide"
)

// Tree is the SDK's access.Tree over the app API's access routes of a tree type (/app/v1/access/{type}/…, internal/access
// tree_routes.go): the calls that name no person go with the app's tenant token, Mine and Level with the person's (and name them,
// as check does, for an app's token).
func (cl *Client) Tree() sdk.Tree { return tree{cl} }

type tree struct{ cl *Client }

var _ sdk.Tree = tree{}

func treePath(typ, id string, rest ...string) string {
	p := "/app/v1/access/" + pathEscape(typ)
	if id != "" {
		p += "/" + pathEscape(id)
	}
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

type treeChanged struct {
	Changed map[string]string `json:"changed"`
}

func (t tree) PutRoot(ctx context.Context, tenant, typ, id string, spec sdk.RootSpec) error {
	body := map[string]any{"archived": spec.Archived, "publishOnly": spec.PublishOnly}
	for k, on := range map[string]bool{"private": spec.Private, "keepManager": spec.KeepManager, "reshare": spec.Reshare, "alwaysInherit": spec.AlwaysInherit} {
		if on { // a root's rules go only when set: an older platform sees the body it knows
			body[k] = true
		}
	}
	return t.cl.call(ctx, Caller{Tenant: tenant}, "POST", treePath(typ, id), nil, body, nil)
}

func (t tree) PutNode(ctx context.Context, tenant, typ, id string, spec sdk.NodeSpec) (sdk.Changed, error) {
	body := map[string]any{"root": spec.Root, "parent": spec.Parent, "inherit": spec.Inherit, "trashed": spec.Trashed}
	var out treeChanged
	if err := t.cl.call(ctx, Caller{Tenant: tenant}, "POST", treePath(typ, id), nil, body, &out); err != nil {
		return nil, err
	}
	return changedOf(out.Changed), nil
}

func (t tree) Delete(ctx context.Context, tenant, typ string, ids ...string) error {
	for _, id := range ids {
		if err := t.cl.call(ctx, Caller{Tenant: tenant}, "DELETE", treePath(typ, id), nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

type wireEntry struct {
	Kind      string     `json:"kind"`
	Principal string     `json:"principal"`
	Level     string     `json:"level"`
	Name      string     `json:"name"`
	At        string     `json:"at,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

func (t tree) SetEntries(ctx context.Context, tenant, typ, id string, entries []sdk.Entry, by string) (sdk.Changed, error) {
	es := make([]wireEntry, 0, len(entries))
	for _, e := range entries {
		if e.Level < sdk.View || e.Level > sdk.Manage {
			return nil, fmt.Errorf("%w: level of %s:%s is View … Manage", ErrInvalid, e.Kind, e.Principal)
		}
		es = append(es, wireEntry{Kind: e.Kind, Principal: e.Principal, Level: levelName(e.Level), Name: e.Name, ExpiresAt: e.ExpiresAt})
	}
	var out treeChanged
	body := map[string]any{"entries": es, "by": by}
	if err := t.cl.call(ctx, Caller{Tenant: tenant}, "PUT", treePath(typ, id, "grants"), nil, body, &out); err != nil {
		return nil, err
	}
	return changedOf(out.Changed), nil
}

type treeView struct {
	Entries   []wireEntry `json:"entries"`
	Effective []wireEntry `json:"effective"`
	Set       string      `json:"set"`
	Level     string      `json:"level"`
	Reason    string      `json:"reason"`
}

func (t tree) Entries(ctx context.Context, tenant, typ, id string) ([]sdk.Entry, error) {
	var v treeView
	if err := t.cl.call(ctx, Caller{Tenant: tenant}, "GET", treePath(typ, id), nil, nil, &v); err != nil {
		return nil, err
	}
	out := make([]sdk.Entry, 0, len(v.Entries))
	for _, e := range v.Entries {
		l := parseLevel(e.Level)
		at, _ := time.Parse(time.RFC3339Nano, e.At)
		out = append(out, sdk.Entry{Kind: e.Kind, Principal: e.Principal, Level: l, Name: e.Name, At: at, ExpiresAt: e.ExpiresAt})
	}
	return out, nil
}

func (t tree) Effective(ctx context.Context, tenant, typ, id string) ([]sdk.Entry, error) {
	var v treeView
	if err := t.cl.call(ctx, Caller{Tenant: tenant}, "GET", treePath(typ, id), nil, nil, &v); err != nil {
		return nil, err
	}
	out := make([]sdk.Entry, 0, len(v.Effective))
	for _, e := range v.Effective {
		l := parseLevel(e.Level)
		out = append(out, sdk.Entry{Kind: e.Kind, Principal: e.Principal, Level: l})
	}
	return out, nil
}

func (t tree) Sets(ctx context.Context, tenant, typ string, ids ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		var v treeView
		err := t.cl.call(ctx, Caller{Tenant: tenant}, "GET", treePath(typ, id), nil, nil, &v)
		switch {
		case errors.Is(err, ErrNotFound):
			continue // a node the kernel does not have has no set
		case err != nil:
			return nil, err
		}
		out[id] = v.Set
	}
	return out, nil
}

func (t tree) Mine(ctx context.Context, c provide.Caller, after string) (sdk.Mine, error) {
	q := url.Values{"user": {c.User}}
	if after != "" {
		q.Set("after", after)
	}
	var in struct {
		Sets      map[string]string `json:"sets"`
		Truncated bool              `json:"truncated"`
		Next      string            `json:"next"`
		Bypass    bool              `json:"bypass"`
	}
	// the sets are the same for every type: "*" names none
	if err := t.cl.call(ctx, c, "GET", "/app/v1/access/*/mine", q, nil, &in); err != nil {
		return sdk.Mine{}, err
	}
	out := sdk.Mine{Sets: make(map[string]int, len(in.Sets)), Truncated: in.Truncated, Next: in.Next, Bypass: in.Bypass}
	for id, name := range in.Sets {
		l := parseLevel(name)
		out.Sets[id] = l
	}
	return out, nil
}

func (t tree) Level(ctx context.Context, c provide.Caller, typ, id string) (sdk.Level, error) {
	var in struct {
		Levels  map[string]string `json:"levels"`
		Reasons map[string]string `json:"reasons"`
	}
	body := map[string]any{"ids": []string{id}, "user": c.User, "agent": c.Agent != ""}
	if err := t.cl.call(ctx, c, "POST", treePath(typ, "", "check"), nil, body, &in); err != nil {
		return sdk.Level{}, err
	}
	l := parseLevel(in.Levels[id])
	return sdk.Level{Level: l, Reason: in.Reasons[id]}, nil
}

func (t tree) Reconcile(ctx context.Context, tenant, rootType, root string, spec sdk.RootSpec, nodes []sdk.Node) (int, error) {
	if nodes == nil {
		nodes = []sdk.Node{}
	}
	var out struct {
		Changed int `json:"changed"`
	}
	body := map[string]any{"spec": spec, "nodes": nodes}
	if err := t.cl.call(ctx, Caller{Tenant: tenant}, "POST", treePath(rootType, root, "reconcile"), nil, body, &out); err != nil {
		return 0, err
	}
	return out.Changed, nil
}

func changedOf(m map[string]string) sdk.Changed {
	out := sdk.Changed{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// levelName is a level's name on the wire (the platform's acl.Level.Name).
func levelName(l int) string {
	switch {
	case l >= sdk.Manage:
		return "manage"
	case l == sdk.Edit:
		return "edit"
	case l == sdk.Comment:
		return "comment"
	case l == sdk.View:
		return "view"
	}
	return "none"
}

// parseLevel is the level of a name on the wire (the platform's acl.ParseLevel); 0 for any other name.
func parseLevel(s string) int {
	switch s {
	case "view":
		return sdk.View
	case "comment":
		return sdk.Comment
	case "edit":
		return sdk.Edit
	case "manage":
		return sdk.Manage
	}
	return 0
}
