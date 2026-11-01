package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/21stware/raise-go/k3"
)

// ApprovalAnswerer is the app's side of the hosted approval protocol (raise-go/k3 approvals.go): what it answers the platform's PREPARE
// call (the facts, title and summary of one object, for the person asking; a refusal is an error with a status below 500, in the
// person's words: an HTTPError, ErrInvalid / ErrConflict / ErrForbidden with words, ErrNotFound) and the DECIDED event (applied once
// per key; a rule of the app that refuses to apply it answers Settled false with a message an administrator reads; an error is
// retried).
type ApprovalAnswerer interface {
	PrepareApproval(ctx context.Context, p k3.ApprovalPrepare) (k3.ApprovalPrepared, error)
	ApprovalDecided(ctx context.Context, key string, e k3.ApprovalEvent) (k3.ApprovalAck, error)
}

// ApprovalAnswering is implemented by an app whose manifest declares approval kinds.
type ApprovalAnswering interface {
	ApprovalAnswers(d Deps) ApprovalAnswerer
}

// serveApprovals answers the platform's two calls of the hosted approval protocol for the kinds the app's manifest declares
// (ADR-0130 P11): PREPARE and DECIDED, at the app's ops address ({opsApi}/approvals/…, /k3/apps/<id> at the app's process). They are
// outside Inbound's check of /k3/ calls, so each checks the platform's assertion itself: via approval, the op, this app as the
// audience, the body's digest, the body's tenant the assertion's. Their patterns are more specific than the ops handler's
// (/k3/apps/<id>/), so it does not take them.
func serveApprovals(mux *http.ServeMux, a App, m *k3.Manifest, d Deps, keys k3.KeySource) {
	an, ok := a.(ApprovalAnswering)
	if !ok || len(m.Approvals) == 0 {
		return
	}
	ans := an.ApprovalAnswers(d)
	if ans == nil {
		return
	}
	base := "/k3/apps/" + a.ID() + "/approvals/"
	verified := func(w http.ResponseWriter, r *http.Request, op string, v any) (Platform, bool) {
		p, ok := verifyCall(r.Context(), r, keys, a.ID(), r.Header.Get(k3.HeaderAssertion), []string{k3.ViaApproval})
		if !ok || p.Op != op {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the platform's assertion is not valid"})
			return p, false
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable"})
			return p, false
		}
		return p, true
	}
	mux.HandleFunc("POST "+base+"prepare", func(w http.ResponseWriter, r *http.Request) {
		var in k3.ApprovalPrepare
		p, ok := verified(w, r, k3.ApprovalPrepareOp, &in)
		if !ok {
			return
		}
		if in.Tenant != p.Tenant {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "another tenant"})
			return
		}
		out, err := ans.PrepareApproval(r.Context(), in)
		if err != nil {
			writeAnswerErr(w, d, "prepare", err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST "+base+"decided", func(w http.ResponseWriter, r *http.Request) {
		var in k3.ApprovalEvent
		p, ok := verified(w, r, k3.ApprovalEventOp, &in)
		if !ok {
			return
		}
		if in.Tenant != p.Tenant {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "another tenant"})
			return
		}
		key := p.Key
		if key == "" {
			key = in.ID + ":" + in.Result
		}
		ack, err := ans.ApprovalDecided(r.Context(), key, in)
		if err != nil {
			writeAnswerErr(w, d, "decided", err)
			return
		}
		writeJSON(w, http.StatusOK, ack)
	})
}

// writeAnswerErr answers an app's refusal with its status and words (the platform hands them to the person, or to an administrator),
// anything else with 500 (retried, for an event).
func writeAnswerErr(w http.ResponseWriter, d Deps, what string, err error) {
	var he *HTTPError
	switch {
	case errors.As(err, &he) && he.Status < 500:
		writeJSON(w, he.Status, map[string]string{"error": he.Message})
	case refusal(err):
		status := http.StatusUnprocessableEntity
		switch {
		case errors.Is(err, ErrConflict):
			status = http.StatusConflict
		case errors.Is(err, ErrForbidden):
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]string{"error": refusedText(err)})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		d.Logger().Warn("appkit: an approval call failed", "call", what, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed"})
	}
}
