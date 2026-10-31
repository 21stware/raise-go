package raise

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signWebhook(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "."))
	m.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifyWebhookOK(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"app.installed","createdAt":"2026-10-28T00:00:00Z","apiVersion":"2026-10-28","tenant":"acme","environment":"test","app":"shop","data":{"version":"1.0.0","scopes":["platform:ping"]}}`)
	secret := "whsec_test"
	header := signWebhook(secret, time.Now(), body)
	ev, err := VerifyWebhook(body, header, secret, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "app.installed" || ev.ID != "evt_1" || ev.Tenant != "acme" {
		t.Fatalf("%#v", ev)
	}
}

func TestVerifyWebhookBadSignature(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"app.installed","createdAt":"2026-10-28T00:00:00Z","apiVersion":"2026-10-28","data":{}}`)
	header := signWebhook("right", time.Now(), body)
	_, err := VerifyWebhook(body, header, "wrong", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyWebhookStaleTimestamp(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"app.installed","createdAt":"2026-10-28T00:00:00Z","apiVersion":"2026-10-28","data":{}}`)
	header := signWebhook("secret", time.Now().Add(-2*time.Hour), body)
	_, err := VerifyWebhook(body, header, "secret", 5*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("got %v", err)
	}
}
