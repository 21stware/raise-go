package raise

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, ro requestOptions) error {
	if c.baseURL == "" {
		return errors.New("raise: set a base URL with WithBaseURL (the /open/v1 root)")
	}
	var payload []byte
	raw, _ := body.(*rawBody)
	if body != nil && raw == nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	if strings.EqualFold(method, http.MethodPost) && !ro.skipIdempotency && ro.idempotencyKey == "" && !isTokenPath(path) {
		ro.idempotencyKey = newUUIDv4()
	}

	var refreshed bool
	attempts := 0
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if raw != nil {
			if err := raw.ready(); err != nil {
				if lastErr != nil {
					return lastErr // a raw body that cannot be sent again: the failure of the one try stands
				}
				return err
			}
		}
		err := c.roundTrip(ctx, method, path, query, payload, raw, out, ro)
		if err == nil {
			return nil
		}
		lastErr = err
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized && apiErr.Code == "invalid_token" && !ro.skipAuth && !refreshed {
			refreshed = true
			c.invalidate(ro.tenant)
			if c.refresh != nil {
				tok, rerr := c.refresh(ctx)
				if rerr != nil {
					return rerr
				}
				c.accessToken = tok
			}
			continue
		}
		if !shouldRetry(method, ro.idempotencyKey, err) {
			return err
		}
		if attempts >= c.maxRetries {
			return err
		}
		wait := retryWait(attempts, apiErr)
		attempts++
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) roundTrip(ctx context.Context, method, path string, query url.Values, payload []byte, raw *rawBody, out any, ro requestOptions) error {
	root := c.baseURL
	if ro.root != "" {
		root = ro.root
	}
	u := root + path
	if encoded := query.Encode(); encoded != "" {
		u += "?" + encoded
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	rawOut, streams := out.(*RawResponse)
	if streams {
		req.Header.Set("Accept", "*/*")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if raw != nil {
		raw.apply(req)
	}
	for k, v := range ro.headers {
		req.Header.Set(k, v)
	}
	if ro.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", ro.idempotencyKey)
	}
	if !ro.skipAuth {
		tok, err := c.token(ctx, ro.tenant)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else {
		req.SetBasicAuth(c.clientID, c.clientSecret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return netErr{err}
	}
	if streams && resp.StatusCode < 400 {
		*rawOut = newRawResponse(resp) // the caller reads and closes the body
		return nil
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return netErr{err}
	}

	if resp.StatusCode >= 400 {
		return decodeAPIError(resp, respBody)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("raise: decode response: %w", err)
	}
	return nil
}

type netErr struct{ err error }

func (e netErr) Error() string { return e.err.Error() }
func (e netErr) Unwrap() error { return e.err }

func decodeAPIError(resp *http.Response, raw []byte) error {
	e := &Error{Status: resp.StatusCode, Details: map[string]any{}}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if sec, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && sec >= 0 {
			e.RetryAfter = time.Duration(sec) * time.Second
			e.hasRetryAfter = true
		}
	}
	e.RequestID = resp.Header.Get("X-Request-Id")
	var wrap struct {
		Error struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			RequestID string         `json:"requestId"`
			DocURL    string         `json:"docUrl"`
			Details   map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &wrap) == nil && wrap.Error.Code != "" {
		e.Code = wrap.Error.Code
		e.Message = wrap.Error.Message
		if wrap.Error.RequestID != "" {
			e.RequestID = wrap.Error.RequestID
		}
		e.DocURL = wrap.Error.DocURL
		if wrap.Error.Details != nil {
			e.Details = wrap.Error.Details
		}
		return e
	}
	e.Code = statusCode(resp.StatusCode)
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(raw))
	}
	if e.Message == "" {
		e.Message = "the platform refused this call; see Code and retry if it is unavailable or rate_limited"
	}
	return e
}

func statusCode(status int) string {
	switch status {
	case 400:
		return "invalid_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 405:
		return "method_not_allowed"
	case 409:
		return "conflict"
	case 413:
		return "payload_too_large"
	case 422:
		return "validation_failed"
	case 429:
		return "rate_limited"
	case 501:
		return "not_implemented"
	case 503:
		return "unavailable"
	default:
		if status >= 500 {
			return "internal"
		}
		return "invalid_request"
	}
}

func shouldRetry(method, idem string, err error) bool {
	var n netErr
	if errors.As(err, &n) {
		if errors.Is(n.err, context.Canceled) || errors.Is(n.err, context.DeadlineExceeded) {
			return false
		}
		return methodIdempotent(method) || idem != ""
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status != http.StatusTooManyRequests && apiErr.Status < 500 {
		return false
	}
	return methodIdempotent(method) || idem != ""
}

func methodIdempotent(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func retryWait(n int, apiErr *Error) time.Duration {
	if apiErr != nil && apiErr.hasRetryAfter {
		return apiErr.RetryAfter
	}
	d := 500 * time.Millisecond * time.Duration(1<<n)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	if d <= 0 {
		return 0
	}
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(d)))
	if err != nil {
		return d
	}
	return d/2 + time.Duration(jitter.Int64())/2
}

func isTokenPath(path string) bool {
	return path == "/oauth/token" || strings.HasSuffix(path, "/oauth/token")
}

func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failed: still produce 16 bytes so a POST can go out; uniqueness is best-effort.
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i % 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
