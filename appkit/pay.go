package appkit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The payment service's refusals, as the platform's payment service names them (its payments.ErrPending …): an answer of /app/v1/pay
// that stands for one matches it with errors.Is, and still matches the kit's own error (ErrConflict, an *HTTPError of status 503 …).
var (
	// ErrPayPending: the gateway did not answer; ask again with the same key in a moment (the order may exist).
	ErrPayPending = errors.New("payments: the gateway did not answer; try again in a moment")
	// ErrPayNotConfigured: collecting money is not set up for this channel.
	ErrPayNotConfigured = errors.New("payments: this payment channel is not set up")
	// ErrPayDisabled: the channel is switched off.
	ErrPayDisabled = errors.New("payments: this payment channel is switched off")
)

// PayGatewayError is the gateway's refusal in its own words (an answer of status 502).
type PayGatewayError struct{ Message string }

func (e *PayGatewayError) Error() string { return "payments: the gateway refused: " + e.Message }

// payErr is the platform's answer to a payment call with the payment refusal it stands for.
type payErr struct{ err, sent error }

func (e *payErr) Error() string   { return e.err.Error() }
func (e *payErr) Unwrap() []error { return []error{e.err, e.sent} }

// payBack maps an answer of /app/v1/pay onto the payment refusals (the platform's words say which one; internal/remoteapp's payBack
// reads them the same way).
func payBack(err error) error {
	if err == nil {
		return nil
	}
	var he *HTTPError
	status, msg := 0, err.Error()
	if errors.As(err, &he) {
		status, msg = he.Status, he.Message
	} else if errors.Is(err, ErrConflict) {
		status, msg = http.StatusConflict, strings.TrimPrefix(strings.TrimPrefix(msg, ErrConflict.Error()), ": ")
	}
	var sent error
	switch {
	case status == http.StatusBadGateway && strings.HasPrefix(msg, "the payment gateway refused: "):
		sent = &PayGatewayError{Message: strings.TrimPrefix(msg, "the payment gateway refused: ")}
	case status == http.StatusServiceUnavailable && strings.HasPrefix(msg, "the payment gateway did not answer"):
		sent = ErrPayPending
	case status == http.StatusConflict && strings.HasPrefix(msg, "collecting money is not set up"):
		sent = ErrPayNotConfigured
	case status == http.StatusConflict && msg == "this payment channel is switched off":
		sent = ErrPayDisabled
	default:
		return err
	}
	return &payErr{err: err, sent: sent}
}

// PayOrder is an order as the platform answers it (the JSON of its payment order), with the customer's payment page (Link) and, for a
// JSAPI trade, the signed parameters of the bridge the app's page hands to WeChat (PayParams, JSON).
type PayOrder struct {
	Tenant         string     `json:"tenant"`
	ID             string     `json:"id"`
	Channel        string     `json:"channel"`
	Product        string     `json:"product"`
	OutTradeNo     string     `json:"outTradeNo"`
	AmountCents    int64      `json:"amountCents"`
	Currency       string     `json:"currency"`
	Subject        string     `json:"subject"`
	Status         string     `json:"status"`
	SourceApp      string     `json:"sourceApp,omitempty"`
	SourceRef      string     `json:"sourceRef,omitempty"`
	PayToken       string     `json:"payToken"`
	CodeURL        string     `json:"codeUrl,omitempty"`
	PayURL         string     `json:"payUrl,omitempty"`
	AppID          string     `json:"appId,omitempty"`
	ChannelTradeNo string     `json:"channelTradeNo,omitempty"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	PaidAt         *time.Time `json:"paidAt,omitempty"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
	RefundedCents  int64      `json:"refundedCents"`
	CreatedBy      string     `json:"createdBy,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	SettledAt      *time.Time `json:"settledAt,omitempty"`
	SettleAttempts int        `json:"settleAttempts"`
	LastError      string     `json:"lastError,omitempty"`
	Link           string     `json:"link,omitempty"`
	PayParams      string     `json:"payParams,omitempty"`
}

// PayRefund is money given back.
type PayRefund struct {
	Tenant          string     `json:"tenant"`
	ID              string     `json:"id"`
	OrderID         string     `json:"orderId"`
	OutRefundNo     string     `json:"outRefundNo"`
	AmountCents     int64      `json:"amountCents"`
	Reason          string     `json:"reason,omitempty"`
	Status          string     `json:"status"`
	ChannelRefundID string     `json:"channelRefundId,omitempty"`
	CreatedBy       string     `json:"createdBy,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	SettledAt       *time.Time `json:"settledAt,omitempty"`
	SettleAttempts  int        `json:"settleAttempts"`
	LastError       string     `json:"lastError,omitempty"`
}

// PayCreateReq asks for a payment. The platform names the app itself (the token's).
type PayCreateReq struct {
	Channel     string
	Product     string // "" = the channel's first product
	AmountCents int64
	Subject     string
	SourceRef   string // a k3:// reference to what is paid for
	IdemKey     string // the same business action asks once; the same key returns the first order
	TTL         time.Duration
	ReturnURL   string // alipay page pay and wechat h5: where the browser goes back to
	OpenID      string // wechat jsapi: the payer's openid
	ClientIP    string // wechat h5: the payer's address
}

// PayRefundReq asks for money back.
type PayRefundReq struct {
	AmountCents int64 // 0 = everything that is still refundable
	Reason      string
	IdemKey     string
}

// Pay is the payment service as an app reaches it (Deps.Pay, Client.Pay).
type Pay interface {
	Create(ctx context.Context, c Caller, req PayCreateReq) (PayOrder, error)
	Get(ctx context.Context, c Caller, id string) (PayOrder, error)
	Cancel(ctx context.Context, c Caller, id string) (PayOrder, error)
	Refund(ctx context.Context, c Caller, orderID string, req PayRefundReq) (PayRefund, error)
	Refunds(ctx context.Context, c Caller, orderID string) ([]PayRefund, error)
	// Link is the customer's payment page of an order this client has seen ("" when not known).
	Link(o PayOrder) string
}

// Pay is the payment service over /app/v1/pay (ADR-0130 §3.2 H7): Deps.Pay = cl.Pay(). The platform answers each order with its
// payment page; Link gives it back for an order this client has seen (an order read from the app's own records has to be read again
// first: Get).
func (cl *Client) Pay() Pay { return &payClient{cl: cl} }

type payClient struct {
	cl    *Client
	mu    sync.Mutex
	links map[string]string // pay token -> the customer's payment page, of the orders seen
}

var _ Pay = (*payClient)(nil)

const maxPayLinks = 10000

func (p *payClient) seen(o PayOrder) PayOrder {
	if o.Link != "" && o.PayToken != "" {
		p.mu.Lock()
		if p.links == nil || len(p.links) >= maxPayLinks {
			p.links = map[string]string{}
		}
		p.links[o.PayToken] = o.Link
		p.mu.Unlock()
	}
	return o
}

func (p *payClient) order(ctx context.Context, c Caller, method, path string, in any) (PayOrder, error) {
	var o PayOrder
	if err := p.cl.call(ctx, c, method, path, nil, in, &o); err != nil {
		return PayOrder{}, payBack(err)
	}
	return p.seen(o), nil
}

func (p *payClient) Create(ctx context.Context, c Caller, req PayCreateReq) (PayOrder, error) {
	body := struct {
		Channel     string `json:"channel"`
		Product     string `json:"product,omitempty"`
		AmountCents int64  `json:"amountCents"`
		Subject     string `json:"subject"`
		SourceRef   string `json:"sourceRef,omitempty"`
		Key         string `json:"key"`
		TTLSeconds  int    `json:"ttlSeconds,omitempty"`
		ReturnURL   string `json:"returnUrl,omitempty"`
		OpenID      string `json:"openId,omitempty"`
		ClientIP    string `json:"clientIp,omitempty"`
	}{req.Channel, req.Product, req.AmountCents, req.Subject, req.SourceRef, req.IdemKey, int(req.TTL / time.Second), req.ReturnURL, req.OpenID, req.ClientIP}
	return p.order(ctx, c, "POST", "/app/v1/pay/orders", body)
}

func (p *payClient) Get(ctx context.Context, c Caller, id string) (PayOrder, error) {
	return p.order(ctx, c, "GET", "/app/v1/pay/orders/"+pathEscape(id), nil)
}

func (p *payClient) Cancel(ctx context.Context, c Caller, id string) (PayOrder, error) {
	return p.order(ctx, c, "POST", "/app/v1/pay/orders/"+pathEscape(id)+"/cancel", struct{}{})
}

func (p *payClient) Refund(ctx context.Context, c Caller, orderID string, req PayRefundReq) (PayRefund, error) {
	body := struct {
		AmountCents int64  `json:"amountCents,omitempty"`
		Reason      string `json:"reason,omitempty"`
		Key         string `json:"key"`
	}{req.AmountCents, req.Reason, req.IdemKey}
	var r PayRefund
	err := p.cl.call(ctx, c, "POST", "/app/v1/pay/orders/"+pathEscape(orderID)+"/refunds", nil, body, &r)
	return r, payBack(err)
}

func (p *payClient) Refunds(ctx context.Context, c Caller, orderID string) ([]PayRefund, error) {
	var out struct {
		Items []PayRefund `json:"items"`
	}
	err := p.cl.call(ctx, c, "GET", "/app/v1/pay/orders/"+pathEscape(orderID)+"/refunds", nil, nil, &out)
	return out.Items, payBack(err)
}

func (p *payClient) Link(o PayOrder) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.links[o.PayToken]
}
