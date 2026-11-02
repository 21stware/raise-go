package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/21stware/raise-go/blobs"
	"github.com/21stware/raise-go/k3"
	"github.com/21stware/raise-go/provide"
	rt "github.com/21stware/raise-go/realtime"
)

// ---- Deps.Files ----

// memBlobs is a BlobStore over a map that records the keys it was asked for.
type memBlobs struct {
	mu       sync.Mutex
	objs     map[string][]byte
	types    map[string]string
	deleted  []string
	prefixes []string
	getErr   error
}

func newMemBlobs() *memBlobs { return &memBlobs{objs: map[string][]byte{}, types: map[string]string{}} }

func (m *memBlobs) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	b, _ := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key], m.types[key] = b, contentType
	return nil
}

func (m *memBlobs) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, ObjectInfo{}, m.getErr
	}
	b, ok := m.objs[key]
	if !ok {
		return nil, ObjectInfo{}, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(b)), ObjectInfo{Key: key, Size: int64(len(b)), ContentType: m.types[key]}, nil
}

func (m *memBlobs) Head(context.Context, string) (ObjectInfo, error) { return ObjectInfo{}, nil }
func (m *memBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, key)
	return nil
}
func (m *memBlobs) List(context.Context, string) ([]ObjectInfo, error) { return nil, nil }
func (m *memBlobs) Copy(context.Context, string, string) error         { return nil }
func (m *memBlobs) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prefixes = append(m.prefixes, prefix)
	return nil
}
func (m *memBlobs) PresignUpload(context.Context, string, int64, time.Duration) (string, error) {
	return "", nil
}
func (m *memBlobs) PresignDownload(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func TestFilesPrefix(t *testing.T) {
	if FilesPrefix("wiki") != "apps/wiki/" {
		t.Fatal(FilesPrefix("wiki"))
	}
}

func TestDepsFiles(t *testing.T) {
	ctx := context.Background()
	mb := newMemBlobs()
	var asked []string
	d := Deps{Blobs: func(tenant string) BlobStore { asked = append(asked, tenant); return mb }}
	f := d.Files("wiki")

	if err := f.Ready(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := f.Put(ctx, "t1", "img/a.png", strings.NewReader("png"), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if got := string(mb.objs["apps/wiki/img/a.png"]); got != "png" || mb.types["apps/wiki/img/a.png"] != "image/png" {
		t.Fatalf("objects %v", mb.objs)
	}
	if len(mb.objs) != 1 {
		t.Fatal("only the key under the app's folder is written")
	}
	rc, info, err := f.Get(ctx, "t1", "img/a.png")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "png" || info.Size != 3 || info.ContentType != "image/png" {
		t.Fatalf("%q %+v", b, info)
	}
	if err := f.Delete(ctx, "t1", "img/a.png"); err != nil || len(mb.deleted) != 1 || mb.deleted[0] != "apps/wiki/img/a.png" {
		t.Fatalf("delete %v %v", err, mb.deleted)
	}
	if err := f.DeletePrefix(ctx, "t1", "img/"); err != nil || len(mb.prefixes) != 1 || mb.prefixes[0] != "apps/wiki/img/" {
		t.Fatalf("delete prefix %v %v", err, mb.prefixes)
	}
	for _, tn := range asked {
		if tn != "t1" {
			t.Fatalf("asked for the store of %q", tn)
		}
	}

	// another app's folder is another prefix
	if err := d.Files("doc").Put(ctx, "t1", "x", strings.NewReader("y"), 1, ""); err != nil || mb.objs["apps/doc/x"] == nil {
		t.Fatalf("%v %v", err, mb.objs)
	}
}

func TestDepsFilesNotFound(t *testing.T) {
	ctx := context.Background()
	mb := newMemBlobs()
	f := Deps{Blobs: func(string) BlobStore { return mb }}.Files("wiki")

	_, _, err := f.Get(ctx, "t1", "missing")
	if !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("a missing object is blobs.ErrNotFound: %v", err)
	}
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("the cause stays reachable: %v", err)
	}

	// the platform's plain not-found maps the same way
	mb.getErr = ErrNotFound
	if _, _, err := f.Get(ctx, "t1", "missing"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("ErrNotFound: %v", err)
	}

	// anything else is passed through
	boom := errors.New("boom")
	mb.getErr = boom
	if _, _, err := f.Get(ctx, "t1", "x"); !errors.Is(err, boom) || errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("other error: %v", err)
	}
}

func TestDepsFilesUnavailable(t *testing.T) {
	ctx := context.Background()
	for name, d := range map[string]Deps{
		"no Blobs":        {},
		"Blobs gives nil": {Blobs: func(string) BlobStore { return nil }},
	} {
		f := d.Files("wiki")
		if err := f.Ready(ctx, "t1"); !errors.Is(err, blobs.ErrUnavailable) {
			t.Fatalf("%s: Ready %v", name, err)
		}
		if err := f.Put(ctx, "t1", "k", strings.NewReader(""), 0, ""); !errors.Is(err, blobs.ErrUnavailable) {
			t.Fatalf("%s: Put %v", name, err)
		}
		if _, _, err := f.Get(ctx, "t1", "k"); !errors.Is(err, blobs.ErrUnavailable) {
			t.Fatalf("%s: Get %v", name, err)
		}
		if err := f.Delete(ctx, "t1", "k"); !errors.Is(err, blobs.ErrUnavailable) {
			t.Fatalf("%s: Delete %v", name, err)
		}
		if err := f.DeletePrefix(ctx, "t1", "k"); !errors.Is(err, blobs.ErrUnavailable) {
			t.Fatalf("%s: DeletePrefix %v", name, err)
		}
	}
	// no tenant: unavailable, and Blobs is not asked
	called := false
	f := Deps{Blobs: func(string) BlobStore { called = true; return newMemBlobs() }}.Files("wiki")
	if err := f.Ready(ctx, ""); !errors.Is(err, blobs.ErrUnavailable) || called {
		t.Fatalf("no tenant: %v called=%v", err, called)
	}
}

func TestDepsFilesOverClient(t *testing.T) {
	// the whole way: Files over Client.Blobs puts under apps/{app}/ and maps the platform's answers
	cl, s := platform(t, status(204, ""))
	f := Deps{Blobs: cl.Blobs}.Files("wiki")
	if err := f.Put(context.Background(), "t1", "a.txt", strings.NewReader("abc"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if s.method != "PUT" || s.path != "/app/v1/blobs/objects/apps/wiki/a.txt" || s.tenant != "t1" || s.body != "abc" {
		t.Fatalf("%+v", s)
	}
	cl, _ = platform(t, status(404, `{"error":{"code":"not_found","message":"no such object"}}`))
	f = Deps{Blobs: cl.Blobs}.Files("wiki")
	if _, _, err := f.Get(context.Background(), "t1", "a.txt"); !errors.Is(err, blobs.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// a 413 on a write is what StoreWriteError turns into the refusal to show
	cl, _ = platform(t, status(413, `{"error":{"message":"storage limit 1 GB reached"}}`))
	f = Deps{Blobs: cl.Blobs}.Files("wiki")
	err := f.Put(context.Background(), "t1", "a.txt", strings.NewReader("abc"), 3, "text/plain")
	var he *HTTPError
	if err == nil || !errors.As(StoreWriteError(err, errors.New("later")), &he) || he.Status != 413 || !strings.Contains(he.Message, "storage limit") {
		t.Fatalf("%v -> %v", err, StoreWriteError(err, errors.New("later")))
	}
}

func TestStoreWriteError(t *testing.T) {
	fallback := errors.New("try again later")

	got := StoreWriteError(Status(413, "tenant storage is full (5 GB)"), fallback)
	var he *HTTPError
	if !errors.As(got, &he) || he.Status != http.StatusRequestEntityTooLarge || he.Message != "tenant storage is full (5 GB)" {
		t.Fatalf("413: %#v", got)
	}
	if errors.Is(got, fallback) {
		t.Fatal("a 413 is not the fallback")
	}
	// wrapped
	got = StoreWriteError(fmt.Errorf("put: %w", Status(413, "full")), fallback)
	if !errors.As(got, &he) || he.Status != 413 || he.Message != "full" {
		t.Fatalf("wrapped 413: %#v", got)
	}
	// anything else is the fallback
	for name, err := range map[string]error{
		"500":     Status(500, "x"),
		"404":     Status(404, "x"),
		"plain":   errors.New("x"),
		"nil":     nil,
		"missing": ErrNotFound,
	} {
		if got := StoreWriteError(err, fallback); got != fallback {
			t.Fatalf("%s: %v", name, got)
		}
	}
}

func TestDepsBacklinksOf(t *testing.T) {
	ctx := context.Background()
	if got, err := (Deps{}).BacklinksOf(ctx, Caller{Tenant: "t1"}, "r", 5, "wiki"); got != nil || err != nil {
		t.Fatalf("no BacklinksAPI: %v %v", got, err)
	}
	var gotC Caller
	var gotRef string
	var gotLimit int
	d := Deps{BacklinksAPI: func(_ context.Context, c Caller, ref string, limit int) ([]Backlink, error) {
		gotC, gotRef, gotLimit = c, ref, limit
		return []Backlink{{Ref: "k3://notes/note/1", App: "notes"}}, nil
	}}
	c := Caller{Tenant: "t1", User: "u1"}
	got, err := d.BacklinksOf(ctx, c, "k3://wiki/page/1", 7, "wiki")
	if err != nil || len(got) != 1 || got[0].App != "notes" || gotC.User != "u1" || gotRef != "k3://wiki/page/1" || gotLimit != 7 {
		t.Fatalf("%v %v %+v %q %d", got, err, gotC, gotRef, gotLimit)
	}
	boom := errors.New("boom")
	d.BacklinksAPI = func(context.Context, Caller, string, int) ([]Backlink, error) { return nil, boom }
	if _, err := d.BacklinksOf(ctx, c, "r", 1, ""); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

// ---- TenantChanges ----

func TestTenantChanges(t *testing.T) {
	var h TenantChanges
	var mu sync.Mutex
	var log []string
	hook := func(name string) func(context.Context, string) {
		return func(ctx context.Context, tenant string) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("a hook runs with a bounded context")
			}
			mu.Lock()
			log = append(log, name+":"+tenant)
			mu.Unlock()
		}
	}
	h.OnInstall(hook("i1"))
	h.OnInstall(hook("i2"))
	h.OnInstall(nil) // ignored
	h.OnPermissions(hook("p1"))

	h.Installs("t1")
	h.Wait()
	mu.Lock()
	got := strings.Join(sortedCopy(log), ",")
	log = nil
	mu.Unlock()
	if got != "i1:t1,i2:t1" {
		t.Fatalf("installs ran %q", got)
	}

	h.Permissions("t2")
	h.Wait()
	mu.Lock()
	got = strings.Join(log, ",")
	log = nil
	mu.Unlock()
	if got != "p1:t2" {
		t.Fatalf("permissions ran %q", got)
	}

	// no tenant: nothing runs
	h.Installs("")
	h.Permissions("")
	h.Wait()
	mu.Lock()
	n := len(log)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("hooks ran for no tenant: %v", log)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestTenantChangesReturnsAtOnce(t *testing.T) {
	var h TenantChanges
	release := make(chan struct{})
	started := make(chan struct{})
	h.OnInstall(func(context.Context, string) { close(started); <-release })
	done := make(chan struct{})
	go func() { h.Installs("t1"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Installs waited for the hook")
	}
	<-started
	waited := make(chan struct{})
	go func() { h.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned while a hook ran")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after the hook did")
	}
}

func TestTenantChangesNilSafe(t *testing.T) {
	var h *TenantChanges
	h.OnInstall(func(context.Context, string) { t.Error("ran") })
	h.OnPermissions(func(context.Context, string) { t.Error("ran") })
	h.Installs("t1")
	h.Permissions("t1")
	h.Wait()
}

func TestTenantChangesHookRegisteredLaterIsHeard(t *testing.T) {
	var h TenantChanges
	var n atomic.Int32
	h.Installs("t1") // no hooks yet
	h.OnInstall(func(context.Context, string) { n.Add(1) })
	h.Installs("t1")
	h.Wait()
	if n.Load() != 1 {
		t.Fatalf("ran %d times", n.Load())
	}
}

// ---- Serve: the knowledge providers and the platform events ----

const kbApp = "kb"

// kbAppImpl is an app of knowledge: it implements every interface serveKnowledge and servePlatformEvents look for; what is
// served depends on its manifest.
type kbAppImpl struct {
	manifest *k3.Manifest

	retr  *fakeRetriever
	bl    provide.BacklinkProvider
	emb   *fakeEmbeddings
	projC chan projCall
	projE error
}

type projCall struct {
	tenant string
	ev     rt.Projected
}

func (a *kbAppImpl) ID() string { return kbApp }
func (a *kbAppImpl) Manifest(string, string) *k3.Manifest {
	return a.manifest
}
func (a *kbAppImpl) Handler(Deps) http.Handler { return http.NotFoundHandler() }

func (a *kbAppImpl) Retriever(Deps) provide.Retriever { return a.retr }
func (a *kbAppImpl) BacklinkProvider(Deps) provide.BacklinkProvider {
	return a.bl
}
func (a *kbAppImpl) Embeddings(Deps) provide.Embeddings { return a.emb }
func (a *kbAppImpl) RealtimeProjected(_ context.Context, _ Deps, tenant string, ev rt.Projected) error {
	if a.projC != nil {
		a.projC <- projCall{tenant, ev}
	}
	return a.projE
}

type fakeRetriever struct {
	mu     sync.Mutex
	caller Caller
	query  provide.RetrieveQuery
	out    []provide.Passage
	err    error
}

func (f *fakeRetriever) Retrieve(_ context.Context, c Caller, q provide.RetrieveQuery) ([]provide.Passage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caller, f.query = c, q
	return f.out, f.err
}
func (f *fakeRetriever) Modes() []string { return []string{provide.ModeKeyword} }

type fakeBacklinks struct {
	mu     sync.Mutex
	caller Caller
	ref    string
	limit  int
	out    []Backlink
	err    error
}

func (f *fakeBacklinks) Backlinks(_ context.Context, c Caller, ref string, limit int) ([]Backlink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caller, f.ref, f.limit = c, ref, limit
	return f.out, f.err
}

// countingBacklinks also keeps its own count.
type countingBacklinks struct {
	fakeBacklinks
	tenant, ref string
	n           int
	err         error
}

func (f *countingBacklinks) CountBacklinks(_ context.Context, tenant, ref string) (int, error) {
	f.tenant, f.ref = tenant, ref
	return f.n, f.err
}

type fakeEmbeddings struct {
	mu     sync.Mutex
	tenant string
	after  string
	limit  int
	refs   []string
	docs   []provide.EmbeddingDoc
	next   string
}

func (f *fakeEmbeddings) EmbeddingDocs(_ context.Context, tenant, after string, limit int) ([]provide.EmbeddingDoc, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenant, f.after, f.limit = tenant, after, limit
	return f.docs, f.next, nil
}

func (f *fakeEmbeddings) EmbeddingChunks(_ context.Context, tenant string, refs []string) ([]provide.EmbeddingChunks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenant, f.refs = tenant, refs
	return nil, nil
}

func kbManifest(p *k3.ProvidersDecl, services ...k3.Service) *k3.Manifest {
	return &k3.Manifest{ID: kbApp, Providers: p, Services: services}
}

func eventService(kind string) k3.Service {
	return k3.Service{ID: "on-" + kind, Trigger: []string{k3.TriggerEvent + kind}, Endpoint: "/k3/services/" + kind}
}

// platformSrv serves a through Serve with signed calls; call posts body as the platform (via, tenant, sub).
type platformSrv struct {
	t      *testing.T
	srv    *httptest.Server
	signer *k3.Signer
}

func newPlatformSrv(t *testing.T, a App, d Deps) *platformSrv {
	t.Helper()
	signer, err := k3.NewSigner("kid-1")
	if err != nil {
		t.Fatal(err)
	}
	h, err := Serve(a, d, signer.JWKS(), "1.0.0", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &platformSrv{t: t, srv: srv, signer: signer}
}

// assertion signs for input (the body of a provider call, or a service run's input).
func (p *platformSrv) assertion(app, via, tenant, sub string, input []byte) string {
	p.t.Helper()
	dig, err := k3.InputDigest(input)
	if err != nil {
		p.t.Fatal(err)
	}
	now := time.Now()
	tok, err := p.signer.Sign(k3.TypAssertion, k3.AssertionClaims{
		Registered:  k3.Registered{Aud: k3.Audience{app}, Exp: now.Add(time.Minute).Unix(), Iat: now.Unix()},
		Tenant:      tenant,
		Via:         via,
		InputDigest: dig,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	_ = sub
	return tok
}

// call posts as the platform. sub names the person behind a provider call ("" none). The assertion's subject is set through the
// claims below, so build them here.
func (p *platformSrv) call(path, via, tenant, sub, body string) (int, string) {
	p.t.Helper()
	input := []byte(body)
	if via == k3.ViaService {
		var run struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(input, &run); err != nil {
			p.t.Fatal(err)
		}
		input = run.Input
	}
	dig, err := k3.InputDigest(input)
	if err != nil {
		p.t.Fatal(err)
	}
	now := time.Now()
	tok, err := p.signer.Sign(k3.TypAssertion, k3.AssertionClaims{
		Registered:  k3.Registered{Sub: sub, Aud: k3.Audience{kbApp}, Exp: now.Add(time.Minute).Unix(), Iat: now.Unix()},
		Tenant:      tenant,
		Via:         via,
		Name:        "Name of " + sub,
		Roles:       []string{"member"},
		InputDigest: dig,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return p.post(path, tok, body)
}

func (p *platformSrv) post(path, token, body string) (int, string) {
	p.t.Helper()
	req, _ := http.NewRequest("POST", p.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(k3.HeaderAssertion, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServeRetrieve(t *testing.T) {
	retr := &fakeRetriever{out: []provide.Passage{{Ref: "k3://kb/page/1", App: kbApp, Title: "T", Text: "hello", Score: 0.5}}}
	app := &kbAppImpl{manifest: kbManifest(&k3.ProvidersDecl{Retrieve: true}), retr: retr}
	p := newPlatformSrv(t, app, Deps{})

	code, body := p.call(PathRetrieve, k3.ViaProvider, "t1", "u1", `{"query":{"Text":"hello","Limit":3,"Scope":{"space":"s1"}}}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Passages []provide.Passage `json:"passages"`
		Modes    []string          `json:"modes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Passages) != 1 || out.Passages[0].Title != "T" || len(out.Modes) != 1 || out.Modes[0] != "keyword" {
		t.Fatalf("%s %v", body, err)
	}
	if retr.caller.User != "u1" || retr.caller.Tenant != "t1" || retr.query.Text != "hello" || retr.query.Limit != 3 || retr.query.Scope["space"] != "s1" {
		t.Fatalf("retriever saw %+v %+v", retr.caller, retr.query)
	}

	// no passages: an empty list, not null
	retr.out = nil
	_, body = p.call(PathRetrieve, k3.ViaProvider, "t1", "u1", `{"query":{"Text":"x"}}`)
	if !strings.Contains(body, `"passages":[]`) {
		t.Fatalf("%s", body)
	}

	// a person is required: no subject, or the platform's own
	for _, sub := range []string{"", "platform"} {
		if code, _ := p.call(PathRetrieve, k3.ViaProvider, "t1", sub, `{"query":{"Text":"x"}}`); code != 403 {
			t.Fatalf("sub %q: %d", sub, code)
		}
	}
	// only the platform: no assertion, or one of another kind
	if code, _ := p.post(PathRetrieve, "", `{"query":{}}`); code != 401 {
		t.Fatalf("no assertion: %d", code)
	}
	if code, _ := p.call(PathRetrieve, k3.ViaEvent, "t1", "u1", `{"query":{}}`); code != 401 {
		t.Fatalf("via event: %d", code)
	}
	// an assertion signed for another body is refused
	tok := p.assertion(kbApp, k3.ViaProvider, "t1", "u1", []byte(`{"query":{"Text":"a"}}`))
	if code, _ := p.post(PathRetrieve, tok, `{"query":{"Text":"b"}}`); code != 401 {
		t.Fatalf("another body: %d", code)
	}
	// an error of the app's own words
	retr.err = Invalid("query too long")
	if code, _ := p.call(PathRetrieve, k3.ViaProvider, "t1", "u1", `{"query":{"Text":"x"}}`); code != 422 {
		t.Fatalf("invalid: %d", code)
	}
	retr.err = errors.New("db down")
	if code, body := p.call(PathRetrieve, k3.ViaProvider, "t1", "u1", `{"query":{"Text":"x"}}`); code != 500 || strings.Contains(body, "db down") {
		t.Fatalf("failure: %d %s", code, body)
	}
}

func TestServeBacklinks(t *testing.T) {
	bl := &fakeBacklinks{out: []Backlink{{Ref: "k3://kb/page/2", App: kbApp, Kind: "page", Title: "Two"}}}
	app := &kbAppImpl{manifest: kbManifest(&k3.ProvidersDecl{Backlinks: true}), bl: bl}
	p := newPlatformSrv(t, app, Deps{})

	code, body := p.call(PathBacklinks, k3.ViaProvider, "t1", "u1", `{"ref":"k3://kb/page/1","limit":10}`)
	if code != 200 || !strings.Contains(body, `"Two"`) {
		t.Fatalf("%d %s", code, body)
	}
	if bl.caller.User != "u1" || bl.caller.Tenant != "t1" || bl.ref != "k3://kb/page/1" || bl.limit != 10 {
		t.Fatalf("saw %+v %q %d", bl.caller, bl.ref, bl.limit)
	}
	bl.out = nil
	if _, body := p.call(PathBacklinks, k3.ViaProvider, "t1", "u1", `{"ref":"r"}`); !strings.Contains(body, `"items":[]`) {
		t.Fatalf("%s", body)
	}
	// for a person only
	if code, _ := p.call(PathBacklinks, k3.ViaProvider, "t1", "", `{"ref":"r"}`); code != 403 {
		t.Fatalf("no person: %d", code)
	}

	// the count for the platform, from the provider's Backlinks as a tenant administrator when it keeps no count
	bl.out = []Backlink{{Ref: "a"}, {Ref: "b"}, {Ref: "c"}}
	code, body = p.call(PathBacklinksCount, k3.ViaProvider, "t1", "", `{"ref":"k3://kb/page/1"}`)
	if code != 200 || strings.TrimSpace(body) != `{"count":3}` {
		t.Fatalf("%d %s", code, body)
	}
	if bl.caller.User != "system:refcheck" || bl.caller.Tenant != "t1" || bl.limit != 100 || !containsString(bl.caller.Roles, RoleTenantAdmin) {
		t.Fatalf("count asked as %+v limit %d", bl.caller, bl.limit)
	}
	// the count is the platform's, not a person's call: a person's subject is fine too, but no assertion is not
	if code, _ := p.post(PathBacklinksCount, "", `{"ref":"r"}`); code != 401 {
		t.Fatalf("no assertion: %d", code)
	}
}

func TestServeBacklinksCountUsesCounter(t *testing.T) {
	cb := &countingBacklinks{n: 42}
	cb.fakeBacklinks.out = []Backlink{{Ref: "only-one"}}
	app := &kbAppImpl{manifest: kbManifest(&k3.ProvidersDecl{Backlinks: true}), bl: cb}
	p := newPlatformSrv(t, app, Deps{})

	code, body := p.call(PathBacklinksCount, k3.ViaProvider, "t1", "", `{"ref":"k3://kb/page/1"}`)
	if code != 200 || strings.TrimSpace(body) != `{"count":42}` {
		t.Fatalf("%d %s", code, body)
	}
	if cb.tenant != "t1" || cb.ref != "k3://kb/page/1" {
		t.Fatalf("counter saw %q %q", cb.tenant, cb.ref)
	}
	if cb.fakeBacklinks.ref != "" {
		t.Fatal("Backlinks must not be called when the provider counts itself")
	}
	cb.err = errors.New("down")
	if code, _ := p.call(PathBacklinksCount, k3.ViaProvider, "t1", "", `{"ref":"r"}`); code != 500 {
		t.Fatalf("%d", code)
	}
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestServeEmbeddings(t *testing.T) {
	emb := &fakeEmbeddings{docs: []provide.EmbeddingDoc{{Ref: "k3://kb/page/1", Hash: "h1"}}, next: "k3://kb/page/1"}
	app := &kbAppImpl{manifest: kbManifest(&k3.ProvidersDecl{Embeddings: true}), emb: emb}
	p := newPlatformSrv(t, app, Deps{})

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"after":"a","limit":20}`, 20},
		{`{"limit":0}`, EmbeddingPage},
		{`{}`, EmbeddingPage},
		{`{"limit":-5}`, EmbeddingPage},
		{`{"limit":501}`, EmbeddingPage},
		{`{"limit":100000}`, EmbeddingPage},
		{`{"limit":500}`, 500},
		{`{"limit":1}`, 1},
	} {
		code, body := p.call(PathEmbeddingDocs, k3.ViaProvider, "t1", "", tc.body)
		if code != 200 {
			t.Fatalf("%s: %d %s", tc.body, code, body)
		}
		if emb.limit != tc.want || emb.tenant != "t1" {
			t.Fatalf("%s: limit %d (want %d), tenant %q", tc.body, emb.limit, tc.want, emb.tenant)
		}
	}
	code, body := p.call(PathEmbeddingDocs, k3.ViaProvider, "t1", "", `{"after":"k3://kb/page/0","limit":5}`)
	var out struct {
		Docs []provide.EmbeddingDoc `json:"docs"`
		Next string                 `json:"next"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil || len(out.Docs) != 1 || out.Docs[0].Hash != "h1" || out.Next != "k3://kb/page/1" || emb.after != "k3://kb/page/0" {
		t.Fatalf("%d %s after=%q", code, body, emb.after)
	}
	emb.docs = nil
	if _, body := p.call(PathEmbeddingDocs, k3.ViaProvider, "t1", "", `{}`); !strings.Contains(body, `"docs":[]`) {
		t.Fatalf("%s", body)
	}

	if code, body := p.call(PathEmbeddingChunks, k3.ViaProvider, "t2", "", `{"refs":["a","b"]}`); code != 200 || !strings.Contains(body, `"docs":[]`) || emb.tenant != "t2" || len(emb.refs) != 2 {
		t.Fatalf("%d %s %q %v", code, body, emb.tenant, emb.refs)
	}
	if code, _ := p.post(PathEmbeddingDocs, "", `{}`); code != 401 {
		t.Fatalf("no assertion: %d", code)
	}
}

func TestServeUndeclaredProvidersAreNotServed(t *testing.T) {
	retr := &fakeRetriever{}
	bl := &fakeBacklinks{}
	emb := &fakeEmbeddings{}
	// the app implements everything, the manifest declares none (and only embeddings in the second round)
	for _, decl := range []*k3.ProvidersDecl{nil, {}, {Embeddings: true}} {
		app := &kbAppImpl{manifest: kbManifest(decl), retr: retr, bl: bl, emb: emb}
		p := newPlatformSrv(t, app, Deps{})
		for path, kind := range map[string]string{
			PathRetrieve: "retrieve", PathBacklinks: "backlinks", PathBacklinksCount: "backlinks",
			PathEmbeddingDocs: "embeddings", PathEmbeddingChunks: "embeddings",
		} {
			code, _ := p.call(path, k3.ViaProvider, "t1", "u1", `{}`)
			want := 404
			if decl != nil && decl.Has(kind) {
				want = 200
			}
			if code != want {
				t.Fatalf("decl %+v %s: %d, want %d", decl, path, code, want)
			}
		}
	}
	if retr.caller.User != "" || bl.ref != "" {
		t.Fatal("an undeclared provider was called")
	}
}

func TestServeNilProviderIsNotServed(t *testing.T) {
	// the app implements the interface but gives no provider (a nil): nothing is mounted
	app := &kbAppImpl{manifest: kbManifest(&k3.ProvidersDecl{Backlinks: true})}
	app.bl = nil
	p := newPlatformSrv(t, app, Deps{})
	for _, path := range []string{PathBacklinks, PathBacklinksCount} {
		if code, _ := p.call(path, k3.ViaProvider, "t1", "u1", `{}`); code != 404 {
			t.Fatalf("%s: %d", path, code)
		}
	}
}

func runBody(tenant, typ, data string) string {
	return fmt.Sprintf(`{"input":{"id":"ev-1","type":%q,"tenant":%q,"data":%s}}`, typ, tenant, data)
}

func TestServeRealtimeProjected(t *testing.T) {
	app := &kbAppImpl{
		manifest: kbManifest(nil, eventService(k3.EventRealtimeProjected)),
		projC:    make(chan projCall, 4),
	}
	p := newPlatformSrv(t, app, Deps{})
	path := "/k3/services/" + k3.EventRealtimeProjected

	code, body := p.call(path, k3.ViaService, "t1", "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1","kind":"page","seq":12,"actors":["u1","u2"]}`))
	if code != 200 || strings.TrimSpace(body) != `{"ok":true}` {
		t.Fatalf("%d %s", code, body)
	}
	select {
	case c := <-app.projC:
		if c.tenant != "t1" || c.ev.Doc != "d1" || c.ev.Kind != "page" || c.ev.Seq != 12 || len(c.ev.Actors) != 2 || c.ev.Actors[1] != "u2" {
			t.Fatalf("%+v", c)
		}
	default:
		t.Fatal("RealtimeProjected was not called")
	}

	// not that event: refused for good (422), never handed to the app
	for name, data := range map[string]string{"no doc": `{"kind":"page","seq":1}`, "not an object": `"x"`, "empty doc": `{"doc":""}`} {
		if code, _ := p.call(path, k3.ViaService, "t1", "", runBody("t1", k3.EventRealtimeProjected, data)); code != 422 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	if len(app.projC) != 0 {
		t.Fatal("a malformed event reached the app")
	}

	// the run names another tenant than the assertion
	if code, _ := p.call(path, k3.ViaService, "t1", "", runBody("t2", k3.EventRealtimeProjected, `{"doc":"d1"}`)); code != 403 {
		t.Fatalf("another tenant: %d", code)
	}
	// only a service delivery
	if code, _ := p.call(path, k3.ViaProvider, "t1", "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1"}`)); code != 401 {
		t.Fatalf("via provider: %d", code)
	}
	if code, _ := p.post(path, "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1"}`)); code != 401 {
		t.Fatalf("no assertion: %d", code)
	}

	// the app's answer: a refusal is 422 (the platform does not retry), anything else 500 (it does)
	app.projE = Invalid("not mine")
	if code, _ := p.call(path, k3.ViaService, "t1", "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1"}`)); code != 422 {
		t.Fatalf("refusal: %d", code)
	}
	app.projE = errors.New("db down")
	if code, body := p.call(path, k3.ViaService, "t1", "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1"}`)); code != 500 || strings.Contains(body, "db down") {
		t.Fatalf("failure: %d %s", code, body)
	}
}

func TestServeRealtimeProjectedNeedsDeclaredService(t *testing.T) {
	app := &kbAppImpl{manifest: kbManifest(nil), projC: make(chan projCall, 1)}
	p := newPlatformSrv(t, app, Deps{})
	if code, _ := p.call("/k3/services/"+k3.EventRealtimeProjected, k3.ViaService, "t1", "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1"}`)); code != 404 {
		t.Fatalf("%d", code)
	}
	if len(app.projC) != 0 {
		t.Fatal("called")
	}
}

func TestServeInstalledAndPermissionsFeedChanges(t *testing.T) {
	app := &kbAppImpl{manifest: kbManifest(nil, eventService(EventAppInstalled), eventService(EventPermissionsChanged))}
	changes := &TenantChanges{}
	installed := make(chan string, 4)
	perms := make(chan string, 4)
	changes.OnInstall(func(_ context.Context, tenant string) { installed <- tenant })
	changes.OnPermissions(func(_ context.Context, tenant string) { perms <- tenant })
	p := newPlatformSrv(t, app, Deps{Changes: changes})

	if code, body := p.call("/k3/services/"+EventAppInstalled, k3.ViaService, "t1", "", runBody("t1", EventAppInstalled, `{}`)); code != 200 || strings.TrimSpace(body) != `{"ok":true}` {
		t.Fatalf("%d %s", code, body)
	}
	changes.Wait()
	select {
	case tn := <-installed:
		if tn != "t1" {
			t.Fatal(tn)
		}
	default:
		t.Fatal("the install hook did not run")
	}
	if len(perms) != 0 {
		t.Fatal("an install ran the permission hooks")
	}

	if code, _ := p.call("/k3/services/"+EventPermissionsChanged, k3.ViaService, "t2", "", runBody("t2", EventPermissionsChanged, `{}`)); code != 200 {
		t.Fatalf("%d", code)
	}
	changes.Wait()
	select {
	case tn := <-perms:
		if tn != "t2" {
			t.Fatal(tn)
		}
	default:
		t.Fatal("the permission hook did not run")
	}
	if len(installed) != 0 {
		t.Fatal("a permission change ran the install hooks")
	}

	// another tenant in the run than in the assertion: refused, no hook
	if code, _ := p.call("/k3/services/"+EventAppInstalled, k3.ViaService, "t1", "", runBody("t9", EventAppInstalled, `{}`)); code != 403 {
		t.Fatalf("%d", code)
	}
	changes.Wait()
	if len(installed) != 0 {
		t.Fatal("a refused call ran a hook")
	}
}

func TestServeInstalledNeedsChanges(t *testing.T) {
	// without Deps.Changes the services are not mounted
	app := &kbAppImpl{manifest: kbManifest(nil, eventService(EventAppInstalled))}
	p := newPlatformSrv(t, app, Deps{})
	if code, _ := p.call("/k3/services/"+EventAppInstalled, k3.ViaService, "t1", "", runBody("t1", EventAppInstalled, `{}`)); code != 404 {
		t.Fatalf("%d", code)
	}
	// with it but undeclared: not mounted either
	app = &kbAppImpl{manifest: kbManifest(nil)}
	p = newPlatformSrv(t, app, Deps{Changes: &TenantChanges{}})
	if code, _ := p.call("/k3/services/"+EventAppInstalled, k3.ViaService, "t1", "", runBody("t1", EventAppInstalled, `{}`)); code != 404 {
		t.Fatalf("%d", code)
	}
}

// ---- the outbox's handler registry (the delivery itself needs the app's database) ----

func TestRegisterOutboxHandler(t *testing.T) {
	const kind = "test.registry.kind"
	t.Cleanup(func() { extMu.Lock(); delete(extHandlers, kind); extMu.Unlock() })
	if _, ok := extHandler(kind); ok {
		t.Fatal("registered before")
	}
	RegisterOutboxHandler(kind, func(context.Context, string, time.Time, OutboxRow) OutboxResult {
		return OutboxResult{State: OutDone, Result: "first"}
	})
	h, ok := extHandler(kind)
	if !ok || h(context.Background(), "t1", time.Now(), OutboxRow{}).Result != "first" {
		t.Fatal("first handler")
	}
	// a later registration of the same kind replaces the earlier
	RegisterOutboxHandler(kind, func(_ context.Context, tenant string, _ time.Time, row OutboxRow) OutboxResult {
		return OutboxResult{State: OutSkipped, Result: tenant + "/" + row.Key}
	})
	h, _ = extHandler(kind)
	if res := h(context.Background(), "t1", time.Now(), OutboxRow{Key: "k"}); res.State != OutSkipped || res.Result != "t1/k" {
		t.Fatalf("%+v", res)
	}
	if _, ok := extHandler(kind + ".other"); ok {
		t.Fatal("another kind")
	}
}

// kbWithOwnEvents is a knowledge app that also answers app.installed itself (AppEventHandling).
type kbWithOwnEvents struct {
	*kbAppImpl
	got chan string
}

func (a kbWithOwnEvents) AppEvents(Deps) map[string]AppEventFunc {
	return map[string]AppEventFunc{"on-" + EventAppInstalled: func(_ context.Context, ev AppEvent) error { a.got <- ev.Tenant; return nil }}
}

func TestServeEveryKnowledgeProviderAndServiceAtOnce(t *testing.T) {
	// retrieve and embeddings are serveKnowledge's, backlinks serveKinds', the three services servePlatformEvents': one mux, no
	// pattern twice (http.ServeMux panics on a duplicate, which Serve must never do).
	decl := &k3.ProvidersDecl{Retrieve: true, Backlinks: true, Embeddings: true}
	app := &kbAppImpl{
		manifest: kbManifest(decl, eventService(EventAppInstalled), eventService(EventPermissionsChanged), eventService(k3.EventRealtimeProjected)),
		retr:     &fakeRetriever{},
		bl:       &fakeBacklinks{},
		emb:      &fakeEmbeddings{},
		projC:    make(chan projCall, 1),
	}
	changes := &TenantChanges{}
	p := newPlatformSrv(t, app, Deps{Changes: changes})

	for _, c := range []struct{ path, via, sub, body string }{
		{PathRetrieve, k3.ViaProvider, "u1", `{"query":{"Text":"x"}}`},
		{PathBacklinks, k3.ViaProvider, "u1", `{"ref":"k3://kb/page/1","limit":5}`},
		{PathBacklinksCount, k3.ViaProvider, "", `{"ref":"k3://kb/page/1"}`},
		{PathEmbeddingDocs, k3.ViaProvider, "", `{"after":"","limit":10}`},
		{PathEmbeddingChunks, k3.ViaProvider, "", `{"refs":["k3://kb/page/1"]}`},
		{"/k3/services/" + EventAppInstalled, k3.ViaService, "", runBody("t1", EventAppInstalled, `{}`)},
		{"/k3/services/" + EventPermissionsChanged, k3.ViaService, "", runBody("t1", EventPermissionsChanged, `{}`)},
		{"/k3/services/" + k3.EventRealtimeProjected, k3.ViaService, "", runBody("t1", k3.EventRealtimeProjected, `{"doc":"d1"}`)},
	} {
		if code, body := p.call(c.path, c.via, "t1", c.sub, c.body); code != 200 {
			t.Errorf("%s: %d %s", c.path, code, body)
		}
	}
	changes.Wait()

	// a service the app answers itself (AppEvents) is left to serveAppEvents, not mounted a second time
	own := kbWithOwnEvents{kbAppImpl: &kbAppImpl{manifest: kbManifest(decl, eventService(EventAppInstalled))}, got: make(chan string, 1)}
	p = newPlatformSrv(t, own, Deps{Changes: changes})
	run := `{"trigger":"` + k3.TriggerEvent + EventAppInstalled + `","input":{"id":"ev-1","type":"` + EventAppInstalled + `","tenant":"t1"}}`
	if code, body := p.call("/k3/services/"+EventAppInstalled, k3.ViaService, "t1", "", run); code != 204 {
		t.Fatalf("%d %s", code, body)
	}
	if tn := <-own.got; tn != "t1" {
		t.Fatal(tn)
	}
}
