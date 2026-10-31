package raise

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const defaultWebhookTolerance = 5 * time.Minute

// Event is the envelope every subscription kind is sent.
type Event struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	CreatedAt   time.Time      `json:"createdAt"`
	APIVersion  string         `json:"apiVersion"`
	Tenant      string         `json:"tenant,omitempty"`
	Environment string         `json:"environment,omitempty"`
	App         string         `json:"app,omitempty"`
	Data        map[string]any `json:"data"`
}

// VerifyWebhook checks X-Kwork-Signature (t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>)
// with a constant-time compare and rejects timestamps outside tolerance (default 5 minutes when tolerance is 0).
func VerifyWebhook(body []byte, header, secret string, tolerance time.Duration) (*Event, error) {
	if tolerance <= 0 {
		tolerance = defaultWebhookTolerance
	}
	ts, sig, err := parseSignature(header)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if now.Sub(ts) > tolerance || ts.Sub(now) > tolerance {
		return nil, errors.New("raise: webhook timestamp is outside the allowed window; use a fresh delivery or raise the tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return nil, errors.New("raise: webhook signature is not valid")
	}
	if !hmac.Equal(got, want) {
		return nil, errors.New("raise: webhook signature is not valid")
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

func parseSignature(header string) (time.Time, string, error) {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	if ts == "" || sig == "" {
		return time.Time{}, "", errors.New("raise: webhook signature header is missing t or v1")
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, "", errors.New("raise: webhook signature timestamp is not a unix time")
	}
	return time.Unix(unix, 0), sig, nil
}
