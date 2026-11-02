package appkit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/21stware/raise-go/access"
	"github.com/21stware/raise-go/k3"
	rt "github.com/21stware/raise-go/realtime"
	"github.com/21stware/raise-go/realtime/pmdoc"
)

// multi plays /app/v1 like platform, but keeps every request (a call that makes several is read in order).
func multi(t *testing.T, h http.HandlerFunc) (*Client, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var all []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		all = append(all, seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b), r.Header.Get("Content-Type"), r.Header.Get(HeaderTenant), r.ContentLength})
		mu.Unlock()
		w.Header().Set(HeaderTenant, r.Header.Get(HeaderTenant))
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	cl := &Client{Base: srv.URL, Token: func(context.Context, Caller) (string, error) { return "tok", nil }}
	return cl, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), all...)
	}
}

func jsonMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", s, err)
	}
	return m
}

func testManifest(docs ...k3.RealtimeDocDecl) *k3.Manifest {
	return &k3.Manifest{ID: "wiki", Realtime: &k3.RealtimeDecl{Docs: docs}}
}

// testHub is a Hub of the single declared kind "page" (codec yjs-v1) over cl.
func testHub(t *testing.T, cl *Client) rt.Hub {
	t.Helper()
	h, err := cl.Realtime(testManifest(k3.RealtimeDocDecl{Kind: "page", Codec: rt.PMCodecName, Resource: "wiki.page"})).NewHub(rt.HubConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestProjections(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"seq":7,"title":"T","plain":"p q","words":2,"refs":[]}`))
	v, ok, err := cl.Projections().Projection(ctx, "t1", "a/b c")
	if err != nil || !ok || v.Seq != 7 || v.Title != "T" || v.Plain != "p q" || v.Words != 2 {
		t.Fatalf("%+v %v %v", v, ok, err)
	}
	if s.method != "GET" || s.path != "/app/v1/realtime/docs/a%2Fb%20c/projection" || s.tenant != "t1" || s.body != "" {
		t.Fatalf("request %+v", s)
	}

	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"gone"}}`))
	_, ok, err = cl.Projections().Projection(ctx, "t1", "d1")
	if ok || err != nil {
		t.Fatalf("a missing document is not an error: ok=%v err=%v", ok, err)
	}

	cl, _ = platform(t, status(500, `{"error":"boom"}`))
	if _, ok, err = cl.Projections().Projection(ctx, "t1", "d1"); ok || err == nil {
		t.Fatalf("500: ok=%v err=%v", ok, err)
	}

	// the service is its own ProjectionSource
	cl, s = platform(t, status(200, `{"seq":1}`))
	svc := cl.Realtime(testManifest()).(rt.ProjectionSource)
	if v, ok, err := svc.Projection(ctx, "t2", "x"); err != nil || !ok || v.Seq != 1 || s.tenant != "t2" || s.path != "/app/v1/realtime/docs/x/projection" {
		t.Fatalf("%+v %v %v %+v", v, ok, err, s)
	}
}

func TestRealtimeNewHub(t *testing.T) {
	cl, get := multi(t, status(204, ""))
	ctx := context.Background()
	kindOf := func(h rt.Hub) string {
		t.Helper()
		if err := h.Invalidate(ctx, "t1", "d"); err != nil {
			t.Fatal(err)
		}
		reqs := get()
		return jsonMap(t, reqs[len(reqs)-1].body)["kind"].(string)
	}

	// no manifest / no realtime declaration
	if _, err := cl.Realtime(nil).NewHub(rt.HubConfig{}); err == nil {
		t.Fatal("nil manifest")
	}
	if _, err := cl.Realtime(&k3.Manifest{ID: "wiki"}).NewHub(rt.HubConfig{}); err == nil {
		t.Fatal("no realtime docs declared")
	}

	// one kind: empty config means yjs-v1 in the app's own namespace
	svc := cl.Realtime(testManifest(
		k3.RealtimeDocDecl{Kind: "page", Codec: rt.PMCodecName, Resource: "wiki.page"},
		k3.RealtimeDocDecl{Kind: "board", Codec: rt.BoardCodecName, Resource: "wiki.board"},
		k3.RealtimeDocDecl{Kind: "shared", Codec: rt.PMCodecName, Store: "docs", Resource: "wiki.shared"},
	))
	h, err := svc.NewHub(rt.HubConfig{})
	if err != nil || kindOf(h) != "page" {
		t.Fatalf("default: %v", err)
	}
	h, err = svc.NewHub(rt.HubConfig{Codec: rt.BoardCodecName, Store: "wiki"})
	if err != nil || kindOf(h) != "board" {
		t.Fatalf("board: %v", err)
	}
	h, err = svc.NewHub(rt.HubConfig{Store: "docs"})
	if err != nil || kindOf(h) != "shared" {
		t.Fatalf("namespace: %v", err)
	}
	if _, err := svc.NewHub(rt.HubConfig{Codec: rt.LedgerCodecName}); err == nil {
		t.Fatal("a codec no kind declares")
	}
	if _, err := svc.NewHub(rt.HubConfig{Store: "other"}); err == nil {
		t.Fatal("a namespace no kind declares")
	}

	// several kinds of one codec and namespace: HubConfig.App names one ("<kind>" or "<app>.<kind>")
	svc = cl.Realtime(testManifest(
		k3.RealtimeDocDecl{Kind: "note", Codec: rt.PMCodecName, Resource: "wiki.note"},
		k3.RealtimeDocDecl{Kind: "page", Codec: rt.PMCodecName, Resource: "wiki.page"},
	))
	if _, err := svc.NewHub(rt.HubConfig{}); err == nil {
		t.Fatal("ambiguous without App")
	}
	if _, err := svc.NewHub(rt.HubConfig{App: "other"}); err == nil {
		t.Fatal("ambiguous with an App that names no kind")
	}
	for app, want := range map[string]string{"note": "note", "page": "page", "wiki.note": "note", "wiki.page": "page"} {
		h, err := svc.NewHub(rt.HubConfig{App: app})
		if err != nil || kindOf(h) != want {
			t.Fatalf("App %q: want %s, err %v", app, want, err)
		}
	}
}

func TestRemoteHubStubs(t *testing.T) {
	cl, get := multi(t, status(204, ""))
	h := testHub(t, cl)
	ctx := context.Background()
	if !h.Serving() {
		t.Fatal("Serving")
	}
	h.Close(1)
	h.CloseAll(1)
	if st := h.Stats(); st.Conns != 0 || st.Accepted != 0 {
		t.Fatalf("stats %+v", st)
	}
	if err := h.Admit(Caller{Tenant: "t", User: "u"}); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("Admit %v", err)
	}
	if err := h.Serve(ctx, nil, Caller{}, time.Time{}); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("Serve %v", err)
	}
	if err := h.Kick(ctx, "t", "u", 4003, "x"); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("Kick %v", err)
	}
	if err := h.Rebuild(ctx, rt.DocRef{Tenant: "t", ID: "d"}); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("Rebuild %v", err)
	}
	if _, err := h.ScrubOutdated(ctx, "t"); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("ScrubOutdated %v", err)
	}
	if n := len(get()); n != 0 {
		t.Fatalf("stubs made %d requests", n)
	}

	svc := cl.Realtime(testManifest())
	if c, ok := svc.Codec("yjs-v1"); ok || c != nil {
		t.Fatal("Codec answers nothing")
	}
}

func TestRealtimeHeads(t *testing.T) {
	ctx := context.Background()
	m := testManifest(
		k3.RealtimeDocDecl{Kind: "page", Codec: rt.PMCodecName, Resource: "wiki.page"},
		k3.RealtimeDocDecl{Kind: "shared", Codec: rt.PMCodecName, Store: "docs", Resource: "wiki.shared"},
	)
	cl, get := multi(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/v1/realtime/docs/p1":
			_, _ = io.WriteString(w, `{"id":"p1","kind":"page","codec":"yjs-v1","epoch":2,"schemaVer":3,"headSeq":10,"projSeq":9,"updatedAt":"2026-10-31T08:00:00Z"}`)
		case "/app/v1/realtime/docs/s1":
			_, _ = io.WriteString(w, `{"id":"s1","kind":"shared","codec":"yjs-v1","epoch":1,"headSeq":1,"projSeq":1}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	})
	svc := cl.Realtime(m)

	// store "" : every namespace; the missing document is left out
	got, err := svc.Heads(ctx, "t1", "", "p1", "gone", "s1")
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	h := got["p1"]
	if h.Codec != "yjs-v1" || h.Epoch != 2 || h.SchemaVer != 3 || h.HeadSeq != 10 || h.ProjSeq != 9 || h.UpdatedAt.IsZero() || h.SnapSeq != 0 || h.PendingBytes != 0 {
		t.Fatalf("head %+v", h)
	}
	reqs := get()
	if len(reqs) != 3 || reqs[0].method != "GET" || reqs[0].path != "/app/v1/realtime/docs/p1" || reqs[0].tenant != "t1" || reqs[1].path != "/app/v1/realtime/docs/gone" {
		t.Fatalf("requests %+v", reqs)
	}

	// a store names the namespace: the app's own ("wiki") leaves out the shared kind's document, and the other way round
	if got, err = svc.Heads(ctx, "t1", "wiki", "p1", "s1"); err != nil || len(got) != 1 || got["p1"].Codec == "" {
		t.Fatalf("wiki: %v %v", got, err)
	}
	if got, err = svc.Heads(ctx, "t1", "docs", "p1", "s1"); err != nil || len(got) != 1 || got["s1"].Codec == "" {
		t.Fatalf("docs: %v %v", got, err)
	}

	cl, _ = platform(t, status(500, `{"error":"x"}`))
	if _, err := cl.Realtime(m).Heads(ctx, "t1", "", "p1"); err == nil {
		t.Fatal("a failure is an error")
	}
}

func TestChunks(t *testing.T) {
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	got := chunks(ids, 100)
	if len(got) != 3 || len(got[0]) != 100 || len(got[1]) != 100 || len(got[2]) != 50 || got[2][0] != "200" {
		t.Fatalf("%d chunks", len(got))
	}
	if got := chunks(ids[:100], 100); len(got) != 1 || len(got[0]) != 100 {
		t.Fatalf("exact: %v", len(got))
	}
	if got := chunks(nil, 100); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestHubInvalidateRevokeUninstalled(t *testing.T) {
	ctx := context.Background()
	cl, get := multi(t, status(204, ""))
	h := testHub(t, cl)

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "d" + strconv.Itoa(i)
	}
	if err := h.Invalidate(ctx, "t1", ids...); err != nil {
		t.Fatal(err)
	}
	reqs := get()
	if len(reqs) != 3 {
		t.Fatalf("%d requests for 250 ids", len(reqs))
	}
	for _, r := range reqs {
		if r.method != "POST" || r.path != "/app/v1/realtime/docs/invalidate" || r.tenant != "t1" {
			t.Fatalf("request %+v", r)
		}
	}
	b := jsonMap(t, reqs[2].body)
	if b["kind"] != "page" || len(b["ids"].([]any)) != 50 {
		t.Fatalf("body %v", b)
	}
	if err := h.Invalidate(ctx, "t1"); err != nil || len(get()) != 3 {
		t.Fatal("no ids, no call")
	}

	if err := h.InvalidateTenant(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	r := get()[3]
	if r.method != "POST" || r.path != "/app/v1/realtime/connections/invalidate" || r.body != `{"kind":"page"}` || r.tenant != "t1" {
		t.Fatalf("invalidate tenant %+v", r)
	}

	if err := h.Revoke(ctx, "t1", "moved", "a", "b"); err != nil {
		t.Fatal(err)
	}
	r = get()[4]
	if r.path != "/app/v1/realtime/docs/revoke" || r.body != `{"ids":["a","b"],"kind":"page","reason":"moved"}` {
		t.Fatalf("revoke %+v", r)
	}

	if err := h.Uninstalled(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	r = get()[5]
	if r.method != "POST" || r.path != "/app/v1/realtime/connections/uninstalled" || r.body != "" || r.tenant != "t1" {
		t.Fatalf("uninstalled %+v", r)
	}
}

func TestHubRevokeNotFound(t *testing.T) {
	ctx := context.Background()
	cl, _ := multi(t, status(404, `{"error":{"code":"not_found","message":"no such document"}}`))
	h := testHub(t, cl)
	if err := h.Revoke(ctx, "t1", "deleted", "a"); err != nil {
		t.Fatalf("a document already deleted is not an error for reason deleted: %v", err)
	}
	if err := h.Revoke(ctx, "t1", "moved", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another reason keeps the error: %v", err)
	}
	cl, _ = multi(t, status(500, `{"error":"x"}`))
	if err := testHub(t, cl).Revoke(ctx, "t1", "deleted", "a"); err == nil {
		t.Fatal("500 with reason deleted is still an error")
	}
	// 404 on the first chunk does not stop the next one
	n := 0
	cl, get := multi(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	})
	ids := make([]string, 150)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	if err := testHub(t, cl).Revoke(ctx, "t1", "deleted", ids...); err != nil || len(get()) != 2 {
		t.Fatalf("err %v, %d requests", err, len(get()))
	}
}

func TestHubPublish(t *testing.T) {
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{method: r.Method, path: r.URL.EscapedPath(), body: string(b), tenant: r.Header.Get(HeaderTenant)}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	cl := &Client{Base: srv.URL, Token: func(context.Context, Caller) (string, error) { return "tok", nil }}
	testHub(t, cl).Publish("t1", "wiki.space:s 1", 42) // fire and forget
	select {
	case s := <-got:
		if s.method != "POST" || s.path != "/app/v1/realtime/topics/wiki.space:s%201/publish" || s.tenant != "t1" || s.body != `{"kind":"page","version":42}` {
			t.Fatalf("publish %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no publish request")
	}
}

type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestHubPublishFailureIsLogged(t *testing.T) {
	cl, _ := multi(t, status(500, `{"error":"down"}`))
	sink := &logSink{}
	svc := cl.Realtime(testManifest(k3.RealtimeDocDecl{Kind: "page", Codec: rt.PMCodecName, Resource: "wiki.page"})).(*RealtimeService)
	svc.Log = slog.New(slog.NewTextHandler(sink, nil))
	h, err := svc.NewHub(rt.HubConfig{})
	if err != nil {
		t.Fatal(err)
	}
	h.Publish("t1", "x:1", 1)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sink.String(), "publishing a real-time topic failed") {
		if time.Now().After(deadline) {
			t.Fatalf("not logged: %q", sink.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHubGenesis(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		code    int
		created bool
	}{{201, true}, {200, false}} {
		cl, get := multi(t, status(tc.code, `{}`))
		h := testHub(t, cl)
		doc := &pmdoc.Node{Type: "doc", Content: []*pmdoc.Node{{Type: "paragraph"}}}
		created, err := h.Genesis(ctx, nil, rt.DocRef{Tenant: "t1", ID: "p1"}, doc)
		if err != nil || created != tc.created {
			t.Fatalf("%d: created=%v err=%v", tc.code, created, err)
		}
		r := get()[0]
		b := jsonMap(t, r.body)
		if r.method != "POST" || r.path != "/app/v1/realtime/docs" || r.tenant != "t1" || b["kind"] != "page" || b["id"] != "p1" || b["resource"] != "p1" {
			t.Fatalf("%d: %+v", tc.code, r)
		}
		if d, _ := b["doc"].(map[string]any); d["type"] != "doc" {
			t.Fatalf("doc %v", b["doc"])
		}
		if _, has := b["state"]; has {
			t.Fatal("no state in a Genesis")
		}
	}

	// without a document: none in the body; the resource named by the context
	cl, get := multi(t, status(201, `{}`))
	h := testHub(t, cl)
	if _, err := h.Genesis(WithRealtimeResource(ctx, "r9"), nil, rt.DocRef{Tenant: "t1", ID: "p1"}, nil); err != nil {
		t.Fatal(err)
	}
	b := jsonMap(t, get()[0].body)
	if _, has := b["doc"]; has || b["resource"] != "r9" || b["id"] != "p1" {
		t.Fatalf("body %v", b)
	}
	// an empty resource id in the context falls back to the document's id
	if _, err := h.Genesis(WithRealtimeResource(ctx, ""), nil, rt.DocRef{Tenant: "t1", ID: "p2"}, nil); err != nil {
		t.Fatal(err)
	}
	if b := jsonMap(t, get()[1].body); b["resource"] != "p2" {
		t.Fatalf("body %v", b)
	}

	// errors
	cl, _ = multi(t, status(403, `{"error":"no"}`))
	if _, err := testHub(t, cl).Genesis(ctx, nil, rt.DocRef{Tenant: "t1", ID: "p1"}, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("%v", err)
	}
}

func TestHubGenesisState(t *testing.T) {
	cl, get := multi(t, status(201, `{}`))
	h := testHub(t, cl)
	created, err := h.GenesisState(context.Background(), nil, rt.DocRef{Tenant: "t1", ID: "p1"}, []byte{0, 1, 2, 255})
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	b := jsonMap(t, get()[0].body)
	if b["state"] != base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255}) || b["kind"] != "page" || b["id"] != "p1" || get()[0].path != "/app/v1/realtime/docs" {
		t.Fatalf("body %v", b)
	}
}

func TestHubBumpEpoch(t *testing.T) {
	ctx := context.Background()
	d := rt.DocRef{Tenant: "t1", ID: "p/1"}

	cl, get := multi(t, status(200, `{"epoch":4}`))
	svc := cl.Realtime(testManifest(k3.RealtimeDocDecl{Kind: "page", Codec: rt.PMCodecName, Resource: "wiki.page"})).(*RealtimeService)
	h, _ := svc.NewHub(rt.HubConfig{})

	// an agent never starts a document over
	if _, err := h.BumpEpoch(ctx, Caller{Tenant: "t1", User: "u", Agent: "claude"}, d, nil, nil); !errors.Is(err, rt.ErrAgent) {
		t.Fatalf("agent: %v", err)
	}
	if len(get()) != 0 {
		t.Fatal("an agent made a request")
	}

	// before needs InTx
	called := false
	if _, err := h.BumpEpoch(ctx, Caller{User: "u"}, d, nil, func(*sql.Tx) error { called = true; return nil }); !errors.Is(err, rt.ErrUnsupported) || called {
		t.Fatalf("before without InTx: %v called=%v", err, called)
	}

	// without before: the tenant comes from the document
	doc := &pmdoc.Node{Type: "doc"}
	ep, err := h.BumpEpoch(ctx, Caller{User: "u"}, d, doc, nil)
	if err != nil || ep != 4 {
		t.Fatalf("%d %v", ep, err)
	}
	r := get()[0]
	if r.method != "POST" || r.path != "/app/v1/realtime/docs/p%2F1/epoch" || r.tenant != "t1" || jsonMap(t, r.body)["doc"].(map[string]any)["type"] != "doc" {
		t.Fatalf("request %+v", r)
	}
	if _, err := h.BumpEpoch(ctx, Caller{User: "u"}, d, nil, nil); err != nil || get()[1].body != `{}` {
		t.Fatalf("empty body %q %v", get()[1].body, err)
	}

	// before runs first, in the app's transaction
	var order []string
	svc.InTx = func(_ context.Context, tenant string, fn func(*sql.Tx) error) error {
		order = append(order, "intx:"+tenant)
		return fn(nil)
	}
	before := func(*sql.Tx) error { order = append(order, "before"); return nil }
	n := len(get())
	if ep, err := h.BumpEpoch(ctx, Caller{Tenant: "t1", User: "u"}, d, nil, before); err != nil || ep != 4 {
		t.Fatalf("%d %v", ep, err)
	}
	if strings.Join(order, ",") != "intx:t1,before" || len(get()) != n+1 {
		t.Fatalf("order %v, %d requests", order, len(get())-n)
	}

	// a before that fails: the platform is not asked
	boom := errors.New("boom")
	if _, err := h.BumpEpoch(ctx, Caller{User: "u"}, d, nil, func(*sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("expected the before error: %v", err)
	}
	if len(get()) != n+1 {
		t.Fatal("the platform was asked after before failed")
	}

	// the platform refuses
	cl2, _ := multi(t, status(409, `{"error":"stale"}`))
	if _, err := testHub(t, cl2).BumpEpoch(ctx, Caller{User: "u"}, d, nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
}

func TestHubStoreHead(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"id":"p1","kind":"page","codec":"yjs-v1","epoch":1,"schemaVer":2,"headSeq":5,"projSeq":4}`))
	hd, err := testHub(t, cl).StoreHead(ctx, rt.DocRef{Tenant: "t1", ID: "p1"})
	if err != nil || hd.HeadSeq != 5 || hd.ProjSeq != 4 || hd.SchemaVer != 2 || hd.Epoch != 1 || s.method != "GET" || s.path != "/app/v1/realtime/docs/p1" || s.tenant != "t1" {
		t.Fatalf("%+v %v %+v", hd, err, s)
	}
	cl, _ = platform(t, status(404, `{"error":"x"}`))
	if _, err := testHub(t, cl).StoreHead(ctx, rt.DocRef{Tenant: "t1", ID: "p1"}); !errors.Is(err, rt.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	cl, _ = platform(t, status(500, `{"error":"x"}`))
	if _, err := testHub(t, cl).StoreHead(ctx, rt.DocRef{Tenant: "t1", ID: "p1"}); err == nil || errors.Is(err, rt.ErrNotFound) {
		t.Fatalf("500: %v", err)
	}
}

func TestHubState(t *testing.T) {
	ctx := context.Background()
	d := rt.DocRef{Tenant: "t1", ID: "p1"}
	enc := base64.StdEncoding.EncodeToString([]byte("hello"))

	cl, s := platform(t, status(200, `{"seq":9,"codec":"yjs-v1","state":"`+enc+`"}`))
	b, seq, err := testHub(t, cl).State(ctx, d)
	if err != nil || string(b) != "hello" || seq != 9 || s.method != "GET" || s.path != "/app/v1/realtime/docs/p1/state" || s.tenant != "t1" {
		t.Fatalf("%q %d %v %+v", b, seq, err, s)
	}

	// no codec in the answer: accepted
	cl, _ = platform(t, status(200, `{"seq":1,"state":"`+enc+`"}`))
	if b, _, err := testHub(t, cl).State(ctx, d); err != nil || string(b) != "hello" {
		t.Fatalf("%q %v", b, err)
	}

	// another codec than the hub's
	cl, _ = platform(t, status(200, `{"seq":9,"codec":"board-v1","state":"`+enc+`"}`))
	if _, _, err := testHub(t, cl).State(ctx, d); !errors.Is(err, rt.ErrWrongCodec) {
		t.Fatalf("codec mismatch: %v", err)
	}

	cl, _ = platform(t, status(200, `{"seq":9,"codec":"yjs-v1","state":"***"}`))
	if _, _, err := testHub(t, cl).State(ctx, d); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("bad base64: %v", err)
	}

	cl, _ = platform(t, status(404, `{"error":"x"}`))
	if _, _, err := testHub(t, cl).State(ctx, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
}

func TestHubServerEdit(t *testing.T) {
	ctx := context.Background()
	d := rt.DocRef{Tenant: "t1", ID: "p1"}
	reply := `{"seq":12,"projection":{"seq":12,"title":"Hello","plain":"body","words":1,"refs":[]}}`
	node := &pmdoc.Node{Type: "paragraph", Content: []*pmdoc.Node{{Type: "text", Text: "x"}}}
	doc := &pmdoc.Node{Type: "doc", Content: []*pmdoc.Node{node}}

	cases := []struct {
		name string
		edit rt.Edit
		key  string
	}{
		{"replace", rt.Replace(doc), "replace"},
		{"append", rt.Append(node), "append"},
		{"title", rt.SetTitle("New"), "title"},
		{"data", rt.ReplaceData(json.RawMessage(`{"a":1}`)), "data"},
		{"board", rt.ReplaceBoard(&rt.BoardDoc{Name: "B"}), "board"},
	}
	for _, tc := range cases {
		cl, get := multi(t, status(200, reply))
		h := testHub(t, cl)
		p, seq, err := h.ServerEdit(ctx, Caller{User: "u1", Name: "U"}, d, tc.edit, rt.OriginUser, tc.name == "title")
		if err != nil || seq != 12 {
			t.Fatalf("%s: %d %v", tc.name, seq, err)
		}
		if p.Seq != 12 || p.Title != "Hello" || p.Plain != "body" || p.Epoch != 0 || len(p.Actors) != 1 || p.Actors[0] != "u1" {
			t.Fatalf("%s: projection %+v", tc.name, p)
		}
		r := get()[0]
		if r.method != "POST" || r.path != "/app/v1/realtime/docs/p1/edits" || r.tenant != "t1" {
			t.Fatalf("%s: %+v", tc.name, r)
		}
		b := jsonMap(t, r.body)
		edit := b["edit"].(map[string]any)
		if len(edit) != 1 || edit[tc.key] == nil || b["origin"] != "user" || b["dry"] != (tc.name == "title") {
			t.Fatalf("%s: body %v", tc.name, b)
		}
	}

	// the data edit carries the raw JSON object
	cl, get := multi(t, status(200, reply))
	if _, _, err := testHub(t, cl).ServerEdit(ctx, Caller{User: "u"}, d, rt.ReplaceData(json.RawMessage(`{"a":1}`)), "", false); err != nil {
		t.Fatal(err)
	}
	if e := jsonMap(t, get()[0].body)["edit"].(map[string]any)["data"].(map[string]any); e["a"] != float64(1) {
		t.Fatalf("data %v", e)
	}

	// an agent never writes; a repair is the platform's own; an unknown kind is unsupported
	h := testHub(t, cl)
	if _, _, err := h.ServerEdit(ctx, Caller{User: "u", Agent: "claude"}, d, rt.SetTitle("x"), "", false); !errors.Is(err, rt.ErrAgent) {
		t.Fatalf("agent: %v", err)
	}
	if _, _, err := h.ServerEdit(ctx, Caller{User: "u"}, d, rt.Repair(), "", false); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("repair: %v", err)
	}
	if _, _, err := h.ServerEdit(ctx, Caller{User: "u"}, d, rt.Edit{Kind: 99}, "", false); !errors.Is(err, rt.ErrUnsupported) {
		t.Fatalf("kind 99: %v", err)
	}
	if n := len(get()); n != 1 {
		t.Fatalf("%d requests (agent, repair and unknown must not reach the platform)", n)
	}

	// no user: no actors; the tenant of the actor wins over the document's
	cl, get = multi(t, status(200, reply))
	p, _, err := testHub(t, cl).ServerEdit(ctx, Caller{Tenant: "t9"}, d, rt.SetTitle("x"), "", false)
	if err != nil || p.Actors == nil || len(p.Actors) != 0 || get()[0].tenant != "t9" {
		t.Fatalf("%+v %v %+v", p, err, get())
	}

	cl, _ = multi(t, status(403, `{"error":"no"}`))
	if _, _, err := testHub(t, cl).ServerEdit(ctx, Caller{User: "u"}, d, rt.SetTitle("x"), "", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("%v", err)
	}
}

func TestHubEnsureProjected(t *testing.T) {
	ctx := context.Background()
	d := rt.DocRef{Tenant: "t1", ID: "p1"}
	cl, get := multi(t, status(200, `{"seq":5}`))
	h := testHub(t, cl)
	for _, tc := range []struct {
		wait time.Duration
		want string
	}{
		{2 * time.Second, "minSeq=5&wait=2000"},
		{time.Minute, "minSeq=5&wait=5000"},
		{-time.Second, "minSeq=5&wait=0"},
		{0, "minSeq=5&wait=0"},
	} {
		if err := h.EnsureProjected(ctx, d, 5, tc.wait); err != nil {
			t.Fatal(err)
		}
		r := get()[len(get())-1]
		if r.method != "GET" || r.path != "/app/v1/realtime/docs/p1/projection" || r.query != tc.want || r.tenant != "t1" {
			t.Fatalf("wait %v: %+v", tc.wait, r)
		}
	}
	cl, _ = multi(t, status(409, `{"error":"projection not there yet"}`))
	if err := testHub(t, cl).EnsureProjected(ctx, d, 5, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("not far enough: %v", err)
	}
}

func TestHubScrub(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"repaired":true,"compacted":false}`))
	res, err := testHub(t, cl).Scrub(ctx, rt.DocRef{Tenant: "t1", ID: "p1"})
	if err != nil || !res.Repaired || res.Compacted || s.method != "POST" || s.path != "/app/v1/realtime/docs/p1/scrub" || s.tenant != "t1" {
		t.Fatalf("%+v %v %+v", res, err, s)
	}
	cl, _ = platform(t, status(500, `{"error":"x"}`))
	if _, err := testHub(t, cl).Scrub(ctx, rt.DocRef{Tenant: "t1", ID: "p1"}); err == nil {
		t.Fatal("500")
	}
}

func TestHubDelete(t *testing.T) {
	ctx := context.Background()
	cl, get := multi(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	})
	h := testHub(t, cl)
	if err := h.Delete(ctx, nil, "t1", "a", "gone", "b c"); err != nil {
		t.Fatalf("a document already gone is not an error: %v", err)
	}
	reqs := get()
	if len(reqs) != 3 || reqs[0].method != "DELETE" || reqs[0].path != "/app/v1/realtime/docs/a" || reqs[2].path != "/app/v1/realtime/docs/b%20c" || reqs[2].tenant != "t1" {
		t.Fatalf("requests %+v", reqs)
	}

	cl, get = multi(t, status(500, `{"error":"x"}`))
	if err := testHub(t, cl).Delete(ctx, nil, "t1", "a", "b"); err == nil || len(get()) != 1 {
		t.Fatalf("a failure stops: err %v, %d requests", err, len(get()))
	}
}

func TestRealtimeReadMeta(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"meta":{"title":"Deck","engine":"x"}}`))
	m, err := cl.Realtime(testManifest()).(rt.MetaReader).ReadMeta(ctx, "t1", "lectern-v1", []byte("state"))
	if err != nil || m["title"] != "Deck" || m["engine"] != "x" {
		t.Fatalf("%v %v", m, err)
	}
	if s.method != "POST" || s.path != "/app/v1/realtime/codecs/lectern-v1/meta" || s.tenant != "t1" || s.body != `{"state":"`+base64.StdEncoding.EncodeToString([]byte("state"))+`"}` {
		t.Fatalf("request %+v", s)
	}
	cl, _ = platform(t, status(422, `{"error":{"message":"no properties"}}`))
	if _, err := cl.Realtime(testManifest()).(rt.MetaReader).ReadMeta(ctx, "t1", "yjs-v1", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

// ---- the access tree ----

func TestTreeLevelNames(t *testing.T) {
	for name, l := range map[string]int{"view": sdk.View, "comment": sdk.Comment, "edit": sdk.Edit, "manage": sdk.Manage} {
		if levelName(l) != name || parseLevel(name) != l {
			t.Fatalf("%s: %q %d", name, levelName(l), parseLevel(name))
		}
	}
	if levelName(0) != "none" || levelName(-1) != "none" {
		t.Fatal("none")
	}
	if levelName(sdk.Manage+3) != "manage" {
		t.Fatal("above manage is manage")
	}
	for _, s := range []string{"", "none", "owner", "VIEW"} {
		if parseLevel(s) != 0 {
			t.Fatalf("parseLevel(%q)", s)
		}
	}
}

func TestTreePutRootNodeDelete(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(204, ""))
	if err := cl.Tree().PutRoot(ctx, "t1", "wiki.space", "s/1", sdk.RootSpec{Archived: true}); err != nil {
		t.Fatal(err)
	}
	if s.method != "POST" || s.path != "/app/v1/access/wiki.space/s%2F1" || s.tenant != "t1" || s.body != `{"archived":true,"publishOnly":false}` {
		t.Fatalf("root %+v", s)
	}

	cl, s = platform(t, status(200, `{"changed":{"n1":"set-a","n2":"set-b"}}`))
	ch, err := cl.Tree().PutNode(ctx, "t1", "wiki.page", "n1", sdk.NodeSpec{Root: "s1", Parent: "p", Inherit: true})
	if err != nil || len(ch) != 2 || ch["n1"] != "set-a" || ch["n2"] != "set-b" {
		t.Fatalf("%v %v", ch, err)
	}
	if s.method != "POST" || s.path != "/app/v1/access/wiki.page/n1" || s.body != `{"inherit":true,"parent":"p","root":"s1","trashed":false}` {
		t.Fatalf("node %+v", s)
	}
	cl, _ = platform(t, status(200, `{}`))
	if ch, err := cl.Tree().PutNode(ctx, "t1", "wiki.page", "n1", sdk.NodeSpec{}); err != nil || ch == nil || len(ch) != 0 {
		t.Fatalf("empty changed must be a non-nil map: %#v %v", ch, err)
	}

	cl, get := multi(t, status(204, ""))
	if err := cl.Tree().Delete(ctx, "t1", "wiki.page", "a", "b"); err != nil {
		t.Fatal(err)
	}
	reqs := get()
	if len(reqs) != 2 || reqs[0].method != "DELETE" || reqs[0].path != "/app/v1/access/wiki.page/a" || reqs[1].path != "/app/v1/access/wiki.page/b" || reqs[1].tenant != "t1" {
		t.Fatalf("%+v", reqs)
	}
	cl, get = multi(t, status(500, `{"error":"x"}`))
	if err := cl.Tree().Delete(ctx, "t1", "wiki.page", "a", "b"); err == nil || len(get()) != 1 {
		t.Fatalf("stops at the first failure: %v %d", err, len(get()))
	}
}

func TestTreeSetEntries(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"changed":{"n1":"s2"}}`))
	entries := []sdk.Entry{
		{Kind: "user", Principal: "u1", Level: sdk.Edit, Name: "Ann"},
		{Kind: "dept", Principal: "d1", Level: sdk.View},
	}
	ch, err := cl.Tree().SetEntries(ctx, "t1", "wiki.space", "s1", entries, "u9")
	if err != nil || ch["n1"] != "s2" {
		t.Fatalf("%v %v", ch, err)
	}
	if s.method != "PUT" || s.path != "/app/v1/access/wiki.space/s1/grants" || s.tenant != "t1" {
		t.Fatalf("%+v", s)
	}
	b := jsonMap(t, s.body)
	es := b["entries"].([]any)
	e0 := es[0].(map[string]any)
	if b["by"] != "u9" || len(es) != 2 || e0["kind"] != "user" || e0["principal"] != "u1" || e0["level"] != "edit" || e0["name"] != "Ann" || es[1].(map[string]any)["level"] != "view" {
		t.Fatalf("body %v", b)
	}
	// no entries: an empty list, not null
	if _, err := cl.Tree().SetEntries(ctx, "t1", "wiki.space", "s1", nil, "u9"); err != nil || !strings.Contains(s.body, `"entries":[]`) {
		t.Fatalf("%v %q", err, s.body)
	}

	// a level outside View … Manage never reaches the platform
	cl, get := multi(t, status(200, `{}`))
	for _, l := range []int{0, -1, sdk.Manage + 1} {
		_, err := cl.Tree().SetEntries(ctx, "t1", "wiki.space", "s1", []sdk.Entry{{Kind: "user", Principal: "u", Level: l}}, "")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("level %d: %v", l, err)
		}
	}
	if len(get()) != 0 {
		t.Fatal("an invalid entry was sent")
	}
}

func TestTreeEntriesEffectiveSets(t *testing.T) {
	ctx := context.Background()
	view := `{"entries":[{"kind":"user","principal":"u1","level":"manage","name":"Ann","at":"2026-10-31T08:00:00.5Z"},{"kind":"role","principal":"r1","level":"bogus"}],` +
		`"effective":[{"kind":"user","principal":"u2","level":"comment"},{"kind":"dept","principal":"d1","level":"view"}],"set":"set-7"}`
	cl, s := platform(t, status(200, view))
	es, err := cl.Tree().Entries(ctx, "t1", "wiki.space", "s1")
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
	if es[0].Level != sdk.Manage || es[0].Name != "Ann" || es[0].At.IsZero() || es[0].At.Nanosecond() != 500000000 || es[1].Level != 0 || !es[1].At.IsZero() {
		t.Fatalf("entries %+v", es)
	}
	if s.method != "GET" || s.path != "/app/v1/access/wiki.space/s1" || s.tenant != "t1" || s.body != "" {
		t.Fatalf("%+v", s)
	}
	eff, err := cl.Tree().Effective(ctx, "t1", "wiki.space", "s1")
	if err != nil || len(eff) != 2 || eff[0].Level != sdk.Comment || eff[1].Level != sdk.View || eff[1].Kind != "dept" {
		t.Fatalf("%+v %v", eff, err)
	}

	cl, get := multi(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/v1/access/wiki.page/a":
			_, _ = io.WriteString(w, `{"set":"s-a"}`)
		case "/app/v1/access/wiki.page/b":
			_, _ = io.WriteString(w, `{"set":"s-b"}`)
		default:
			w.WriteHeader(404)
		}
	})
	sets, err := cl.Tree().Sets(ctx, "t1", "wiki.page", "a", "missing", "b")
	if err != nil || len(sets) != 2 || sets["a"] != "s-a" || sets["b"] != "s-b" {
		t.Fatalf("a node the kernel does not have has no set: %v %v", sets, err)
	}
	if len(get()) != 3 {
		t.Fatalf("%d requests", len(get()))
	}
	cl, _ = platform(t, status(500, `{"error":"x"}`))
	if _, err := cl.Tree().Sets(ctx, "t1", "wiki.page", "a"); err == nil {
		t.Fatal("500")
	}
}

func TestTreeMineAndLevel(t *testing.T) {
	ctx := context.Background()
	var gotCaller Caller
	cl, s := platform(t, status(200, `{"sets":{"s1":"edit","s2":"view","s3":"x"},"truncated":true,"next":"s3","bypass":true}`))
	cl.Token = func(_ context.Context, c Caller) (string, error) { gotCaller = c; return "tok", nil }
	c := Caller{Tenant: "t1", User: "u1", Name: "Ann"}
	m, err := cl.Tree().Mine(ctx, c, "s0")
	if err != nil || m.Sets["s1"] != sdk.Edit || m.Sets["s2"] != sdk.View || m.Sets["s3"] != 0 || len(m.Sets) != 3 || !m.Truncated || m.Next != "s3" || !m.Bypass {
		t.Fatalf("%+v %v", m, err)
	}
	if s.method != "GET" || s.path != "/app/v1/access/*/mine" || s.query != "after=s0&user=u1" || s.tenant != "t1" || gotCaller.User != "u1" {
		t.Fatalf("%+v caller %+v", s, gotCaller)
	}
	if _, err := cl.Tree().Mine(ctx, c, ""); err != nil || s.query != "user=u1" {
		t.Fatalf("no after: %v %q", err, s.query)
	}

	cl, s = platform(t, status(200, `{"levels":{"n1":"comment"},"reasons":{"n1":"archived"}}`))
	cl.Token = func(_ context.Context, c Caller) (string, error) { gotCaller = c; return "tok", nil }
	lv, err := cl.Tree().Level(ctx, c, "wiki.page", "n1")
	if err != nil || lv.Level != sdk.Comment || lv.Reason != "archived" {
		t.Fatalf("%+v %v", lv, err)
	}
	if s.method != "POST" || s.path != "/app/v1/access/wiki.page/check" || s.tenant != "t1" || gotCaller.User != "u1" {
		t.Fatalf("%+v", s)
	}
	b := jsonMap(t, s.body)
	if b["user"] != "u1" || b["agent"] != false || len(b["ids"].([]any)) != 1 || b["ids"].([]any)[0] != "n1" {
		t.Fatalf("body %v", b)
	}
	// an agent's call says so
	if _, err := cl.Tree().Level(ctx, Caller{Tenant: "t1", User: "u1", Agent: "claude"}, "wiki.page", "n1"); err != nil || jsonMap(t, s.body)["agent"] != true {
		t.Fatalf("agent: %v %q", err, s.body)
	}
	// an id the answer does not name has no level
	if lv, err := cl.Tree().Level(ctx, c, "wiki.page", "other"); err != nil || lv.Level != 0 || lv.Reason != "" {
		t.Fatalf("%+v %v", lv, err)
	}
}

func TestTreeReconcile(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"changed":3}`))
	n, err := cl.Tree().Reconcile(ctx, "t1", "wiki.space", "s1", sdk.RootSpec{PublishOnly: true}, []sdk.Node{{ID: "n1", NodeSpec: sdk.NodeSpec{Root: "s1", Inherit: true}}})
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if s.method != "POST" || s.path != "/app/v1/access/wiki.space/s1/reconcile" || s.tenant != "t1" {
		t.Fatalf("%+v", s)
	}
	b := jsonMap(t, s.body)
	node := b["nodes"].([]any)[0].(map[string]any)
	if b["spec"].(map[string]any)["publishOnly"] != true || node["id"] != "n1" || node["root"] != "s1" || node["inherit"] != true {
		t.Fatalf("body %v", b)
	}
	if _, err := cl.Tree().Reconcile(ctx, "t1", "wiki.space", "s1", sdk.RootSpec{}, nil); err != nil || !strings.Contains(s.body, `"nodes":[]`) {
		t.Fatalf("nil nodes are an empty list: %v %q", err, s.body)
	}
}

// ---- the org chart and backlinks ----

func TestOrgChart(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"items":[{"id":"d1","name":"R&D","path":"/d1/"},{"id":"d2","parentId":"d1","name":"Web","path":"/d1/d2/"}]}`))
	us, err := cl.Org().Units(ctx, "t1")
	if err != nil || len(us) != 2 || us[1].ParentID != "d1" || us[1].Path != "/d1/d2/" {
		t.Fatalf("%+v %v", us, err)
	}
	if s.method != "GET" || s.path != "/app/v1/org/units" || s.tenant != "t1" || s.body != "" {
		t.Fatalf("%+v", s)
	}

	cl, s = platform(t, status(200, `{"items":[{"id":"r1","name":"Editor"}]}`))
	rs, err := cl.Org().Roles(ctx, "t1")
	if err != nil || len(rs) != 1 || rs[0].Name != "Editor" || s.path != "/app/v1/org/roles" {
		t.Fatalf("%+v %v %+v", rs, err, s)
	}

	cl, s = platform(t, status(200, `{"units":["d2","d1"],"roles":["r1"]}`))
	us2, rs2, err := cl.Org().Placement(ctx, "t1", "u 1")
	if err != nil || len(us2) != 2 || us2[0] != "d2" || len(rs2) != 1 || rs2[0] != "r1" {
		t.Fatalf("%v %v %v", us2, rs2, err)
	}
	if s.path != "/app/v1/org/placement" || s.query != "user=u+1" {
		t.Fatalf("%+v", s)
	}

	cl, _ = platform(t, status(200, `{}`))
	us, err = cl.Org().Units(ctx, "t1")
	if err != nil || len(us) != 0 {
		t.Fatalf("%v %v", us, err)
	}

	cl, _ = platform(t, status(403, `{"error":"no"}`))
	if _, err := cl.Org().Units(ctx, "t1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("%v", err)
	}
	// an answer for another tenant is refused
	cl, _ = platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderTenant, "other")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"d1"}]}`)
	})
	if _, err := cl.Org().Units(ctx, "t1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other tenant: %v", err)
	}
}

func TestClientBacklinks(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"items":[{"ref":"k3://notes/note/1","app":"notes","kind":"note","title":"N","context":"see [[x]]"}]}`))
	bl, err := cl.Backlinks(ctx, Caller{Tenant: "t1", User: "u1"}, "k3://wiki/page/p 1", 20)
	if err != nil || len(bl) != 1 || bl[0].App != "notes" || bl[0].Title != "N" || bl[0].Context != "see [[x]]" {
		t.Fatalf("%+v %v", bl, err)
	}
	if s.method != "GET" || s.path != "/app/v1/backlinks" || s.tenant != "t1" || s.query != "limit=20&ref=k3%3A%2F%2Fwiki%2Fpage%2Fp+1" {
		t.Fatalf("%+v", s)
	}
	cl, _ = platform(t, status(200, `{}`))
	if bl, err := cl.Backlinks(ctx, Caller{Tenant: "t1"}, "r", 5); err != nil || len(bl) != 0 {
		t.Fatalf("%v %v", bl, err)
	}
	cl, _ = platform(t, status(404, `{"error":"x"}`))
	if _, err := cl.Backlinks(ctx, Caller{Tenant: "t1"}, "r", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	cl, _ = platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderTenant, "other")
		_, _ = io.WriteString(w, `{"items":[{"ref":"x"}]}`)
	})
	if _, err := cl.Backlinks(ctx, Caller{Tenant: "t1"}, "r", 5); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other tenant: %v", err)
	}
}
