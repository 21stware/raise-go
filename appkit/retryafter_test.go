package appkit

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// A refusal's Retry-After reaches the app on the HTTPError (a 429 of the platform's rate limits, a 503): delay-seconds and an HTTP
// date both, 0 when the platform sent none or one that cannot be read or is past.
func TestHTTPErrorCarriesRetryAfter(t *testing.T) {
	ctx := context.Background()
	answer := func(code int, retry string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"too many tag changes"}}`))
		}
	}
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for _, c := range []struct {
		name     string
		code     int
		retry    string
		min, max time.Duration
	}{
		{"seconds", 429, "120", 120 * time.Second, 120 * time.Second},
		{"an HTTP date", 429, date, 80 * time.Second, 90 * time.Second},
		{"a 503 with seconds", 503, "7", 7 * time.Second, 7 * time.Second},
		{"none", 429, "", 0, 0},
		{"unreadable", 429, "soon", 0, 0},
		{"a date in the past", 429, "Wed, 21 Oct 2015 07:28:00 GMT", 0, 0},
		{"negative", 429, "-5", 0, 0},
	} {
		cl, _ := platform(t, answer(c.code, c.retry))
		err := cl.CustomerTags().Tag(ctx, Caller{Tenant: "t1"}, "cu_1", "tg_1", true)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != c.code {
			t.Fatalf("%s: %v, want an HTTPError %d", c.name, err, c.code)
		}
		if he.RetryAfter < c.min || he.RetryAfter > c.max {
			t.Errorf("%s: RetryAfter %v, want %v..%v", c.name, he.RetryAfter, c.min, c.max)
		}
	}
}

func TestRetryAfterParse(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"0":                              0,
		" 30 ":                           30 * time.Second,
		"Sun, 01 Nov 2026 10:10:00 GMT":  10 * time.Minute,
		"Sunday, 01-Nov-26 10:00:30 GMT": 30 * time.Second, // RFC 850, also an HTTP date
		"Sun Nov  1 10:00:05 2026":       5 * time.Second,  // asctime
		"99999999999999999999":           0,
		"":                               0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
