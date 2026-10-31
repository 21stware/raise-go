package raise

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func envelope(code, msg, reqID string, details map[string]any) []byte {
	if details == nil {
		details = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "requestId": reqID, "docUrl": "https://dev.example.com/docs/errors#" + code, "details": details},
	})
	return b
}

type fakePlatform struct {
	mu sync.Mutex

	tokenCalls atomic.Int32
	whoCalls   atomic.Int32
	postCalls  atomic.Int32

	tokenSleep time.Duration
	tokenOnce  sync.Once
	issuedAt   time.Time
	expiresIn  int
	clock      func() time.Time

	whoStatus []int
	whoN      int

	postStatus []int
	postN      int
	postKeys   []string

	rowPages map[string]Page[Row]

	customer func(body map[string]any) (int, []byte)
}

func (f *fakePlatform) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", f.token)
	mux.HandleFunc("/whoami", f.whoami)
	mux.HandleFunc("/tables/{table}/views/{view}/rows", f.rows)
	mux.HandleFunc("/me/bookings", f.postBookings)
	return mux
}

func (f *fakePlatform) token(w http.ResponseWriter, r *http.Request) {
	f.tokenCalls.Add(1)
	if f.tokenSleep > 0 {
		time.Sleep(f.tokenSleep)
	}
	id, sec, ok := r.BasicAuth()
	if !ok || id == "" || sec == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="kwork open platform"`)
		w.Header().Set("X-Request-Id", "tok-401")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(envelope("invalid_client", "the client id or secret is not valid", "tok-401", nil))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	grant, _ := body["grant_type"].(string)
	if grant == "urn:raise:grant-type:customer" {
		if f.customer != nil {
			status, raw := f.customer(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(raw)
			return
		}
	}
	if grant == "client_credentials" {
		if _, has := body["tenant"]; !has && id == "app-level" {
			w.Header().Set("X-Request-Id", "tok-400")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(envelope("tenant_required", "this app-level credential needs tenant", "tok-400", nil))
			return
		}
	}
	exp := f.expiresIn
	if exp == 0 {
		exp = 300
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "tok-" + strconv.Itoa(int(f.tokenCalls.Load())),
		"token_type":   "Bearer",
		"expires_in":   exp,
		"scope":        "platform:ping tables.base:read",
		"tenant":       "acme",
		"environment":  "test",
	})
}

func (f *fakePlatform) whoami(w http.ResponseWriter, r *http.Request) {
	n := int(f.whoCalls.Add(1))
	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(envelope("token_required", "an integration token is required", "who-401", nil))
		return
	}
	f.mu.Lock()
	st := 200
	if f.whoN < len(f.whoStatus) {
		st = f.whoStatus[f.whoN]
		f.whoN++
	}
	f.mu.Unlock()
	w.Header().Set("X-Request-Id", "who-"+strconv.Itoa(n))
	if st == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(st)
		_, _ = w.Write(envelope("rate_limited", "slow down", "who-"+strconv.Itoa(n), nil))
		return
	}
	if st == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(st)
		_, _ = w.Write(envelope("unavailable", "retry", "who-"+strconv.Itoa(n), nil))
		return
	}
	if st == http.StatusBadRequest {
		w.WriteHeader(st)
		_, _ = w.Write(envelope("invalid_request", "bad query", "who-"+strconv.Itoa(n), nil))
		return
	}
	if st == http.StatusUnprocessableEntity {
		w.WriteHeader(st)
		_, _ = w.Write(envelope("validation_failed", "bad field", "who-"+strconv.Itoa(n), nil))
		return
	}
	if st == http.StatusUnauthorized {
		w.WriteHeader(st)
		_, _ = w.Write(envelope("invalid_token", "expired", "who-"+strconv.Itoa(n), nil))
		return
	}
	if st >= 400 {
		w.WriteHeader(st)
		_, _ = w.Write(envelope("internal", "fail", "who-"+strconv.Itoa(n), nil))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tenant": "acme", "app": "shop", "credential": "cred-1", "scopes": []string{"platform:ping"},
	})
}

func (f *fakePlatform) rows(w http.ResponseWriter, r *http.Request) {
	cur := r.URL.Query().Get("cursor")
	page, ok := f.rowPages[cur]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(envelope("not_found", "no page", "row-404", nil))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

func (f *fakePlatform) postBookings(w http.ResponseWriter, r *http.Request) {
	n := int(f.postCalls.Add(1))
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	f.postKeys = append(f.postKeys, key)
	st := 201
	if f.postN < len(f.postStatus) {
		st = f.postStatus[f.postN]
		f.postN++
	}
	f.mu.Unlock()
	w.Header().Set("X-Request-Id", "post-"+strconv.Itoa(n))
	if st == http.StatusServiceUnavailable || st == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(st)
		_, _ = w.Write(envelope("unavailable", "retry", "post-"+strconv.Itoa(n), nil))
		return
	}
	if st >= 400 {
		w.WriteHeader(st)
		_, _ = w.Write(envelope("invalid_request", "no", "post-"+strconv.Itoa(n), nil))
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "b1", "memberId": "m1", "sessionId": "s1", "courseName": "yoga", "startsAt": time.Now().Format(time.RFC3339), "endsAt": time.Now().Format(time.RFC3339), "room": "", "status": "booked", "cancelCharge": 0})
}

func newTestClient(t *testing.T, f *fakePlatform, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	all := append([]Option{WithBaseURL(srv.URL), WithHTTPClient(srv.Client())}, opts...)
	return New("cid", "csecret", all...)
}

func TestTokenCachedAndRefreshedEarly(t *testing.T) {
	f := &fakePlatform{expiresIn: 300}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := newTestClient(t, f)
	c.now = func() time.Time { return now }

	if _, err := c.Platform.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Platform.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token fetched %d times, want 1", n)
	}

	now = now.Add(241 * time.Second) // 60s before 300s expiry
	if _, err := c.Platform.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenCalls.Load(); n != 2 {
		t.Fatalf("token fetched %d times after early refresh, want 2", n)
	}
}

func TestTokenSingleFlight(t *testing.T) {
	f := &fakePlatform{tokenSleep: 80 * time.Millisecond, expiresIn: 300}
	c := newTestClient(t, f)
	ctx := context.Background()
	var wg sync.WaitGroup
	errc := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Platform.Whoami(ctx)
			errc <- err
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token fetched %d times under 20 goroutines, want 1", n)
	}
}

func TestRetry429And503(t *testing.T) {
	f := &fakePlatform{whoStatus: []int{429, 200}}
	c := newTestClient(t, f)
	if _, err := c.Platform.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.whoCalls.Load(); n != 2 {
		t.Fatalf("whoami calls %d, want 2 (one 429 then success)", n)
	}

	f2 := &fakePlatform{whoStatus: []int{503, 200}}
	c2 := newTestClient(t, f2)
	if _, err := c2.Platform.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f2.whoCalls.Load(); n != 2 {
		t.Fatalf("whoami calls %d, want 2 (one 503 then success)", n)
	}
}

func TestNoRetryOn400And422(t *testing.T) {
	f := &fakePlatform{whoStatus: []int{400, 200}}
	c := newTestClient(t, f)
	err := mustErr(t, func() error { _, err := c.Platform.Whoami(context.Background()); return err })
	if !IsCode(err, "invalid_request") {
		t.Fatalf("got %v", err)
	}
	if n := f.whoCalls.Load(); n != 1 {
		t.Fatalf("whoami calls %d, want 1 (no retry on 400)", n)
	}

	f2 := &fakePlatform{whoStatus: []int{422, 200}}
	c2 := newTestClient(t, f2)
	err = mustErr(t, func() error { _, err := c2.Platform.Whoami(context.Background()); return err })
	if !IsCode(err, "validation_failed") {
		t.Fatalf("got %v", err)
	}
	if n := f2.whoCalls.Load(); n != 1 {
		t.Fatalf("whoami calls %d, want 1 (no retry on 422)", n)
	}
}

func TestPOSTIdempotencyKeyKeptAcrossRetries(t *testing.T) {
	f := &fakePlatform{postStatus: []int{503, 201}}
	c := newTestClient(t, f)
	var out MeBooking
	err := c.do(context.Background(), "POST", "/me/bookings", nil, map[string]string{"memberId": "m", "sessionId": "s"}, &out, requestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f.postCalls.Load() != 2 {
		t.Fatalf("POST calls %d, want 2", f.postCalls.Load())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.postKeys) != 2 || f.postKeys[0] == "" || f.postKeys[0] != f.postKeys[1] {
		t.Fatalf("Idempotency-Key not reused: %v", f.postKeys)
	}
}

func TestErrorDecoding(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			f.token(w, r)
			return
		}
		w.Header().Set("X-Request-Id", "req-9f2c")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(envelope("scope_required", "this call needs the scope platform:ping", "req-9f2c", map[string]any{"scope": "platform:ping"}))
	}))
	t.Cleanup(srv.Close)
	c := New("cid", "sec", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.Platform.Whoami(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("got %T %v", err, err)
	}
	if e.Code != "scope_required" || e.RequestID != "req-9f2c" || e.DocURL == "" || e.Details["scope"] != "platform:ping" {
		t.Fatalf("decoded %#v", e)
	}
	if e.RetryAfter != 7*time.Second {
		t.Fatalf("Retry-After %v", e.RetryAfter)
	}
	if !IsCode(err, "scope_required") {
		t.Fatal("IsCode")
	}
}

func TestPaginationThreePages(t *testing.T) {
	c1, c2 := "c1", "c2"
	f := &fakePlatform{rowPages: map[string]Page[Row]{
		"":   {Data: []Row{{Id: "a", Version: 1, Fields: map[string]any{}}}, NextCursor: &c1},
		"c1": {Data: []Row{{Id: "b", Version: 1, Fields: map[string]any{}}}, NextCursor: &c2},
		"c2": {Data: []Row{{Id: "c", Version: 1, Fields: map[string]any{}}}, NextCursor: nil},
	}}
	client := newTestClient(t, f)
	it := client.Tables.ListRowsAll(context.Background(), "t", "v", nil)
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Item().Id)
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if got := ids; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("ids %v", got)
	}
}

func TestForCustomerMatched(t *testing.T) {
	f := &fakePlatform{}
	f.customer = func(in map[string]any) (int, []byte) {
		if in["phone"] != "13900001111" {
			t.Errorf("phone %v", in["phone"])
		}
		raw, _ := json.Marshal(map[string]any{
			"access_token": "cus-tok", "token_type": "Bearer", "expires_in": 900,
			"scope": "membership.me:read", "tenant": "acme", "environment": "test", "customer": "cus_1",
		})
		return 200, raw
	}
	c := newTestClient(t, f, WithTenant("acme"))
	cc, err := c.ForCustomer(context.Background(), "13900001111")
	if err != nil {
		t.Fatal(err)
	}
	if cc == nil || cc.Me == nil {
		t.Fatal("customer client")
	}
}

func TestForCustomerNotMatched(t *testing.T) {
	f := &fakePlatform{}
	f.customer = func(map[string]any) (int, []byte) {
		return 200, []byte(`{"matched":false}`)
	}
	c := newTestClient(t, f, WithTenant("acme"))
	_, err := c.ForCustomer(context.Background(), "13900001111")
	if !errors.Is(err, ErrNoCustomer) {
		t.Fatalf("got %v", err)
	}
}

func TestForCustomerAmbiguous(t *testing.T) {
	f := &fakePlatform{}
	f.customer = func(map[string]any) (int, []byte) {
		return 409, envelope("ambiguous_customer", "several customers share this number", "amb-1", map[string]any{
			"candidates": []any{"cus_a", "cus_b"},
		})
	}
	c := newTestClient(t, f, WithTenant("acme"))
	_, err := c.ForCustomer(context.Background(), "13900001111")
	if !IsCode(err, "ambiguous_customer") {
		t.Fatalf("got %v", err)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
	cands, _ := e.Details["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("candidates %#v", e.Details)
	}
}

func TestTenantRequired(t *testing.T) {
	f := &fakePlatform{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New("app-level", "sec", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.Platform.Whoami(context.Background())
	if err == nil || !IsCode(err, "tenant_required") {
		t.Fatalf("got %v", err)
	}
}

func TestInvalidTokenRefreshOnce(t *testing.T) {
	f := &fakePlatform{whoStatus: []int{401, 200}, expiresIn: 300}
	c := newTestClient(t, f)
	if _, err := c.Platform.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls.Load() != 2 {
		t.Fatalf("token calls %d, want 2 (initial + refresh after invalid_token)", f.tokenCalls.Load())
	}
	if f.whoCalls.Load() != 2 {
		t.Fatalf("whoami calls %d, want 2", f.whoCalls.Load())
	}
}

func mustErr(t *testing.T, fn func() error) error {
	t.Helper()
	err := fn()
	if err == nil {
		t.Fatal("expected error")
	}
	return err
}
