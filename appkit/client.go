package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	clientTimeout = 10 * time.Second
	responseMax   = 8 << 20
)

// TokenSource gives the bearer token for a call made for c: the person's App token when c came from
// a request, an installation token otherwise.
type TokenSource func(ctx context.Context, c Caller) (string, error)

// Client reaches the platform's app API (/app/v1, ADR-0066 §2, ADR-0130 §2.2) over HTTP with the meaning of the platform's in-process
// interfaces: Access, Directory, CustomerClient, SiteVisitors, Links, the ledger and the audit chain, and the clients in their own files
// (EntitlementClient, Vault, Sites, OrgStores, Pay, Approvals, …). Errors come back as the kit's
// sentinels (ErrNotFound, ErrInvalid, ErrConflict, ErrForbidden) or an *HTTPError.
type Client struct {
	Base  string       // the platform, e.g. "http://kwork:8000"
	HTTP  *http.Client // nil: a client with a 10 s timeout
	Token TokenSource
}

// Customers is the customer centre (/app/v1/customers).
func (cl *Client) Customers() CustomerClient { return customers{cl} }

// Visitors is the site visitors (/app/v1/site-visitors).
func (cl *Client) Visitors() SiteVisitors { return visitors{cl} }

// Access is the tenant's permission model (/app/v1/access, /app/v1/unit).
func (cl *Client) Access() Access { return access{cl} }

// Directory is the tenant's people (/app/v1/members).
func (cl *Client) Directory() Directory { return directory{cl} }

// Links is the links layer (/app/v1/links).
func (cl *Client) Links() *LinksClient { return &LinksClient{cl: cl} }

func (cl *Client) http() *http.Client {
	base := cl.HTTP
	if base == nil {
		base = &http.Client{Timeout: clientTimeout}
	}
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (cl *Client) baseURL() (*url.URL, error) {
	if strings.TrimSpace(cl.Base) == "" {
		return nil, errors.New("appkit: platform address is empty")
	}
	u, err := url.Parse(cl.Base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("appkit: platform address is not a URL")
	}
	return u, nil
}

func (cl *Client) forBase(u *url.URL) bool {
	base, err := cl.baseURL()
	if err != nil || u == nil {
		return false
	}
	return strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host)
}

type apiReply struct {
	status int
	hdr    http.Header
	body   []byte
}

func (cl *Client) do(ctx context.Context, c Caller, method, path string, query url.Values, in any) (apiReply, error) {
	var zero apiReply
	if cl.Token == nil {
		return zero, errors.New("appkit: no token source")
	}
	token, err := cl.Token(ctx, c)
	if err != nil {
		return zero, err
	}
	base, err := cl.baseURL()
	if err != nil {
		return zero, err
	}
	rel, err := url.Parse(path)
	if err != nil {
		return zero, err
	}
	u := base.ResolveReference(rel)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	if !cl.forBase(u) {
		return zero, errors.New("appkit: request is not for the platform")
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return zero, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	for k, v := range traceOf(ctx) { // the platform's trace goes on through the app and back (ADR-0130 §2.2)
		req.Header.Set(k, v)
	}
	for k, v := range headersOf(ctx) {
		req.Header.Set(k, v)
	}
	if c.Tenant != "" {
		req.Header.Set(HeaderTenant, c.Tenant)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.http().Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, responseMax))
	if err != nil {
		return zero, err
	}
	return apiReply{status: resp.StatusCode, hdr: resp.Header.Clone(), body: b}, nil
}

func (cl *Client) call(ctx context.Context, c Caller, method, path string, query url.Values, in, out any) error {
	rep, err := cl.do(ctx, c, method, path, query, in)
	if err != nil {
		return err
	}
	if err := mapStatus(rep.status, rep.body); err != nil {
		return err
	}
	if out == nil || rep.status == http.StatusNoContent || len(bytes.TrimSpace(rep.body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(rep.body, out); err != nil {
		return fmt.Errorf("appkit: the platform's answer is not the expected JSON: %w", err)
	}
	return nil
}

func mapStatus(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	msg := errorMessage(body)
	switch status {
	case http.StatusNotFound:
		return wrapSentinel(ErrNotFound, msg)
	case http.StatusUnprocessableEntity:
		return wrapSentinel(ErrInvalid, msg)
	case http.StatusConflict:
		return wrapSentinel(ErrConflict, msg)
	case http.StatusForbidden:
		return wrapSentinel(ErrForbidden, msg)
	}
	if status >= 300 {
		if msg == "" {
			msg = http.StatusText(status)
		}
		return &HTTPError{Status: status, Message: msg}
	}
	return nil
}

func wrapSentinel(sent error, msg string) error {
	msg = strings.TrimSpace(msg)
	if msg == "" || msg == sent.Error() {
		return sent
	}
	return fmt.Errorf("%w: %s", sent, msg)
}

// errorMessage is the message of an error answer: the app API's envelope {"error": {"message"}}, or a flat {"error": "…"}.
func errorMessage(body []byte) string {
	var m struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &m) == nil && len(m.Error) > 0 {
		var s string
		if json.Unmarshal(m.Error, &s) == nil {
			return s
		}
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(m.Error, &e) == nil {
			return e.Message
		}
	}
	return strings.TrimSpace(string(body))
}

func tenantOf(hdr http.Header) string {
	if hdr == nil {
		return ""
	}
	return hdr.Get(HeaderTenant)
}

func refuseOtherTenant(hdr http.Header, want string) error {
	if want == "" {
		return nil
	}
	if got := tenantOf(hdr); got != "" && got != want {
		return ErrForbidden
	}
	return nil
}

func pathEscape(s string) string {
	return url.PathEscape(s)
}

func unmarshalJSON(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("appkit: the platform's answer is not the expected JSON: %w", err)
	}
	return nil
}

// ---- access ----

type access struct{ cl *Client }

var _ Access = access{}

func (a access) Access(ctx context.Context, tenant, subject string, platformRoles []string, permission, typeID string) (AccessResult, error) {
	c := Caller{Tenant: tenant, User: subject, Roles: platformRoles}
	q := url.Values{"point": {permission}}
	if typeID != "" {
		q.Set("type", typeID)
	}
	rep, err := a.cl.do(ctx, c, "GET", "/app/v1/access", q, nil)
	if err != nil {
		return AccessResult{}, err
	}
	if err := mapStatus(rep.status, rep.body); err != nil {
		return AccessResult{}, err
	}
	if err := refuseOtherTenant(rep.hdr, tenant); err != nil {
		return AccessResult{}, err
	}
	var acc AccessResult
	if err := unmarshalJSON(rep.body, &acc); err != nil {
		return AccessResult{}, err
	}
	return acc, nil
}

func (a access) UnitOf(ctx context.Context, tenant, subject string) (string, error) {
	c := Caller{Tenant: tenant, User: subject}
	var out struct {
		Unit string `json:"unit"`
	}
	rep, err := a.cl.do(ctx, c, "GET", "/app/v1/unit", nil, nil)
	if err != nil {
		return "", err
	}
	if err := mapStatus(rep.status, rep.body); err != nil {
		return "", err
	}
	if err := refuseOtherTenant(rep.hdr, tenant); err != nil {
		return "", err
	}
	if err := unmarshalJSON(rep.body, &out); err != nil {
		return "", err
	}
	return out.Unit, nil
}

// ---- directory ----

type directory struct{ cl *Client }

var _ Directory = directory{}

func (d directory) Members(ctx context.Context, tenant string) ([]Member, error) {
	c := Caller{Tenant: tenant}
	var out struct {
		Items []Member `json:"items"`
	}
	rep, err := d.cl.do(ctx, c, "GET", "/app/v1/members", nil, nil)
	if err != nil {
		return nil, err
	}
	if err := mapStatus(rep.status, rep.body); err != nil {
		return nil, err
	}
	if err := refuseOtherTenant(rep.hdr, tenant); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(rep.body, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ---- customers ----

type customers struct{ cl *Client }

var _ CustomerClient = customers{}

func (c customers) Resolve(ctx context.Context, caller Caller, req CustomerResolveReq) (CustomerRef, error) {
	var ref CustomerRef
	err := c.cl.call(ctx, caller, "POST", "/app/v1/customers/resolve", nil, req, &ref)
	return ref, err
}

func (c customers) Preview(ctx context.Context, caller Caller, req CustomerResolveReq) (CustomerRef, error) {
	var ref CustomerRef
	err := c.cl.call(ctx, caller, "POST", "/app/v1/customers/resolve", url.Values{"dryRun": {"1"}}, req, &ref)
	return ref, err
}

func (c customers) Get(ctx context.Context, caller Caller, id string) (*CustomerView, error) {
	var v CustomerView
	err := c.cl.call(ctx, caller, "GET", "/app/v1/customers/"+pathEscape(id), nil, nil, &v)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (c customers) Touch(ctx context.Context, caller Caller, req CustomerTouchReq) (bool, error) {
	var out struct {
		Written bool `json:"written"`
	}
	err := c.cl.call(ctx, caller, "POST", "/app/v1/customers/touch", nil, req, &out)
	return out.Written, err
}

// ---- visitors ----

type visitors struct{ cl *Client }

var _ SiteVisitors = visitors{}

// VerifiedCustomerOf and LinkedCustomerOf read the same route: it answers the tie and whether it was proved.
func (v visitors) VerifiedCustomerOf(ctx context.Context, c Caller, site, visitor string) (string, error) {
	id, verified, err := v.tie(ctx, c, site, visitor)
	if err != nil || !verified {
		return "", err
	}
	return id, nil
}

func (v visitors) LinkedCustomerOf(ctx context.Context, c Caller, site, visitor string) (string, error) {
	id, _, err := v.tie(ctx, c, site, visitor)
	return id, err
}

func (v visitors) tie(ctx context.Context, c Caller, site, visitor string) (string, bool, error) {
	var out struct {
		Customer string `json:"customer"`
		Verified bool   `json:"verified"`
	}
	err := v.cl.call(ctx, c, "GET", "/app/v1/site-visitors/"+pathEscape(site)+"/"+pathEscape(visitor), nil, nil, &out)
	return out.Customer, out.Verified, err
}

func (v visitors) Identified(ctx context.Context, c Caller, after int64, limit int) ([]IdentifiedVisitor, error) {
	q := url.Values{
		"after": {strconv.FormatInt(after, 10)},
		"limit": {strconv.Itoa(limit)},
	}
	var out struct {
		Items []IdentifiedVisitor `json:"items"`
	}
	err := v.cl.call(ctx, c, "GET", "/app/v1/site-visitors", q, nil, &out)
	return out.Items, err
}

func (v visitors) Identify(ctx context.Context, c Caller, site, visitor, customer string) (string, error) {
	var out struct {
		Customer string `json:"customer"`
	}
	err := v.cl.call(ctx, c, "PUT", "/app/v1/site-visitors/"+pathEscape(site)+"/"+pathEscape(visitor), nil, map[string]string{"customer": customer}, &out)
	return out.Customer, err
}

// ---- ledger ----

// PostLedger hands a ledger posting to the platform (POST /app/v1/ledger/posts; Deps.LedgerAPI). The answer is its key.
func (cl *Client) PostLedger(ctx context.Context, c Caller, req PostReq) (string, error) {
	var out struct {
		Key string `json:"key"`
	}
	err := cl.call(ctx, c, "POST", "/app/v1/ledger/posts", nil, req, &out)
	return out.Key, err
}

// ---- links ----

// LinksClient is the platform's links layer as an app on its own asks it (/app/v1/links): another app's records, as the caller sees them.
type LinksClient struct{ cl *Client }

var _ Links = (*LinksClient)(nil)

func (l *LinksClient) SearchLinksIn(ctx context.Context, c Caller, app, query string, limit int) ([]Link, error) {
	var out struct {
		Items []Link `json:"items"`
	}
	q := url.Values{"app": {app}, "q": {query}, "limit": {strconv.Itoa(limit)}}
	err := l.cl.call(ctx, c, "GET", "/app/v1/links", q, nil, &out)
	return out.Items, err
}

func (l *LinksClient) ResolveLink(ctx context.Context, c Caller, ref string) (Link, bool, error) {
	var out struct {
		Found bool `json:"found"`
		Link  Link `json:"link"`
	}
	err := l.cl.call(ctx, c, "GET", "/app/v1/links/resolve", url.Values{"ref": {ref}}, nil, &out)
	return out.Link, out.Found, err
}

// Installed says whether the tenant has app installed and on (Deps.AppInstalled of an app on its own): the links layer answers it.
func (l *LinksClient) Installed(ctx context.Context, tenant, app string) bool {
	var out struct {
		Installed bool `json:"installed"`
	}
	err := l.cl.call(ctx, Caller{Tenant: tenant}, "GET", "/app/v1/links", url.Values{"app": {app}, "limit": {"1"}}, nil, &out)
	return err == nil && out.Installed
}

// ---- audit ----

// Audit writes one entry on the tenant's chain as the caller (POST /app/v1/audit; Deps.AuditAPI).
func (cl *Client) Audit(ctx context.Context, c Caller, action, resource string, diff any) error {
	body := map[string]any{"action": action, "resource": resource}
	if diff != nil {
		raw, err := json.Marshal(diff)
		if err != nil {
			return err
		}
		body["diff"] = json.RawMessage(raw)
	}
	return cl.call(ctx, c, "POST", "/app/v1/audit", nil, body, nil)
}

// ---- tenants ----

// Tenants lists the tenants that have the app installed (GET /app/v1/installations; the app's own token only).
func (cl *Client) Tenants(ctx context.Context, c Caller) ([]string, error) {
	var out struct {
		Tenants []string `json:"tenants"`
	}
	err := cl.call(ctx, c, "GET", "/app/v1/installations", nil, nil, &out)
	return out.Tenants, err
}
