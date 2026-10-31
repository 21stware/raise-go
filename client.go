package raise

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Client talks to Raise Open API v1 as an application (tenant token).
type Client struct {
	resources
	// App is the app API (/app/v1): what an installed app asks of the platform. It uses the same credential and tokens; its root is
	// derived from the /open/v1 base URL (WithBaseURL).
	App          *AppAPI
	clientID     string
	clientSecret string
	baseURL      string
	tenant       string
	http         *http.Client
	maxRetries   int
	userAgent    string
	now          func() time.Time
	tok          *tokenState
	accessToken  string // set on a child client that already holds a token (customer)
	refresh      func(ctx context.Context) (string, error)
}

// Option configures New.
type Option func(*Client)

// WithBaseURL sets the /open/v1 root, for example https://api.example.com/open/v1.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithTenant sets the organisation id. Required for an app-level (store) credential.
func WithTenant(id string) Option {
	return func(c *Client) { c.tenant = id }
}

// WithHTTPClient replaces the HTTP client used for every call.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithMaxRetries sets how many times a 429, 5xx or network error is retried (default 3).
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithUserAgent replaces the default User-Agent raise-go/<Version>.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// New builds a client. It does not talk to the network until the first call.
func New(clientID, clientSecret string, opts ...Option) *Client {
	c := &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         http.DefaultClient,
		maxRetries:   3,
		userAgent:    "raise-go/" + Version,
		now:          time.Now,
		tok:          newTokenState(),
	}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.initResources()
	c.App = newAppAPI(c)
	return c
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Client) child() *Client {
	n := &Client{
		clientID:     c.clientID,
		clientSecret: c.clientSecret,
		baseURL:      c.baseURL,
		tenant:       c.tenant,
		http:         c.http,
		maxRetries:   c.maxRetries,
		userAgent:    c.userAgent,
		now:          c.now,
		tok:          c.tok,
	}
	n.initResources()
	n.App = newAppAPI(n)
	return n
}

// CustomerClient talks as one asserted customer (/me… only).
type CustomerClient struct {
	customerResources
	c *Client
}

// CustomerOption configures ForCustomer.
type CustomerOption func(*customerOpts)

type customerOpts struct {
	customerID string
}

// WithCustomerID picks one of the candidates after 409 ambiguous_customer.
func WithCustomerID(id string) CustomerOption {
	return func(o *customerOpts) { o.customerID = id }
}

// ForCustomer exchanges a customer token for this phone and returns a client that can call /me….
// ErrNoCustomer when nobody matches. 409 ambiguous_customer is returned as *Error with details.candidates.
func (c *Client) ForCustomer(ctx context.Context, phone string, opts ...CustomerOption) (*CustomerClient, error) {
	var o customerOpts
	for _, fn := range opts {
		fn(&o)
	}
	tok, err := c.fetchCustomerToken(ctx, phone, o.customerID)
	if err != nil {
		return nil, err
	}
	inner := c.child()
	inner.accessToken = tok
	inner.refresh = func(ctx context.Context) (string, error) {
		return c.fetchCustomerToken(ctx, phone, o.customerID)
	}
	cc := &CustomerClient{c: inner}
	cc.initResources()
	return cc, nil
}

// CallOption is passed to generated POST methods.
type CallOption func(*requestOptions)

type requestOptions struct {
	root            string // replaces the client's base URL for this call (the app API's /app/v1 root)
	idempotencyKey  string
	tenant          string
	skipAuth        bool
	skipIdempotency bool
}

// WithIdempotencyKey sets the Idempotency-Key for one POST. The SDK generates a uuid v4 when this is omitted.
func WithIdempotencyKey(k string) CallOption {
	return func(o *requestOptions) { o.idempotencyKey = k }
}
