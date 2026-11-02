package appkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/21stware/raise-go/k3"
)

// AccessRequests is an app's side of the access requests on its own resources (providers.accessRequests, ADR-0130 P8; the
// requests are the access kernel's: /app/v1/access/{type}/{id}/requests and /app/v1/access/requests…). The kernel files and keeps
// the requests, the cooldown, the notices and the audit; what is the app's — who the caller is on a resource, who decides, how an
// approval is written — the app answers here, over one provider. Only for the types the app keeps its own list or rules for (a
// tree type, an app-rules type); a flat kernel type needs none. The platform's firstparty.AccessRequests.
//
// Each callback runs outside the platform's transaction: the app opens its own.
type AccessRequests struct {
	// Levels is the caller's level on some resources of a type (none, view, comment, edit, manage; manage: they may decide its
	// requests) and, for the resources they manage only, the title. An id left out does not exist (404 to the caller).
	Levels func(ctx context.Context, c Caller, typ string, ids []string) (AccessLevels, error)
	// Deciders are the people told of a new request on a resource (no fallback: an empty list tells nobody).
	Deciders func(ctx context.Context, tenant, typ, id string) ([]string, error)
	// Grant writes an approval: the decider gives user the level (until ExpiresAt when set). The app enforces its own rules (a
	// decider's cap, never lowering an existing grant, the terms it allows); an error refuses the approval and is shown to the decider.
	Grant func(ctx context.Context, decider Caller, in AccessGrant) error
}

// AccessLevels is the answer of AccessRequests.Levels: level names by id, and titles of the ones the caller manages.
type AccessLevels struct {
	Levels map[string]string `json:"levels"`
	Titles map[string]string `json:"titles,omitempty"`
}

// AccessGrant is an approval to write.
type AccessGrant struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	User      string     `json:"user"`
	Name      string     `json:"name"`
	Level     string     `json:"level"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// AccessRequestHost is an app that answers providers.accessRequests (Serve answers it when the manifest declares
// providers.accessRequests). nil: none.
type AccessRequestHost interface {
	AccessRequests(d Deps) *AccessRequests
}

// The app's endpoint of providers.accessRequests (one route, by op) and the platform's time for an answer (the platform's
// providers.PathAccessRequests, AccessRequestsTimeout).
const (
	PathAccessRequests    = "/k3/providers/access-requests"
	AccessRequestsTimeout = 5 * time.Second
)

// The ops of AccessRequestsCall (the platform's providers.AccessOpLevels, AccessOpDeciders, AccessOpGrant).
const (
	AccessOpLevels   = "levels"
	AccessOpDeciders = "deciders"
	AccessOpGrant    = "grant"
)

// AccessRequestsCall is what the platform sends to PathAccessRequests: op "levels" (Caller, Type, IDs), "deciders" (Type, ID) or
// "grant" (Caller: the decider; Grant). The platform's providers.AccessRequestsCall.
type AccessRequestsCall struct {
	Op     string       `json:"op"`
	Caller *Caller      `json:"caller,omitempty"`
	Type   string       `json:"type"`
	ID     string       `json:"id,omitempty"`
	IDs    []string     `json:"ids,omitempty"`
	Grant  *AccessGrant `json:"grant,omitempty"`
}

// serveAccess answers providers.accessRequests for an app that implements AccessRequestHost and declares it.
func serveAccess(mux *http.ServeMux, a App, decl *k3.ProvidersDecl, d Deps) {
	if p, ok := a.(AccessRequestHost); ok && decl.Has("accessRequests") {
		if ar := p.AccessRequests(d); ar != nil {
			serveAccessRequests(mux, ar, d)
		}
	}
}

// serveAccessRequests answers providers.accessRequests: one route, by op. levels and grant are asked for a person (the
// assertion's; the body's caller only adds whether an agent asks), deciders as the platform. A refusal is the app's error as
// Fail answers it (403, 409, 422 with {"error": …}).
//
//	POST /k3/providers/access-requests {op:"levels", caller, type, ids}   → AccessLevels
//	POST /k3/providers/access-requests {op:"deciders", type, id}          → {"users": [..]}
//	POST /k3/providers/access-requests {op:"grant", caller, type, grant}  → 204
func serveAccessRequests(mux *http.ServeMux, ar *AccessRequests, d Deps) {
	mux.HandleFunc("POST "+PathAccessRequests, func(w http.ResponseWriter, r *http.Request) {
		var in AccessRequestsCall
		if !platformJSON(w, r, k3.ViaProvider, &in) {
			return
		}
		p, _ := PlatformOf(r.Context())
		person := func() (Caller, bool) {
			c := p.Caller
			if c.User == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "asked for a person"})
				return Caller{}, false
			}
			if in.Caller != nil {
				if in.Caller.User != c.User || (in.Caller.Tenant != "" && in.Caller.Tenant != c.Tenant) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "another caller"})
					return Caller{}, false
				}
				c.Agent = in.Caller.Agent
			}
			return c, true
		}
		switch in.Op {
		case AccessOpLevels:
			c, ok := person()
			if !ok || ar.Levels == nil {
				if ok {
					writeJSON(w, http.StatusOK, AccessLevels{Levels: map[string]string{}})
				}
				return
			}
			out, err := ar.Levels(r.Context(), c, in.Type, in.IDs)
			if err != nil {
				Fail(w, d.Logger(), err)
				return
			}
			if out.Levels == nil {
				out.Levels = map[string]string{}
			}
			writeJSON(w, http.StatusOK, out)
		case AccessOpDeciders:
			users := []string{}
			if ar.Deciders != nil {
				got, err := ar.Deciders(r.Context(), p.Tenant, in.Type, in.ID)
				if err != nil {
					Fail(w, d.Logger(), err)
					return
				}
				if got != nil {
					users = got
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"users": users})
		case AccessOpGrant:
			c, ok := person()
			if !ok {
				return
			}
			if in.Grant == nil || in.Grant.User == "" || in.Grant.ID == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "a grant needs its resource and person"})
				return
			}
			if in.Grant.Type == "" {
				in.Grant.Type = in.Type
			}
			if ar.Grant == nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "the app takes no approvals"})
				return
			}
			if err := ar.Grant(r.Context(), c, *in.Grant); err != nil {
				Fail(w, d.Logger(), err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown op"})
		}
	})
}

// AccessRequests is Deps.AccessRequestsAPI: one of the access-request routes on the kernel (/app/v1/access/…, ADR-0130 P8), for
// the person c; the answer's status and body as they are (a refusal is the kernel's answer, not an error). An app on its own
// forwards its own /api/access/* request routes here. The platform's remoteapp.Client.AccessRequests.
func (cl *Client) AccessRequests(ctx context.Context, c Caller, method, path string, body []byte) (int, []byte, error) {
	path, err := accessRoute(path)
	if err != nil {
		return 0, nil, err
	}
	var in any
	if len(body) > 0 {
		in = json.RawMessage(body)
	}
	rep, err := cl.do(ctx, c, method, "/app/v1/access"+path, nil, in)
	if err != nil {
		return 0, nil, err
	}
	return rep.status, rep.body, nil
}

// accessRoute is a forwarded path below /app/v1/access, checked: every segment (the query split off first) is decoded and refused
// when the escape is bad, or when it is empty, "." or "..", or holds "/" or "\\" — literally or %-encoded. Such a segment would be
// cleaned away by the platform's router (which answers an unclean path with a redirect) and could name a route outside the access
// requests with the person's token. A double-encoded one (%252e%252e) decodes to the literal "%2e%2e" and stays one harmless segment.
// The client never follows a redirect either (Client.http).
func accessRoute(path string) (string, error) {
	route, query, _ := strings.Cut(path, "?")
	route = "/" + strings.TrimLeft(route, "/")
	for _, seg := range strings.Split(strings.TrimPrefix(route, "/"), "/") {
		dec, err := url.PathUnescape(seg)
		msg := ""
		switch {
		case err != nil:
			msg = "the path has a bad escape"
		case dec == "" || dec == "." || dec == "..":
			msg = "the path has an empty, \".\" or \"..\" segment"
		case strings.ContainsAny(dec, "/\\"):
			msg = "a segment of the path holds a slash"
		}
		if msg != "" {
			return "", fmt.Errorf("%w: %s", ErrInvalid, msg)
		}
	}
	if query != "" {
		route += "?" + query
	}
	return route, nil
}
