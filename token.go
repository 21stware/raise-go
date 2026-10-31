package raise

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const tokenRefreshSkew = 60 * time.Second

type tokenState struct {
	mu       sync.Mutex
	cache    map[string]*cachedToken
	inflight map[string]*tokenFlight
}

type cachedToken struct {
	access string
	expiry time.Time
}

type tokenFlight struct {
	done chan struct{}
	tok  *cachedToken
	err  error
}

func newTokenState() *tokenState {
	return &tokenState{cache: map[string]*cachedToken{}, inflight: map[string]*tokenFlight{}}
}

func tenantKey(tenant string) string {
	if tenant == "" {
		return "_"
	}
	return tenant
}

func (c *Client) token(ctx context.Context, tenant string) (string, error) {
	if c.accessToken != "" {
		return c.accessToken, nil
	}
	if tenant == "" {
		tenant = c.tenant
	}
	key := tenantKey(tenant)
	now := c.clock()
	st := c.tok
	st.mu.Lock()
	if tok := st.cache[key]; tok != nil && now.Before(tok.expiry.Add(-tokenRefreshSkew)) {
		access := tok.access
		st.mu.Unlock()
		return access, nil
	}
	if f := st.inflight[key]; f != nil {
		st.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
			if f.err != nil {
				return "", f.err
			}
			return f.tok.access, nil
		}
	}
	f := &tokenFlight{done: make(chan struct{})}
	st.inflight[key] = f
	st.mu.Unlock()

	tok, err := c.exchangeClientCredentials(ctx, tenant)
	st.mu.Lock()
	delete(st.inflight, key)
	f.tok, f.err = tok, err
	if err == nil {
		st.cache[key] = tok
	}
	close(f.done)
	st.mu.Unlock()
	if err != nil {
		return "", err
	}
	return tok.access, nil
}

// Token returns the access token the client sends for tenant ("" for the WithTenant one), exchanging the credential when there is none
// or it ends within 60 seconds. An app's backend sends the same token to the platform's app API (/app/v1), which this SDK does not wrap
// yet (ADR-0130 §2.2).
func (c *Client) Token(ctx context.Context, tenant string) (string, error) {
	return c.token(ctx, tenant)
}

func (c *Client) invalidate(tenant string) {
	if tenant == "" {
		tenant = c.tenant
	}
	st := c.tok
	st.mu.Lock()
	delete(st.cache, tenantKey(tenant))
	st.mu.Unlock()
}

type tokenJSON struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	Tenant      string `json:"tenant"`
	Environment string `json:"environment"`
	Customer    string `json:"customer"`
	Matched     *bool  `json:"matched"`
}

func (c *Client) exchangeClientCredentials(ctx context.Context, tenant string) (*cachedToken, error) {
	body := map[string]any{"grant_type": "client_credentials"}
	if tenant != "" {
		body["tenant"] = tenant
	}
	var out tokenJSON
	if err := c.postToken(ctx, body, &out); err != nil {
		if IsCode(err, "tenant_required") {
			return nil, fmt.Errorf("raise: this credential needs WithTenant (an app-level key is not bound to one organisation): %w", err)
		}
		return nil, err
	}
	return c.cachedFrom(out), nil
}

func (c *Client) fetchCustomerToken(ctx context.Context, phone, customerID string) (string, error) {
	body := map[string]any{
		"grant_type": "urn:raise:grant-type:customer",
		"phone":      phone,
	}
	if c.tenant != "" {
		body["tenant"] = c.tenant
	}
	if customerID != "" {
		body["customer_id"] = customerID
	}
	var out tokenJSON
	if err := c.postToken(ctx, body, &out); err != nil {
		return "", err
	}
	if out.Matched != nil && !*out.Matched {
		return "", ErrNoCustomer
	}
	if out.AccessToken == "" {
		return "", ErrNoCustomer
	}
	return out.AccessToken, nil
}

func (c *Client) cachedFrom(out tokenJSON) *cachedToken {
	exp := out.ExpiresIn
	if exp <= 0 {
		exp = 300
	}
	return &cachedToken{access: out.AccessToken, expiry: c.clock().Add(time.Duration(exp) * time.Second)}
}

func (c *Client) postToken(ctx context.Context, body, out any) error {
	return c.do(ctx, "POST", "/oauth/token", nil, body, out, requestOptions{skipAuth: true, skipIdempotency: true})
}
