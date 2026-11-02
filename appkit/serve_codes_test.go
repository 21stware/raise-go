package appkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

// The typed errors on the provider wire: the handover that stopped answers its category as a code next to its words, and a work
// card answers "no_card" for a card it does not know and "not_found" for any other not-found.

type codesApp struct{}

func (*codesApp) ID() string { return "codes" }

func (*codesApp) Manifest(version, base string) *k3.Manifest {
	m := NewManifest("codes", "编码", version, base)
	m.Providers = &k3.ProvidersDecl{Handover: true, Work: true}
	m.Work = &k3.WorkDecl{}
	return m
}

func (*codesApp) Handler(Deps) http.Handler              { return http.NotFoundHandler() }
func (*codesApp) HandoverProvider(Deps) HandoverProvider { return codesHandover{} }
func (*codesApp) WorkProvider(Deps) WorkProvider         { return codesWork{} }
func (*codesApp) Ops(Deps) []Op                          { return nil }
func (*codesApp) Grantables(Deps) map[string]Granter     { return nil }
func (*codesApp) ApprovalAnswers(Deps) ApprovalAnswerer  { return nil }
func (*codesApp) CustomerAsserter(Deps) CustomerAsserter { return nil }
func (*codesApp) Settler(Deps) Settler                   { return nil }

type codesHandover struct{ HandoverProvider }

func (codesHandover) HandOver(_ context.Context, _ Caller, _, to string, kinds []string, _ bool) ([]Responsibility, error) {
	moved := []Responsibility{{Kind: "customers", Count: 3, Movable: true}}
	switch kinds[0] {
	case "invalid":
		return nil, fmt.Errorf("%w: to the same person", ErrInvalid)
	case "forbidden":
		return moved, fmt.Errorf("%w: contracts stay", ErrForbidden)
	case "not_found":
		return moved, fmt.Errorf("%w: no team %s", ErrNotFound, to)
	case "conflict":
		return moved, fmt.Errorf("%w: a transfer is running", ErrConflict)
	case "plain":
		return moved, fmt.Errorf("the table is down")
	}
	return moved, nil
}

type codesWork struct{ WorkProvider }

func (codesWork) WorkCard(_ context.Context, _ Caller, id string, _ WorkQuery) (k3.WorkCardData, error) {
	switch id {
	case "gone":
		return k3.WorkCardData{}, fmt.Errorf("%w: refund 7", ErrNotFound)
	case "nope":
		return k3.WorkCardData{}, ErrNoCard
	}
	return k3.WorkCardData{}, nil
}

func TestServeTypedProviderErrors(t *testing.T) {
	sg, err := k3.NewSigner("codes")
	if err != nil {
		t.Fatal(err)
	}
	h, err := Serve(&codesApp{}, Deps{}, sg.JWKS(), "1.0.0", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		digest, err := k3.InputDigest(json.RawMessage(body))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		tok, err := sg.Sign(k3.TypAssertion, k3.AssertionClaims{Registered: k3.Registered{Iss: "p", Sub: "mia", Aud: k3.Audience{"codes"}, Iat: now.Unix(),
			Exp: now.Add(time.Minute).Unix(), Jti: "j" + path + body}, Tenant: "alpha", Via: k3.ViaProvider, InputDigest: digest})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		req.Header.Set(k3.HeaderAssertion, tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}

	for _, tc := range []struct{ kind, code, words string }{
		{"invalid", "invalid", "to the same person"},
		{"forbidden", "forbidden", "contracts stay"},
		{"not_found", "not_found", "no team mia"},
		{"conflict", "conflict", "a transfer is running"},
		{"plain", "", "the table is down"},
	} {
		status, m := post(PathHandoverMove, `{"from":"leo","to":"mia","kinds":["`+tc.kind+`"]}`)
		code, _ := m["code"].(string)
		words, _ := m["error"].(string)
		if status != 200 || code != tc.code || !strings.Contains(words, tc.words) {
			t.Errorf("handover %s: %d %v (want code %q, words %q)", tc.kind, status, m, tc.code, tc.words)
		}
		if items, _ := m["items"].([]any); len(items) != 1 && tc.kind != "invalid" {
			t.Errorf("handover %s: what moved is still reported: %v", tc.kind, m["items"])
		}
	}
	if status, m := post(PathHandoverMove, `{"from":"leo","to":"mia","kinds":["customers"]}`); status != 200 || m["error"] != nil || m["code"] != nil {
		t.Errorf("a handover that finished: %d %v", status, m)
	}

	for _, tc := range []struct {
		id, code string
		status   int
	}{{"nope", "no_card", 404}, {"gone", "not_found", 404}, {"today", "", 200}} {
		status, m := post(PathWorkCard, `{"id":"`+tc.id+`","query":{}}`)
		code, _ := m["code"].(string)
		if status != tc.status || code != tc.code {
			t.Errorf("card %s: %d %v (want %d code %q)", tc.id, status, m, tc.status, tc.code)
		}
	}
}
