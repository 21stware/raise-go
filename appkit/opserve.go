package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/21stware/raise-go/k3"
)

// The op server: the write-gate of the platform's app SDK (packages/app-sdk/go, SYSTEM-DESIGN §9) for the ops of an app, copied with
// what OpsHandler uses of it. The platform's draft service is the only caller, with its assertion:
//
//   - no assertion                           → 401 (an op is not called with a person's token here)
//   - a dry-run                              → a dry-run assertion bound to the op and the input
//   - an execute                             → signature, audience, op, input digest, expiry and agentDraftable are all checked
//   - the same idempotency key returns the first result

// opCaller is who executes an op (the SDK's Caller).
type opCaller struct {
	Subject   string
	Tenant    string
	Roles     []string
	Name      string
	Via       string
	Delegated bool
	Assertion *k3.AssertionClaims
}

type opFunc func(ctx context.Context, c opCaller, input json.RawMessage) (k3.ExecuteResult, error)

type opHandler struct{ dryRun, execute opFunc }

// opFailure is a domain error with an HTTP status and issues (the SDK's OpError).
type opFailure struct {
	Status int
	Issues []k3.Issue
	Msg    string
}

func (e *opFailure) Error() string { return e.Msg }

func invalidOp(issues ...k3.Issue) *opFailure {
	return &opFailure{Status: http.StatusUnprocessableEntity, Issues: issues, Msg: "validation failed"}
}

type opServer struct {
	m      *k3.Manifest
	keys   k3.KeySource
	idem   IdempotencyStore
	ops    map[string]opHandler
	prefix string
}

func newOpServer(m *k3.Manifest, keys k3.KeySource, idem IdempotencyStore) (*opServer, error) {
	if m == nil {
		return nil, errors.New("appkit: manifest required")
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("appkit: invalid manifest: %w", err)
	}
	if idem == nil {
		idem = &memoryStore{m: map[string]k3.ExecuteResult{}}
	}
	prefix := "/k3"
	if u, err := url.Parse(m.OpsAPI); err == nil && u.Path != "" {
		prefix = strings.TrimRight(u.Path, "/")
	}
	return &opServer{m: m, keys: keys, idem: idem, ops: map[string]opHandler{}, prefix: prefix}, nil
}

func (a *opServer) handle(op string, dryRun, execute opFunc) {
	a.ops[op] = opHandler{dryRun: dryRun, execute: execute}
}

type opHTTPErr struct {
	status int
	msg    string
}

func (e *opHTTPErr) Error() string { return e.msg }

func (a *opServer) serveOp(w http.ResponseWriter, r *http.Request) {
	opName, phase := r.PathValue("op"), r.PathValue("phase")
	h, ok := a.ops[opName]
	if !ok || (phase != "dry-run" && phase != "execute") {
		writeErr(w, http.StatusNotFound, "unknown op or phase")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	var req k3.ExecuteRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		writeErr(w, http.StatusBadRequest, "body must be {\"input\":{...}}")
		return
	}

	var caller opCaller
	if phase == "dry-run" {
		caller, err = a.authDryRun(r, opName, req)
	} else {
		caller, err = a.authExecute(r, opName, req)
	}
	if err != nil {
		var he *opHTTPErr
		if errors.As(err, &he) {
			writeErr(w, he.status, he.msg)
			return
		}
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	ctx := r.Context()

	if phase == "dry-run" {
		res, err := h.dryRun(ctx, caller, req.Input)
		a.respond(w, res, err, false)
		return
	}

	key := req.IdempotencyKey
	if caller.Assertion != nil {
		key = caller.Assertion.IdempotencyKey
	}
	if key != "" {
		// Scope by tenant and subject so one user can never receive another user's stored result.
		scoped := caller.Tenant + "/" + caller.Subject + "/" + opName + "/" + key
		if prev, ok := a.idem.Get(ctx, scoped); ok {
			writeJSON(w, http.StatusOK, prev)
			return
		}
		res, err := h.execute(ctx, caller, req.Input)
		if err == nil {
			res.Applied = true
			a.idem.Put(ctx, scoped, res)
		}
		a.respond(w, res, err, true)
		return
	}
	res, err := h.execute(ctx, caller, req.Input)
	a.respond(w, res, err, true)
}

func (a *opServer) respond(w http.ResponseWriter, res k3.ExecuteResult, err error, applied bool) {
	if err != nil {
		var oe *opFailure
		if errors.As(err, &oe) {
			writeJSON(w, oe.Status, map[string]any{"error": oe.Msg, "issues": oe.Issues})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	res.Applied = applied
	writeJSON(w, http.StatusOK, res)
}

// errNoAssertion is the answer to an op call without the platform's assertion.
var errNoAssertion = &opHTTPErr{http.StatusUnauthorized, "an op is called by the platform with its assertion"}

// authDryRun accepts what the draft service sends: a dry-run assertion bound to the op and the input. A dry-run never writes, so a
// delegated (agent) caller is fine.
func (a *opServer) authDryRun(r *http.Request, opName string, req k3.ExecuteRequest) (opCaller, error) {
	raw := r.Header.Get(k3.HeaderAssertion)
	if raw == "" {
		return opCaller{}, errNoAssertion
	}
	var c k3.AssertionClaims
	if err := k3.Verify(r.Context(), raw, k3.TypAssertion, a.keys, &c); err != nil {
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion rejected: " + err.Error()}
	}
	switch {
	case c.Via != k3.ViaDryRun:
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "this assertion is not for a dry-run"}
	case !c.Aud.Contains(a.m.ID):
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion audience mismatch"}
	case c.Op != opName:
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion bound to a different op"}
	}
	if d, err := k3.InputDigest(req.Input); err != nil || d != c.InputDigest {
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "input does not match the assertion"}
	}
	return opCaller{Subject: c.Sub, Tenant: c.Tenant, Roles: c.Roles, Name: c.Name, Via: c.Via, Delegated: c.Delegated, Assertion: &c}, nil
}

func (a *opServer) authExecute(r *http.Request, opName string, req k3.ExecuteRequest) (opCaller, error) {
	raw := r.Header.Get(k3.HeaderAssertion)
	if raw == "" {
		return opCaller{}, errNoAssertion
	}
	var c k3.AssertionClaims
	if err := k3.Verify(r.Context(), raw, k3.TypAssertion, a.keys, &c); err != nil {
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion rejected: " + err.Error()}
	}
	m := a.m
	switch {
	case !c.Aud.Contains(m.ID):
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion audience mismatch"}
	case c.Op != opName:
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion bound to a different op"}
	case c.Via == k3.ViaDryRun:
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "a dry-run assertion cannot execute"}
	case (c.Via == k3.ViaDraft || c.Via == k3.ViaPolicy || c.Via == k3.ViaConfirm || c.Via == k3.ViaDirect) && !m.Ops[opName].Draftable():
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "op is not agent-draftable"}
	case c.IdempotencyKey == "":
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "assertion without idempotency key"}
	}
	d, err := k3.InputDigest(req.Input)
	if err != nil || d != c.InputDigest {
		return opCaller{}, &opHTTPErr{http.StatusForbidden, "input does not match the reviewed revision"}
	}
	return opCaller{Subject: c.Sub, Tenant: c.Tenant, Roles: c.Roles, Name: c.Name, Via: c.Via, Assertion: &c}, nil
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
