package appkit

import (
	"context"
	"sync"
	"time"
)

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
		return PayOrder{}, err
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
	return r, err
}

func (p *payClient) Refunds(ctx context.Context, c Caller, orderID string) ([]PayRefund, error) {
	var out struct {
		Items []PayRefund `json:"items"`
	}
	err := p.cl.call(ctx, c, "GET", "/app/v1/pay/orders/"+pathEscape(orderID)+"/refunds", nil, nil, &out)
	return out.Items, err
}

func (p *payClient) Link(o PayOrder) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.links[o.PayToken]
}
