package appkit

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/21stware/raise-go/k3"
)

// Approvals is the approval service as an app reaches it (Deps.Approvals, Client.Approvals). The app's kinds are its manifest's
// `approvals`; the platform asks the app for the facts (PREPARE) and tells it the outcome (DECIDED), so nothing here shares a
// transaction with the app. The person's calls go out with their page session.
type Approvals interface {
	Submit(ctx context.Context, c Caller, in k3.ApprovalSubmit) (k3.ApprovalRef, error)
	Resubmit(ctx context.Context, c Caller, id string, in k3.ApprovalSubmit) (k3.ApprovalRef, error)
	Withdraw(ctx context.Context, c Caller, id, reason string) (k3.ApprovalRef, error)
	// Of is nil (no error) when the object has no approval the person may see.
	Of(ctx context.Context, c Caller, subject string) (*k3.ApprovalView, error)
	// Waiting lists the person's pending tasks of a kind, at most limit of them (0: the platform's bound).
	Waiting(ctx context.Context, c Caller, kind string, limit int) ([]k3.ApprovalWaiting, error)
	Act(ctx context.Context, c Caller, task string, act k3.ApprovalAct) (*k3.ApprovalView, error)
	Participates(ctx context.Context, c Caller, subject, user string) (bool, error)
}

// Approvals is the approval service over /app/v1/approvals (ADR-0130 §3.2 H7): Deps.Approvals = cl.Approvals().
func (cl *Client) Approvals() Approvals { return approvalsClient{cl} }

type approvalsClient struct{ cl *Client }

var _ Approvals = approvalsClient{}

// asWho is the context of a call made for c: the app's own call (no person: no subject, or the app's own "app:{id}") goes with the app's tenant token even inside a person's
// request (their page session is on the context there).
func asWho(ctx context.Context, c Caller) context.Context {
	if !isPerson(c) {
		return WithPersonToken(ctx, "", "")
	}
	return ctx
}

func (a approvalsClient) Submit(ctx context.Context, c Caller, in k3.ApprovalSubmit) (k3.ApprovalRef, error) {
	var r k3.ApprovalRef
	err := a.cl.call(asWho(ctx, c), c, "POST", "/app/v1/approvals", nil, in, &r)
	return r, err
}

func (a approvalsClient) Resubmit(ctx context.Context, c Caller, id string, in k3.ApprovalSubmit) (k3.ApprovalRef, error) {
	var r k3.ApprovalRef
	err := a.cl.call(asWho(ctx, c), c, "POST", "/app/v1/approvals/"+pathEscape(id)+"/resubmit", nil, in, &r)
	return r, err
}

func (a approvalsClient) Withdraw(ctx context.Context, c Caller, id, reason string) (k3.ApprovalRef, error) {
	var r k3.ApprovalRef
	body := struct {
		Reason string `json:"reason,omitempty"`
	}{reason}
	err := a.cl.call(asWho(ctx, c), c, "POST", "/app/v1/approvals/"+pathEscape(id)+"/withdraw", nil, body, &r)
	return r, err
}

func (a approvalsClient) Of(ctx context.Context, c Caller, subject string) (*k3.ApprovalView, error) {
	var v k3.ApprovalView
	if err := a.cl.call(asWho(ctx, c), c, "GET", "/app/v1/approvals", url.Values{"subject": {subject}}, nil, &v); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

func (a approvalsClient) Waiting(ctx context.Context, c Caller, kind string, limit int) ([]k3.ApprovalWaiting, error) {
	q := url.Values{"kind": {kind}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Items []k3.ApprovalWaiting `json:"items"`
	}
	err := a.cl.call(asWho(ctx, c), c, "GET", "/app/v1/approvals/waiting", q, nil, &out)
	return out.Items, err
}

func (a approvalsClient) Act(ctx context.Context, c Caller, task string, act k3.ApprovalAct) (*k3.ApprovalView, error) {
	var v k3.ApprovalView
	if err := a.cl.call(asWho(ctx, c), c, "POST", "/app/v1/approvals/tasks/"+pathEscape(task)+"/actions", nil, act, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (a approvalsClient) Participates(ctx context.Context, c Caller, subject, user string) (bool, error) {
	var out struct {
		Participates bool `json:"participates"`
	}
	err := a.cl.call(asWho(ctx, c), c, "GET", "/app/v1/approvals/participates", url.Values{"subject": {subject}, "user": {user}}, nil, &out)
	return out.Participates, err
}
