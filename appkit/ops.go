package appkit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/21stware/raise-go/k3"
)

// An op is what an app lets the agent propose (ADR-0033). The app declares it in its manifest (title, description, input
// schema, risk, what the person has to do) and implements it as a pair of functions: DryRun says what applying would do
// (checks, consequences, the base it acts on) without changing anything, Execute does it. The same command function should
// serve the app's own HTTP route, so the rule exists once: the platform's draft service calls these two with a signed
// assertion, never with the agent's token.

// OpResult is what DryRun and Execute return.
type OpResult struct {
	// Ref is the k3:// reference of the record the op created or changed.
	Ref    string
	Output any
	// Issues are findings of the dry-run. An Issue with Severity "error" (or none) makes the item invalid; "warning" lets it
	// pass with a note; "choose" (with Candidates) asks the person to pick.
	Issues []k3.Issue
	// Consequences say what applying will cause: notices, approvals, balances, ledger entries. Written by the app.
	Consequences []k3.Consequence
	// Base holds the current values of the fields the op touches (the "before" the person compares with); BaseVersion is an
	// opaque token that changes whenever what the op acts on changes (a version number, an updated-at): the draft goes stale
	// when it differs from the one seen when the draft was made.
	Base        map[string]any
	BaseVersion string
	// Coverage is "full" (default) or "partial" when some checks only run when the op executes.
	Coverage string
	// Changes are the app's own before → after, field by field (ADR-0054 §3): a derived field, or a declared field whose value is a
	// structure (a chart's definition) said in the app's words, which screens show in place of the structure.
	Changes []k3.Change
	// Summary is the app's one sentence on what the op does, in its own words ("张晓雨（M00001）预约「流瑜伽」10-20 19:00，预约后余 11 位"):
	// the change list under a reply and the review bar show it first (ADR-0095 §6). Never personal data: an agent reads it too.
	Summary string
}

// OpFunc is one side of an op.
type OpFunc func(ctx context.Context, c Caller, input json.RawMessage) (OpResult, error)

// Op is one declared op with its two sides.
type Op struct {
	Name    string
	DryRun  OpFunc
	Execute OpFunc
}

// Opped is implemented by an app that has reached L3. OpDecls is the manifest's `ops`; Ops are the implementations (they
// need the app's dependencies). Both must name the same ops: Serve refuses to start otherwise.
type Opped interface {
	OpDecls() map[string]k3.Op
	Ops(d Deps) []Op
}

// OpsAPIFor is the address the draft service calls for an app's ops: {origin}/k3/apps/{id}.
func OpsAPIFor(origin, id string) string { return strings.TrimRight(origin, "/") + "/k3/apps/" + id }

// AddOps raises a manifest to level 3 with the app's op declarations. The ops address is set by the platform when it
// publishes the app (it knows where the app listens).
func AddOps(m *k3.Manifest, decls map[string]k3.Op) {
	if len(decls) == 0 {
		return
	}
	m.Level = k3.L3Draftable
	for name, op := range decls {
		if len(op.InputOrder) == 0 {
			op.InputOrder = propertyOrder(op.Input)
			decls[name] = op
		}
	}
	m.Ops = decls
	m.DryRun = "full"
	m.OpsAPI = OpsAPIFor("http://127.0.0.1:8000", m.ID)
}

// NoBody is the value an app's RequestBodies gives a write route that takes no JSON body (the platform's op parity checks,
// ADR-0091; internal/firstparty/opparity.NoBody there).
var NoBody any = noBody{}

type noBody struct{}

// Invalids builds an error for a dry-run or an execute that carries several findings.
func Invalids(issues ...k3.Issue) error { return &issueError{issues} }

type issueError struct{ issues []k3.Issue }

func (e *issueError) Error() string {
	if len(e.issues) == 0 {
		return "invalid"
	}
	return e.issues[0].Message
}

// OpsHandler serves {origin}/k3/apps/{id}/ops/{op}/{dry-run|execute} for an app: the platform's draft service is the only
// caller, with an assertion it signed (verified here against the platform's keys). idem keeps the first result of an execute per
// key (PGIdempotency; nil: in memory).
func OpsHandler(m *k3.Manifest, ops []Op, platformKeys k3.KeySource, idem IdempotencyStore) (http.Handler, error) {
	have := map[string]Op{}
	for _, o := range ops {
		if _, dup := have[o.Name]; dup {
			return nil, fmt.Errorf("appkit: op %s.%s implemented twice", m.ID, o.Name)
		}
		have[o.Name] = o
	}
	var missing []string
	for name := range m.Ops {
		if _, ok := have[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range have {
		if _, ok := m.Ops[name]; !ok {
			missing = append(missing, name+" (not declared)")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("appkit: %s: ops declared and implemented differ: %s", m.ID, strings.Join(missing, ", "))
	}
	srv, err := newOpServer(m, platformKeys, idem)
	if err != nil {
		return nil, err
	}
	for _, o := range ops {
		srv.handle(o.Name, adapt(o.DryRun), adapt(o.Execute))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+srv.prefix+"/ops/{op}/{phase}", srv.serveOp)
	return mux, nil
}

func adapt(f OpFunc) opFunc {
	return func(ctx context.Context, c opCaller, input json.RawMessage) (k3.ExecuteResult, error) {
		res, err := f(ctx, Caller{Tenant: c.Tenant, User: c.Subject, Name: c.Name, Roles: c.Roles, Via: c.Via}, input)
		if err != nil {
			return k3.ExecuteResult{}, opError(err)
		}
		out := k3.ExecuteResult{Ref: res.Ref, Issues: res.Issues, Consequences: res.Consequences, Base: res.Base, BaseVersion: res.BaseVersion, Coverage: res.Coverage, Changes: res.Changes, Summary: res.Summary}
		if res.Output != nil {
			b, merr := json.Marshal(res.Output)
			if merr != nil {
				return k3.ExecuteResult{}, merr
			}
			out.Output = b
		}
		return out, nil
	}
}

// opError maps what app code returns to what the op server answers: a finding or a refusal is a result the person can read.
func opError(err error) error {
	var is *issueError
	var he *HTTPError
	switch {
	case errors.As(err, &is):
		return invalidOp(is.issues...)
	case errors.As(err, &he):
		return &opFailure{Status: he.Status, Msg: he.Message, Issues: []k3.Issue{{Code: "refused", Message: he.Message}}}
	case errors.Is(err, ErrInvalid):
		return invalidOp(k3.Issue{Code: "invalid", Message: trimSentinel(err, ErrInvalid, "invalid")})
	case errors.Is(err, ErrForbidden):
		return &opFailure{Status: http.StatusForbidden, Msg: "forbidden", Issues: []k3.Issue{{Code: "forbidden", Message: trimSentinel(err, ErrForbidden, "you are not allowed to do this")}}}
	case errors.Is(err, ErrNotFound):
		return invalidOp(k3.Issue{Code: "not_found", Message: "要处理的对象已经不存在了（可能已被删除，或你看不到它）"})
	case errors.Is(err, ErrConflict), errors.Is(err, ErrExists):
		return invalidOp(k3.Issue{Code: "conflict", Message: trimSentinel(err, ErrConflict, "conflicts with what exists")})
	}
	return err
}

// IdempotencyStore remembers the first result of an op's execute per key.
type IdempotencyStore interface {
	Get(ctx context.Context, key string) (k3.ExecuteResult, bool)
	Put(ctx context.Context, key string, res k3.ExecuteResult)
}

type memoryStore struct {
	mu sync.Mutex
	m  map[string]k3.ExecuteResult
}

func (s *memoryStore) Get(_ context.Context, k string) (k3.ExecuteResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[k]
	return r, ok
}

func (s *memoryStore) Put(_ context.Context, k string, r k3.ExecuteResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = r
}

// PGIdempotency keeps the first result of an op per key in the app's database, so a draft run that is resumed after a crash
// cannot do the same thing twice (the draft service re-sends the same key; an in-memory store would not survive the restart).
// Table op_result of the app's schema (Schema; the app's own migration makes it), row-level security by tenant; Schema "" is the
// platform's platform.op_result.
type PGIdempotency struct {
	DB     *sql.DB
	Schema string
}

func (p PGIdempotency) table() string {
	if p.Schema == "" {
		return "platform.op_result"
	}
	return quoteIdent(p.Schema) + ".op_result"
}

func tenantOfKey(key string) string {
	t, _, _ := strings.Cut(key, "/")
	return t
}

func (p PGIdempotency) Get(ctx context.Context, key string) (k3.ExecuteResult, bool) {
	var res k3.ExecuteResult
	var raw []byte
	err := Tx(ctx, p.DB, tenantOfKey(key), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT result FROM `+p.table()+` WHERE tenant_id=$1 AND key=$2`, tenantOfKey(key), key).Scan(&raw)
	})
	if err != nil || json.Unmarshal(raw, &res) != nil {
		return k3.ExecuteResult{}, false
	}
	return res, true
}

func (p PGIdempotency) Put(ctx context.Context, key string, res k3.ExecuteResult) {
	b, _ := json.Marshal(res)
	_ = Tx(ctx, p.DB, tenantOfKey(key), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO `+p.table()+` (tenant_id, key, result, at) VALUES ($1,$2,$3,now()) ON CONFLICT DO NOTHING`, tenantOfKey(key), key, string(b))
		return err
	})
}

// propertyOrder lists the property names of an object schema in the order the schema text gives them.
func propertyOrder(schema json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(schema))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil
		}
		if key == "properties" {
			if t, err := dec.Token(); err != nil || t != json.Delim('{') {
				return nil
			}
			var names []string
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return names
				}
				names = append(names, k.(string))
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					return names
				}
			}
			return names
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}
