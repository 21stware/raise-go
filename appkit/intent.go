package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// The platform's sentence parser (ADR-0086 §3) for an app on its own: the L2 input of the app's pages (IntentInput in
// @21stware/raise-app-kit) posts one sentence (and a photo) for one of the app's ops to IntentPath on the app's own origin; the app
// hands it to the platform's POST /app/v1/intent/parse as the person, and the page gets the platform's answer as it is: a proposal for
// the op's input, never a write. The platform's firstparty.MountIntent, over HTTP.

// IntentPath is where an app's pages ask for a parse (the platform's firstparty.IntentPath).
const IntentPath = "/api/intent/parse"

// intentBodyMax bounds a forwarded request: three images of 5 MiB, base64 encoded, and the rest (the platform's intent.MaxBody).
const intentBodyMax = 3*(5<<20)*4/3 + 64<<10

// IntentRequest is one sentence for one of the app's ops (App may stay empty: the platform fills in the calling app).
type IntentRequest struct {
	App         string             `json:"app,omitempty"`
	Op          string             `json:"op"`
	Text        string             `json:"text,omitempty"`
	Attachments []IntentAttachment `json:"attachments,omitempty"`
	Context     *IntentContext     `json:"context,omitempty"`
}

// IntentAttachment is an image sent with the sentence: its bytes base64 encoded, or a data: URL.
type IntentAttachment struct {
	Name string `json:"name,omitempty"`
	Mime string `json:"mime,omitempty"`
	Data string `json:"data"`
}

// IntentContext is what the page knows: the records it is about (k3:// references), the person's time and time zone.
type IntentContext struct {
	Refs []string `json:"refs,omitempty"`
	Now  string   `json:"now,omitempty"` // RFC 3339
	TZ   string   `json:"tz,omitempty"`  // IANA name; the platform's default is Asia/Shanghai
}

// IntentResult is the proposal: the op's input as far as it could be told, where each value came from, what is still missing.
type IntentResult struct {
	Input    map[string]any         `json:"input"`
	Fields   map[string]IntentField `json:"fields"`
	Missing  []string               `json:"missing"`
	Rejected []IntentRejection      `json:"rejected,omitempty"`
	By       string                 `json:"by"` // rules (nothing charged) or model
	Usage    IntentUsage            `json:"usage"`
}

// IntentField says where one value came from.
type IntentField struct {
	Source     string            `json:"source"` // stated | inferred
	Note       string            `json:"note,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Label      string            `json:"label,omitempty"`
	Candidates []IntentCandidate `json:"candidates,omitempty"`
}

// IntentCandidate is another record a name matched.
type IntentCandidate struct {
	Value string `json:"value"`
	Ref   string `json:"ref"`
	Label string `json:"label"`
}

// IntentRejection is a value that was found but does not fit the op.
type IntentRejection struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// IntentUsage is what the parse cost: billed micros and points (1 point = 10,000 micros).
type IntentUsage struct {
	Points float64 `json:"points"`
	Micros int64   `json:"micros"`
}

// IntentClient is the platform's sentence parser over /app/v1/intent/parse (scope app.intent:parse). Only a page session's person
// has a sentence parsed: the app's own caller is refused (403).
type IntentClient struct{ cl *Client }

// Intent is the platform's sentence parser over HTTP: Deps.IntentAPI = cl.Intent().Forward.
func (cl *Client) Intent() *IntentClient { return &IntentClient{cl: cl} }

// Parse asks for a proposal for c, the person of a page session.
func (ic *IntentClient) Parse(ctx context.Context, c Caller, req IntentRequest) (IntentResult, error) {
	var out IntentResult
	err := ic.cl.call(ctx, c, "POST", "/app/v1/intent/parse", nil, req, &out)
	return out, err
}

// Forward hands a page's request body to the platform as it is and answers the platform's status and body as they are (a refusal is
// the platform's answer, with its code and problems, not an error): what MountIntent serves.
func (ic *IntentClient) Forward(ctx context.Context, c Caller, body []byte) (int, []byte, error) {
	if !json.Valid(body) {
		return 0, nil, errors.Join(ErrInvalid, errors.New("the body is not JSON"))
	}
	rep, err := ic.cl.do(ctx, c, "POST", "/app/v1/intent/parse", nil, json.RawMessage(body))
	if err != nil {
		return 0, nil, err
	}
	return rep.status, rep.body, nil
}

// MountIntent serves POST /api/intent/parse on an app's router for its pages: the caller the gateway verified asks for a proposal for
// one of this app's ops, through d.IntentAPI (503 when it is not wired). Nothing is written.
func MountIntent(r Router, d Deps) {
	r.HandleFunc("POST "+IntentPath, Auth(func(w http.ResponseWriter, req *http.Request, c Caller) {
		if d.IntentAPI == nil {
			JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "intent parsing is not available", "code": "intent_off"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, intentBodyMax))
		if err != nil {
			JSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the request is too large (at most 3 images of 5 MiB)", "code": "too_large"})
			return
		}
		if !json.Valid(body) {
			JSON(w, http.StatusBadRequest, map[string]string{"error": "the body is not a JSON request", "code": "invalid_request"})
			return
		}
		status, out, err := d.IntentAPI(req.Context(), c, body)
		if err != nil {
			Fail(w, d.Logger(), err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}))
}
