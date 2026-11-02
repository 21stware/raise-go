package appkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/21stware/raise-go/k3"
)

// Drafts is the platform's draft service over /app/v1/drafts (ADR-0130 §3.2 H7): Deps.Drafts = cl.Drafts(), and MountDrafts serves the
// person's drafts on the app's router. The review calls go out with the person's page session; a refusal of the draft service comes
// back with its status and details (DraftError), so MountDrafts answers it unchanged. Create is the app drafting for a person.
func (cl *Client) Drafts() *Drafts { return &Drafts{cl: cl} }

// Drafts implements DraftService over HTTP.
type Drafts struct{ cl *Client }

var _ DraftService = (*Drafts)(nil)

// DraftCreateReq is the body of POST /app/v1/drafts: the draft and the person it is for (the page session's person when For is empty;
// required on the app's own call).
type DraftCreateReq struct {
	For string `json:"for,omitempty"`
	k3.DraftCreate
}

// DraftError is a refusal of the platform's draft service: its status, message and the fields it carries ({item, missing, confirm…}).
type DraftError struct {
	Status  int
	Message string
	Extra   map[string]any
}

func (e *DraftError) Error() string           { return e.Message }
func (e *DraftError) HTTPStatus() int         { return e.Status }
func (e *DraftError) Details() map[string]any { return e.Extra }

func (d *Drafts) do(ctx context.Context, a k3.DraftActor, method, path string, q url.Values, in, out any) error {
	c := Caller{Tenant: a.Tenant, User: a.Subject, Name: a.Name, Roles: a.Roles}
	if a.Delegated { // the agent: it reads, with its own delegated session (the platform redacts what only the person fills)
		s := agentSessionOf(ctx)
		if method != http.MethodGet || s.token == "" || s.tenant != a.Tenant || s.user != a.Subject || s.agent != a.Agent {
			return &DraftError{Status: http.StatusForbidden, Message: "the assistant only reads drafts, with its own session for the person"}
		}
		ctx, c.Agent = withSessionToken(ctx, s.token), a.Agent
	}
	rep, err := d.cl.do(ctx, c, method, path, q, in)
	if err != nil {
		return err
	}
	if err := refuseOtherTenant(rep.hdr, a.Tenant); err != nil {
		return err
	}
	if rep.status < 200 || rep.status >= 300 {
		return draftError(rep.status, rep.body)
	}
	if out == nil || len(strings.TrimSpace(string(rep.body))) == 0 {
		return nil
	}
	return unmarshalJSON(rep.body, out)
}

// draftError keeps what the draft service said: the message and the other fields of its answer (the app API's envelope or flat).
func draftError(status int, body []byte) error {
	e := &DraftError{Status: status, Message: errorMessage(body)}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		if env, ok := m["error"]; ok && len(env) > 0 && env[0] == '{' { // {"error": {"code", "message", "details"}}
			var inner struct {
				Details map[string]any `json:"details"`
			}
			if json.Unmarshal(env, &inner) == nil && len(inner.Details) > 0 {
				e.Extra = inner.Details
			}
		}
		for k, v := range m {
			if k == "error" || k == "code" {
				continue
			}
			var x any
			if json.Unmarshal(v, &x) == nil {
				if e.Extra == nil {
					e.Extra = map[string]any{}
				}
				e.Extra[k] = x
			}
		}
	}
	return e
}

func (d *Drafts) List(ctx context.Context, a k3.DraftActor, f k3.DraftListFilter) ([]*k3.DraftView, error) {
	q := url.Values{}
	for k, v := range map[string]string{"status": f.Status, "target": f.Target, "session": f.Session} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var out struct {
		Drafts []*k3.DraftView `json:"drafts"`
	}
	err := d.do(ctx, a, "GET", "/app/v1/drafts", q, nil, &out)
	return out.Drafts, err
}

func (d *Drafts) Get(ctx context.Context, a k3.DraftActor, id string) (*k3.DraftView, error) {
	var v k3.DraftView
	if err := d.do(ctx, a, "GET", "/app/v1/drafts/"+pathEscape(id), nil, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (d *Drafts) Revalidate(ctx context.Context, a k3.DraftActor, id string) (*k3.DraftView, error) {
	var v k3.DraftView
	if err := d.do(ctx, a, "GET", "/app/v1/drafts/"+pathEscape(id), url.Values{"revalidate": {"1"}}, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (d *Drafts) Review(ctx context.Context, a k3.DraftActor, id, item string, e k3.ItemEdit) (*k3.DraftItem, []string, error) {
	var out struct {
		Item *k3.DraftItem `json:"item"`
	}
	err := d.do(ctx, a, "PATCH", "/app/v1/drafts/"+pathEscape(id)+"/items/"+pathEscape(item), nil, e, &out)
	return out.Item, nil, err
}

func (d *Drafts) Abandon(ctx context.Context, a k3.DraftActor, id, item string) error {
	return d.do(ctx, a, "POST", "/app/v1/drafts/"+pathEscape(id)+"/items/"+pathEscape(item)+"/abandon", nil, struct{}{}, nil)
}

func (d *Drafts) Rebase(ctx context.Context, a k3.DraftActor, id, item string) (*k3.DraftItem, error) {
	var out struct {
		Item *k3.DraftItem `json:"item"`
	}
	err := d.do(ctx, a, "POST", "/app/v1/drafts/"+pathEscape(id)+"/items/"+pathEscape(item)+"/rebase", nil, struct{}{}, &out)
	return out.Item, err
}

func (d *Drafts) Apply(ctx context.Context, a k3.DraftActor, id string, sel []k3.ApplyItem) (*k3.ApplyOutcome, error) {
	var out k3.ApplyOutcome
	body := map[string]any{"items": sel}
	if sel == nil {
		body["items"] = []k3.ApplyItem{}
	}
	if err := d.do(ctx, a, "POST", "/app/v1/drafts/"+pathEscape(id)+"/apply", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d *Drafts) Provenance(ctx context.Context, a k3.DraftActor, ref string) ([]k3.Provenance, error) {
	var out struct {
		Provenance []k3.Provenance `json:"provenance"`
	}
	err := d.do(ctx, a, "GET", "/app/v1/provenance", url.Values{"ref": {ref}}, nil, &out)
	return out.Provenance, err
}

// Create drafts changes with the app's own ops for a person (req.For; the page session's person when c is one) to confirm. The answer
// is the draft as the app sees it.
func (d *Drafts) Create(ctx context.Context, c Caller, req DraftCreateReq) (*k3.DraftView, error) {
	var v k3.DraftView
	if err := d.do(ctx, k3.DraftActor{Tenant: c.Tenant, Subject: c.User, Name: c.Name, Roles: c.Roles}, "POST", "/app/v1/drafts", nil, req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
