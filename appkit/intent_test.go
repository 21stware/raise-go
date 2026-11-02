package appkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntentParseAndForward(t *testing.T) {
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input":{"when":"2026-11-03"},"fields":{"when":{"source":"stated"}},"missing":["who"],"by":"rules","usage":{"points":0,"micros":0}}`))
	})
	res, err := cl.Intent().Parse(context.Background(), tc, IntentRequest{Op: "visit.create", Text: "明天", Context: &IntentContext{TZ: "Asia/Shanghai"}})
	if err != nil || res.By != "rules" || res.Input["when"] != "2026-11-03" || res.Fields["when"].Source != "stated" || len(res.Missing) != 1 {
		t.Fatalf("parse: %+v %v", res, err)
	}
	if s.method != "POST" || s.path != "/app/v1/intent/parse" || s.tenant != "t1" || s.body != `{"op":"visit.create","text":"明天","context":{"tz":"Asia/Shanghai"}}` {
		t.Fatalf("parse request: %+v", *s)
	}

	// a refusal is the platform's answer as it is when forwarded, an error when parsed
	cl, _ = platform(t, status(422, `{"error":"op: the name of one of the App's ops","code":"invalid_request","problems":[{"path":"op","message":"x"}]}`))
	code, body, err := cl.Intent().Forward(context.Background(), tc, []byte(`{"op":""}`))
	if err != nil || code != 422 || !strings.Contains(string(body), `"problems"`) {
		t.Fatalf("forward: %d %s %v", code, body, err)
	}
	if _, err := cl.Intent().Parse(context.Background(), tc, IntentRequest{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("parse refused: %v", err)
	}
	if _, _, err := cl.Intent().Forward(context.Background(), tc, []byte(`not json`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forward of a body that is not JSON: %v", err)
	}
}

func TestMountIntent(t *testing.T) {
	ask := func(d Deps, agent string, body string) *httptest.ResponseRecorder {
		r := NewRouter()
		MountIntent(r, d)
		req := httptest.NewRequest("POST", IntentPath, strings.NewReader(body))
		req.Header.Set(HeaderTenant, "t1")
		req.Header.Set(HeaderUser, "u1")
		if agent != "" {
			req.Header.Set(HeaderAgent, agent)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := ask(Deps{}, "", `{"op":"a"}`); w.Code != 503 || !strings.Contains(w.Body.String(), "intent_off") {
		t.Fatalf("not wired: %d %s", w.Code, w.Body)
	}
	var got Caller
	var sent string
	d := Deps{IntentAPI: func(_ context.Context, c Caller, body []byte) (int, []byte, error) {
		got, sent = c, string(body)
		return 429, []byte(`{"error":"too many requests; wait a minute","code":"rate_limited"}`), nil
	}}
	w := ask(d, "", `{"op":"visit.create","text":"明天"}`)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "rate_limited") || got.User != "u1" || got.Tenant != "t1" || sent != `{"op":"visit.create","text":"明天"}` {
		t.Fatalf("forwarded: %d %s %+v %s", w.Code, w.Body, got, sent)
	}
	if w := ask(d, "", `{"op":`); w.Code != 400 {
		t.Fatalf("not JSON: %d", w.Code)
	}
	if w := ask(d, "agent-1", `{"op":"a"}`); w.Code != 405 {
		t.Fatalf("the agent only reads: %d", w.Code)
	}
}
