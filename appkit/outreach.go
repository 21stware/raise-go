package appkit

import (
	"context"
	"net/url"
	"time"
)

// OutreachClient is the platform's outreach service as an app reaches it (Deps.OutreachAPI, Client.Outreach): one message to one
// customer by template, and the read side of channels, templates and the message log.
type OutreachClient interface {
	Send(ctx context.Context, c Caller, req OutreachSendReq) (OutreachQueued, error)
	Channels(ctx context.Context, c Caller) ([]OutreachChannel, error)
	Templates(ctx context.Context, c Caller, channel string, withArchived bool) ([]OutreachTemplate, error)
	Template(ctx context.Context, c Caller, id string) (OutreachTemplate, error)
	Message(ctx context.Context, c Caller, id string) (OutreachMessage, error)
}

// OutreachSendReq asks for one message to one customer. Key makes the call safe to repeat: the same key is the same message. Purpose is
// declared by the trigger: marketing needs a granted consent at send time, service does not.
type OutreachSendReq struct {
	Key      string            `json:"key"`
	Customer string            `json:"customer"`
	Channel  string            `json:"channel"`
	Template string            `json:"template"`
	Vars     map[string]string `json:"vars,omitempty"`
	Purpose  string            `json:"purpose"`
	// TriggerRef is what caused it (k3://order/order/o_1): shown in the log, no personal data.
	TriggerRef string `json:"triggerRef,omitempty"`
}

// OutreachQueued is the answer of Send. Existing: the key was queued before (nothing was added).
type OutreachQueued struct {
	ID       string `json:"id"`
	Existing bool   `json:"existing"`
}

// OutreachChannel is a channel as the screen shows it.
type OutreachChannel struct {
	Kind         string               `json:"kind"`
	Name         string               `json:"name"`
	Registered   bool                 `json:"registered"`
	Configured   bool                 `json:"configured"`
	Enabled      bool                 `json:"enabled"`
	Ready        bool                 `json:"ready"` // configured, switched on and (official account) bound
	Provider     string               `json:"provider,omitempty"`
	Signature    string               `json:"signature,omitempty"`
	KeyID        string               `json:"keyId,omitempty"`
	Region       string               `json:"region,omitempty"`
	AppID        string               `json:"appId,omitempty"`
	Secret       OutreachSecretInfo   `json:"secret"`
	CallbackURL  string               `json:"callbackUrl,omitempty"`
	ReplyURL     string               `json:"replyUrl,omitempty"`
	Status       string               `json:"status,omitempty"`
	Note         string               `json:"note,omitempty"`
	Account      string               `json:"account,omitempty"`
	LastTest     *OutreachTestResult  `json:"lastTest,omitempty"`
	UpdatedBy    string               `json:"updatedBy,omitempty"`
	UpdatedAt    *time.Time           `json:"updatedAt,omitempty"`
	Capabilities OutreachCapabilities `json:"capabilities"`
}

// OutreachSecretInfo says whether a secret is set, never what it is.
type OutreachSecretInfo struct {
	Set         bool   `json:"set"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// OutreachTestResult is the outcome of the last test send, in words.
type OutreachTestResult struct {
	OK      bool      `json:"ok"`
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// OutreachCapabilities is what a channel can do.
type OutreachCapabilities struct {
	NeedsTemplate bool   `json:"needsTemplate"`
	Receipts      bool   `json:"receipts"`
	UnsubReply    bool   `json:"unsubReply"`
	Idempotent    bool   `json:"idempotent"`
	Status        bool   `json:"status"`
	Identity      string `json:"identity"` // the customer identity kind the channel sends to (phone, openid)
}

// OutreachField is one field of an official-account template message (name → value pattern).
type OutreachField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// OutreachTemplate is a message template.
type OutreachTemplate struct {
	ID                string          `json:"id"`
	Channel           string          `json:"channel"`
	Name              string          `json:"name"`
	Purpose           string          `json:"purpose"`
	Body              string          `json:"body"`
	Fields            []OutreachField `json:"fields"`
	ParamNames        []string        `json:"paramNames"`
	ChannelTemplateID string          `json:"channelTemplateId"`
	URL               string          `json:"url,omitempty"`
	ApprovalState     string          `json:"approvalState"`
	ApprovalNote      string          `json:"approvalNote,omitempty"`
	Version           int             `json:"version"`
	Active            bool            `json:"active"`
	Variables         []string        `json:"variables"`
	Warnings          []string        `json:"warnings,omitempty"`
	Used              int             `json:"used"` // messages sent with it
	CreatedBy         string          `json:"createdBy,omitempty"`
	UpdatedBy         string          `json:"updatedBy,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

// OutreachMessage is one message of the log. No phone number, no openid, no text: the recipient is the customer id.
type OutreachMessage struct {
	ID              string            `json:"id"`
	Customer        string            `json:"customer"`
	CustomerName    string            `json:"customerName,omitempty"`
	Channel         string            `json:"channel"`
	Template        string            `json:"template"`
	TemplateName    string            `json:"templateName"`
	TemplateVersion int               `json:"templateVersion"`
	Purpose         string            `json:"purpose"`
	TriggerApp      string            `json:"triggerApp,omitempty"`
	TriggerRef      string            `json:"triggerRef,omitempty"`
	Vars            map[string]string `json:"vars"`
	State           string            `json:"state"`
	Reason          string            `json:"reason,omitempty"`
	ReasonText      string            `json:"reasonText,omitempty"`
	Detail          string            `json:"detail,omitempty"`
	Attempts        int               `json:"attempts"`
	ProviderRef     string            `json:"providerRef,omitempty"`
	Cost            int               `json:"cost"`
	DeliverAfter    *time.Time        `json:"deliverAfter,omitempty"`
	SentAt          *time.Time        `json:"sentAt,omitempty"`
	DeliveredAt     *time.Time        `json:"deliveredAt,omitempty"`
	CreatedBy       string            `json:"createdBy,omitempty"`
	CreatedAt       time.Time         `json:"createdAt"`
	UpdatedAt       time.Time         `json:"updatedAt"`
}

// Outreach is the outreach service over /app/v1/outreach (ADR-0130 §3.2 H7): Deps.OutreachAPI = cl.Outreach().
func (cl *Client) Outreach() OutreachClient { return outreachClient{cl} }

type outreachClient struct{ cl *Client }

var _ OutreachClient = outreachClient{}

func (o outreachClient) Send(ctx context.Context, c Caller, req OutreachSendReq) (OutreachQueued, error) {
	var q OutreachQueued
	err := o.cl.call(ctx, c, "POST", "/app/v1/outreach/send", nil, req, &q)
	return q, err
}

func (o outreachClient) Channels(ctx context.Context, c Caller) ([]OutreachChannel, error) {
	var out struct {
		Items []OutreachChannel `json:"items"`
	}
	err := o.cl.call(ctx, c, "GET", "/app/v1/outreach/channels", nil, nil, &out)
	return out.Items, err
}

func (o outreachClient) Templates(ctx context.Context, c Caller, channel string, withArchived bool) ([]OutreachTemplate, error) {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	if withArchived {
		q.Set("archived", "1")
	}
	var out struct {
		Items []OutreachTemplate `json:"items"`
	}
	err := o.cl.call(ctx, c, "GET", "/app/v1/outreach/templates", q, nil, &out)
	return out.Items, err
}

func (o outreachClient) Template(ctx context.Context, c Caller, id string) (OutreachTemplate, error) {
	var t OutreachTemplate
	err := o.cl.call(ctx, c, "GET", "/app/v1/outreach/templates/"+pathEscape(id), nil, nil, &t)
	return t, err
}

func (o outreachClient) Message(ctx context.Context, c Caller, id string) (OutreachMessage, error) {
	var m OutreachMessage
	err := o.cl.call(ctx, c, "GET", "/app/v1/outreach/messages/"+pathEscape(id), nil, nil, &m)
	return m, err
}
