package appkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

// ErrModelOff is what a Model answers when the platform has no model for the app (503 model_off). A budget or rate refusal keeps the
// router's status and words (*HTTPError).
var ErrModelOff = errors.New("no model is available")

// ModelImage is a picture of a Read: its type (image/png, image/jpeg, image/webp) and bytes.
type ModelImage struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"` // base64 on the wire
}

// Model is the tenant's service model as an app reaches it (Deps.Model, Client.Model). scene names what the call is for (metered by it).
type Model interface {
	Complete(ctx context.Context, c Caller, scene, system, user string) (string, error)
	// Read answers about pictures.
	Read(ctx context.Context, c Caller, scene, system, user string, images []ModelImage) (string, error)
	// Transcribe turns recorded speech (mime: its content type) into text.
	Transcribe(ctx context.Context, c Caller, audio []byte, mime string) (string, error)
}

// ModelStatus is GET /app/v1/models: whether a model is on offer, whether speech is transcribed, and the service model's name.
type ModelStatus struct {
	Model        bool   `json:"model"`
	Transcribe   bool   `json:"transcribe"`
	ServiceModel string `json:"serviceModel,omitempty"`
}

// ModelClient is the Model over /app/v1/models (ADR-0130 §3.2 H7).
type ModelClient struct{ cl *Client }

var _ Model = (*ModelClient)(nil)

// Model is the tenant's service model over HTTP: Deps.Model = cl.Model().
func (cl *Client) Model() *ModelClient { return &ModelClient{cl: cl} }

type modelReq struct {
	Scene  string       `json:"scene"`
	System string       `json:"system,omitempty"`
	User   string       `json:"user"`
	Images []ModelImage `json:"images,omitempty"`
}

type modelText struct {
	Text string `json:"text"`
}

func modelErr(err error) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusServiceUnavailable && he.Message == ErrModelOff.Error() {
		return ErrModelOff
	}
	return err
}

func (m *ModelClient) Complete(ctx context.Context, c Caller, scene, system, user string) (string, error) {
	var out modelText
	err := m.cl.call(ctx, c, "POST", "/app/v1/models/complete", nil, modelReq{Scene: scene, System: system, User: user}, &out)
	return out.Text, modelErr(err)
}

func (m *ModelClient) Read(ctx context.Context, c Caller, scene, system, user string, images []ModelImage) (string, error) {
	var out modelText
	err := m.cl.call(ctx, c, "POST", "/app/v1/models/read", nil, modelReq{Scene: scene, System: system, User: user, Images: images}, &out)
	return out.Text, modelErr(err)
}

func (m *ModelClient) Transcribe(ctx context.Context, c Caller, audio []byte, mime string) (string, error) {
	resp, err := m.cl.stream(ctx, c, "POST", "/app/v1/models/transcribe", nil, bytes.NewReader(audio), int64(len(audio)), mime)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, responseMax))
	if err := mapStatus(resp.StatusCode, resp.Header, body); err != nil {
		return "", modelErr(err)
	}
	var out modelText
	if err := unmarshalJSON(body, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// Status says whether a model is on offer and whether speech is transcribed (what a page offers).
func (m *ModelClient) Status(ctx context.Context, c Caller) (ModelStatus, error) {
	var st ModelStatus
	err := m.cl.call(ctx, c, "GET", "/app/v1/models", nil, nil, &st)
	return st, err
}
