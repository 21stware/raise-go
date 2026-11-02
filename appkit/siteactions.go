package appkit

import (
	"context"
	"encoding/json"

	"github.com/21stware/raise-go/k3"
)

// Site actions an app serves on its tenants' sites (/_k/v1/{name} on the site's own host, ADR-0049 §6), through the platform instead of
// the platform calling the app's package (ADR-0066 §1, docs/handover/handover-distribution-pilot.md #5). The platform keeps everything that is
// the site's: the method, same origin and JSON for a write, the body limit, the per-address and per-visitor limits, the visitor cookie
// (made, or required to be there already), and salted hashes of the address and the browser. The app gets a clean request and answers
// with a status and a body. The remote form is the same request posted to the app with a signed assertion; the specs become the
// manifest's siteActions.

// Visitor requirements of a site action.
const (
	SiteVisitorNone       = ""           // the action does not need to know the visitor
	SiteVisitorMake       = "make"       // the visitor cookie is made when missing (a promotion link opened by a new browser)
	SiteVisitorIdentified = "identified" // the cookie must be there already; without it 401 {"code":"identify"} (go through wechat.auth)
)

// SiteActionSpec is how the platform guards one action before the app sees it.
type SiteActionSpec struct {
	Name          string // "dist.touch": the app's prefix, a dot, the action
	Method        string // GET (HEAD too) or POST
	Visitor       string // SiteVisitor*
	PerAddress    int    // requests per address and minute
	PerVisitor    int    // per visitor and minute (with a visitor)
	PerVisitorDay int    // per visitor and day (0: no daily limit)
	PerSite       int    // all requests to the action on the site, per minute (0: none): against a guess spread over many addresses
	MaxBody       int    // bytes of a POST body (0: 4 KiB)
	// LooseBody accepts a POST whose Content-Type is not application/json (pages built before the rule); the body must still be JSON.
	LooseBody bool
	// OpenID and Referrer ask for SiteActionReq.OpenID / AppID and SiteActionReq.Referrer (both need a visitor; the referrer is
	// waited for up to 300 ms, so only an action that records a distribution source asks for it).
	OpenID, Referrer bool
}

// SiteActionReq is the request the app gets.
type SiteActionReq struct {
	Tenant, Site, Action string
	Visitor              string            // the visitor id ("" when the spec needs none)
	Origin               string            // "https://host" of the site the visitor is on (for links)
	IPHash, DeviceHash   string            // salted hashes of the address and the browser, stable per tenant; never the values
	Unit                 string            // the department the site belongs to (records the visitor writes are owned by it); "" none
	Query                map[string]string // the query string of a GET (first value of each key)
	Body                 json.RawMessage   // the body of a POST (JSON, within MaxBody), nil for a GET
	// AppID and OpenID: the tenant's WeChat official account and the visitor's openid for it, when the spec asks (OpenID) and
	// WeChat has authorised this visitor on this site; "" otherwise. Referrer: the customer id of the distributor who sent the
	// visitor, when the spec asks (Referrer); "" when nobody did. The visitor's address never leaves the platform, only IPHash.
	AppID, OpenID, Referrer string
}

// SiteActionResult is the app's answer: Body is written as JSON with Status. A refusal is a result too ({"error", "code"}); an
// error is reserved for what the app could not handle at all, which the platform answers 500 without detail.
type SiteActionResult struct {
	Status int
	Body   any
	// CacheSeconds lets a public answer be cached that long (Cache-Control: public, max-age=N); 0: not cached.
	CacheSeconds int
}

// SiteActionProvider serves an app's site actions.
type SiteActionProvider interface {
	SiteActionSpecs() []SiteActionSpec
	SiteAction(ctx context.Context, req SiteActionReq) (SiteActionResult, error)
}

// SiteActing is implemented by an App that serves site actions. SiteActions must return the specs even for an empty Deps (tests and
// the description table list them without a database).
type SiteActing interface {
	SiteActions(d Deps) SiteActionProvider
}

// SiteRefusal is the body of a refused site action.
func SiteRefusal(status int, code, msg string) SiteActionResult {
	return SiteActionResult{Status: status, Body: map[string]string{"error": msg, "code": code}}
}

// Protocol is the spec as a manifest carries it (k3.SiteActionSpec).
func (s SiteActionSpec) Protocol() k3.SiteActionSpec {
	return k3.SiteActionSpec{Name: s.Name, Method: s.Method, Visitor: s.Visitor, PerAddress: s.PerAddress, PerVisitor: s.PerVisitor,
		PerVisitorDay: s.PerVisitorDay, PerSite: s.PerSite, MaxBody: s.MaxBody, LooseBody: s.LooseBody, OpenID: s.OpenID, Referrer: s.Referrer}
}

// SiteActionAnswer is what an app answers for a site action (POST /k3/site-actions/{name}; the platform's providers.SiteActionAnswer).
type SiteActionAnswer struct {
	Status       int             `json:"status"`
	Body         json.RawMessage `json:"body"`
	CacheSeconds int             `json:"cacheSeconds,omitempty"`
}
