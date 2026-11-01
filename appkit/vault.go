package appkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Vault is the platform's secret store as an app uses it (/app/v1/secrets, ADR-0130 P6): what must not sit in the app's database in
// the clear (the recipient of a physical prize) is encrypted under a key per subject; Shred destroys a subject's key for good. The
// subject is the app's own, <kind>:<id> (kind: lower-case letters, digits and -); the platform files it under the calling app and the
// tenant, so an app never reaches another app's or another tenant's keys. Deps.Vault is nil when it is not wired.
type Vault interface {
	Encrypt(ctx context.Context, tenant, subject string, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, tenant, subject string, ciphertext []byte) ([]byte, error)
	Shred(ctx context.Context, tenant, subject string) error
}

// Vault is the platform's secret store over /app/v1/secrets: Deps.Vault = cl.Vault(). Each call is the app's own call in that tenant;
// a ciphertext that does not decrypt (a shredded key, another subject's) is ErrConflict, a subject that is not <kind>:<id> or more than
// one call takes (64 KiB of JSON) is ErrInvalid.
func (cl *Client) Vault() Vault { return vault{cl} }

type vault struct{ cl *Client }

var _ Vault = vault{}

func (v vault) Encrypt(ctx context.Context, tenant, subject string, plaintext []byte) ([]byte, error) {
	var out struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	err := v.cl.tenantCall(ctx, tenant, "POST", "/app/v1/secrets/encrypt", nil, map[string]any{"subject": subject, "plaintext": plaintext}, &out)
	if err != nil {
		return nil, secretErr(err)
	}
	return out.Ciphertext, nil
}

func (v vault) Decrypt(ctx context.Context, tenant, subject string, ciphertext []byte) ([]byte, error) {
	var out struct {
		Plaintext []byte `json:"plaintext"`
	}
	err := v.cl.tenantCall(ctx, tenant, "POST", "/app/v1/secrets/decrypt", nil, map[string]any{"subject": subject, "ciphertext": ciphertext}, &out)
	if err != nil {
		return nil, secretErr(err)
	}
	return out.Plaintext, nil
}

func (v vault) Shred(ctx context.Context, tenant, subject string) error {
	return secretErr(v.cl.tenantCall(ctx, tenant, "POST", "/app/v1/secrets/shred", nil, map[string]string{"subject": subject}, nil))
}

// secretErr reads a body the platform refused as too large (413: the app API takes at most 64 KiB of JSON, about 48 KiB of data) as
// ErrInvalid: the caller asked to encrypt more than one call takes.
func secretErr(err error) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusRequestEntityTooLarge {
		return fmt.Errorf("%w: at most 64 KiB per call", ErrInvalid)
	}
	return err
}

// tenantCall is call as the app's own call in tenant, refusing an answer about another tenant (as Directory and Access do).
func (cl *Client) tenantCall(ctx context.Context, tenant, method, path string, query url.Values, in, out any) error {
	rep, err := cl.do(ctx, Caller{Tenant: tenant}, method, path, query, in)
	if err != nil {
		return err
	}
	if err := mapStatus(rep.status, rep.hdr, rep.body); err != nil {
		return err
	}
	if err := refuseOtherTenant(rep.hdr, tenant); err != nil {
		return err
	}
	if out == nil || len(rep.body) == 0 {
		return nil
	}
	return unmarshalJSON(rep.body, out)
}
