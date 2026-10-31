package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/21stware/raise-go/k3"
)

type seen struct {
	method, path, query, body, ctype, tenant string
	clen                                     int64
}

// platform plays /app/v1: h answers, the last request is recorded.
func platform(t *testing.T, h http.HandlerFunc) (*Client, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*s = seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b), r.Header.Get("Content-Type"), r.Header.Get(HeaderTenant), r.ContentLength}
		w.Header().Set(HeaderTenant, r.Header.Get(HeaderTenant))
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Token: func(context.Context, Caller) (string, error) { return "tok", nil }}, s
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func TestBlobsGetStreams(t *testing.T) {
	mod := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Last-Modified", mod.Format(http.TimeFormat))
		_, _ = io.WriteString(w, "hello")
	})
	rc, info, err := cl.Blobs("t1").Get(context.Background(), "apps/a/dir x/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "hello" || info.Size != 5 || info.ContentType != "text/plain" || !info.ModTime.Equal(mod) || info.Key != "apps/a/dir x/f.txt" {
		t.Fatalf("got %q %+v", b, info)
	}
	if s.method != "GET" || s.path != "/app/v1/blobs/objects/apps/a/dir%20x/f.txt" || s.tenant != "t1" {
		t.Fatalf("request %+v", s)
	}
}

func TestBlobsPut(t *testing.T) {
	cl, s := platform(t, status(204, ""))
	if err := cl.Blobs("t1").Put(context.Background(), "apps/a/f", strings.NewReader("abc"), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if s.method != "PUT" || s.path != "/app/v1/blobs/objects/apps/a/f" || s.body != "abc" || s.ctype != "image/png" || s.clen != 3 {
		t.Fatalf("request %+v", s)
	}
	// empty object: known length 0, default type
	if err := cl.Blobs("t1").Put(context.Background(), "apps/a/e", strings.NewReader(""), 0, ""); err != nil {
		t.Fatal(err)
	}
	if s.clen != 0 || s.body != "" || s.ctype != "application/octet-stream" {
		t.Fatalf("empty put %+v", s)
	}
}

func TestBlobsMeta(t *testing.T) {
	cl, s := platform(t, status(200, `{"key":"apps/a/f","size":9,"contentType":"x/y","modTime":"2026-10-31T08:00:00Z"}`))
	info, err := cl.Blobs("t1").Head(context.Background(), "apps/a/f")
	if err != nil || info.Size != 9 || info.ContentType != "x/y" || info.ModTime.IsZero() {
		t.Fatalf("%+v %v", info, err)
	}
	if s.method != "GET" || s.path != "/app/v1/blobs/meta/apps/a/f" {
		t.Fatalf("request %+v", s)
	}
}

func TestBlobsListCopyDeletePresign(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"items":[{"key":"apps/a/x","size":1},{"key":"apps/a/y","size":2}]}`))
	got, err := cl.Blobs("t1").List(ctx, "apps/a/")
	if err != nil || len(got) != 2 || got[1].Key != "apps/a/y" || s.method != "GET" || s.path != "/app/v1/blobs" || s.query != "prefix=apps%2Fa%2F" {
		t.Fatalf("list %+v %v %+v", got, err, s)
	}

	cl, s = platform(t, status(204, ""))
	if err := cl.Blobs("t1").Copy(ctx, "apps/a/x", "apps/a/z"); err != nil {
		t.Fatal(err)
	}
	if s.method != "POST" || s.path != "/app/v1/blobs/copy" || s.body != `{"dst":"apps/a/z","src":"apps/a/x"}` {
		t.Fatalf("copy %+v", s)
	}
	if err := cl.Blobs("t1").Delete(ctx, "apps/a/x"); err != nil || s.method != "DELETE" || s.path != "/app/v1/blobs/objects/apps/a/x" {
		t.Fatalf("delete %v %+v", err, s)
	}
	if err := cl.Blobs("t1").DeletePrefix(ctx, "apps/a/"); err != nil || s.path != "/app/v1/blobs/delete-prefix" || s.body != `{"prefix":"apps/a/"}` {
		t.Fatalf("delete-prefix %v %+v", err, s)
	}

	cl, s = platform(t, status(200, `{"url":"http://signed","method":"PUT"}`))
	u, err := cl.Blobs("t1").PresignUpload(ctx, "apps/a/x", 42, 5*time.Minute)
	if err != nil || u != "http://signed" || s.path != "/app/v1/blobs/presign" {
		t.Fatalf("presign %q %v %+v", u, err, s)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(s.body), &body)
	if body["method"] != "PUT" || body["key"] != "apps/a/x" || body["ttlSeconds"] != float64(300) || body["size"] != float64(42) {
		t.Fatalf("presign body %v", body)
	}
	if _, err := cl.Blobs("t1").PresignDownload(ctx, "apps/a/x", 0); err != nil {
		t.Fatal(err)
	}
	body = nil
	_ = json.Unmarshal([]byte(s.body), &body)
	if body["method"] != "GET" || body["ttlSeconds"] != nil {
		t.Fatalf("presign body %v", body)
	}
}

func TestBlobsErrors(t *testing.T) {
	ctx := context.Background()
	cl, _ := platform(t, status(404, `{"error":{"code":"not_found","message":"no such object"}}`))
	b := cl.Blobs("t1")
	if _, _, err := b.Get(ctx, "apps/a/x"); !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("get: %v", err)
	}
	if _, err := b.Head(ctx, "apps/a/x"); !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("head: %v", err)
	}
	if err := b.Copy(ctx, "a", "b"); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("copy: %v", err)
	}
	for code, want := range map[int]error{422: ErrInvalid, 409: ErrConflict, 403: ErrForbidden} {
		cl, _ := platform(t, status(code, `{"error":"no"}`))
		if err := cl.Blobs("t1").Put(ctx, "k", strings.NewReader("x"), 1, ""); !errors.Is(err, want) {
			t.Fatalf("put %d: %v", code, err)
		}
	}
	var he *HTTPError
	cl, _ = platform(t, status(500, `{"error":"boom"}`))
	if _, _, err := cl.Blobs("t1").Get(ctx, "k"); !errors.As(err, &he) || he.Status != 500 {
		t.Fatalf("500: %v", err)
	}
}

func TestBlobsRefuseOtherTenant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderTenant, "other")
		_, _ = io.WriteString(w, "x")
	}))
	defer srv.Close()
	cl := &Client{Base: srv.URL, Token: func(context.Context, Caller) (string, error) { return "tok", nil }}
	if _, _, err := cl.Blobs("t1").Get(context.Background(), "apps/a/x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestNotify(t *testing.T) {
	cl, s := platform(t, status(204, ""))
	ctx := WithNoticeClass(context.Background(), ClassMention)
	cl.Notify(ctx, "t1", []string{"u1", "u2"}, "T", "B", "/x")
	if s.method != "POST" || s.path != "/app/v1/notify" || s.tenant != "t1" {
		t.Fatalf("request %+v", s)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(s.body), &got)
	if got["class"] != "mention" || got["title"] != "T" || got["body"] != "B" || got["link"] != "/x" || len(got["users"].([]any)) != 2 {
		t.Fatalf("body %v", got)
	}
	cl.Notify(context.Background(), "t1", []string{"u1"}, "T", "", "")
	if strings.Contains(s.body, "class") {
		t.Fatalf("class sent without one: %s", s.body)
	}
	// a refusal is not returned and does not panic
	cl, _ = platform(t, status(403, `{"error":"no"}`))
	cl.Notify(context.Background(), "t1", []string{"u1"}, "T", "", "")
}

func TestTell(t *testing.T) {
	var got [][]string
	d := Deps{Notify: func(_ context.Context, tenant string, users []string, title, body, link string) {
		got = append(got, users)
	}}
	d.Tell(context.Background(), "t1", []string{"a", "", " a ", "b", "  "}, "T", "B", "/l")
	d.Tell(context.Background(), "t1", []string{"", " "}, "T", "B", "/l")
	if len(got) != 1 || strings.Join(got[0], ",") != "a,b" {
		t.Fatalf("got %v", got)
	}
	Deps{}.Tell(context.Background(), "t1", []string{"a"}, "T", "", "") // nil-safe
	if NoticeClass(context.Background()) != "" {
		t.Fatal("class without one")
	}
}

func TestDrafts(t *testing.T) {
	ctx := context.Background()
	a := k3.DraftActor{Tenant: "t1", Subject: "u1", Name: "U", Roles: []string{"member"}}
	cl, s := platform(t, status(200, `{"drafts":[{"id":"d_1"}]}`))
	vs, err := cl.Drafts().List(ctx, a, k3.DraftListFilter{Status: "open", Target: "x"})
	if err != nil || len(vs) != 1 || vs[0].ID != "d_1" || s.method != "GET" || s.path != "/app/v1/drafts" || s.query != "status=open&target=x" || s.tenant != "t1" {
		t.Fatalf("list %v %v %+v", vs, err, s)
	}

	cl, s = platform(t, status(200, `{"id":"d 1"}`))
	if _, err := cl.Drafts().Get(ctx, a, "d 1"); err != nil || s.path != "/app/v1/drafts/d%201" {
		t.Fatalf("get %v %+v", err, s)
	}
	if _, err := cl.Drafts().Revalidate(ctx, a, "d_1"); err != nil || s.query != "revalidate=1" {
		t.Fatalf("revalidate %v %+v", err, s)
	}
	if _, err := cl.Drafts().Apply(ctx, a, "d_1", nil); err != nil || s.method != "POST" || s.path != "/app/v1/drafts/d_1/apply" || s.body != `{"items":[]}` {
		t.Fatalf("apply %v %+v", err, s)
	}
	if err := cl.Drafts().Abandon(ctx, a, "d_1", "i_1"); err != nil || s.method != "POST" || s.path != "/app/v1/drafts/d_1/items/i_1/abandon" {
		t.Fatalf("abandon %v %+v", err, s)
	}
	if _, err := cl.Drafts().Rebase(ctx, a, "d_1", "i_1"); err != nil || s.path != "/app/v1/drafts/d_1/items/i_1/rebase" {
		t.Fatalf("rebase %v %+v", err, s)
	}
	if _, _, err := cl.Drafts().Review(ctx, a, "d_1", "i_1", k3.ItemEdit{Revision: 2}); err != nil || s.method != "PATCH" || s.path != "/app/v1/drafts/d_1/items/i_1" || !strings.Contains(s.body, `"revision":2`) {
		t.Fatalf("review %v %+v", err, s)
	}

	cl, s = platform(t, status(200, `{"provenance":[{"ref":"k3://x"}]}`))
	ps, err := cl.Drafts().Provenance(ctx, a, "k3://x/y")
	if err != nil || len(ps) != 1 || s.path != "/app/v1/provenance" || s.query != "ref=k3%3A%2F%2Fx%2Fy" {
		t.Fatalf("provenance %v %v %+v", ps, err, s)
	}

	cl, s = platform(t, status(201, `{"id":"d_9"}`))
	v, err := cl.Drafts().Create(ctx, Caller{Tenant: "t1", User: "app"}, DraftCreateReq{For: "u1", DraftCreate: k3.DraftCreate{Summary: "s"}})
	if err != nil || v.ID != "d_9" || s.method != "POST" || s.path != "/app/v1/drafts" || !strings.Contains(s.body, `"for":"u1"`) || !strings.Contains(s.body, `"summary":"s"`) {
		t.Fatalf("create %v %v %+v", v, err, s)
	}
}

func TestDraftsRefusals(t *testing.T) {
	ctx := context.Background()
	a := k3.DraftActor{Tenant: "t1", Subject: "u1"}
	cases := []struct {
		code   int
		body   string
		detail string
	}{
		{409, `{"error":"stale","item":"i_1"}`, "item"},
		{422, `{"error":{"code":"invalid","message":"bad","details":{"missing":["a"]}}}`, "missing"},
		{403, `{"error":"not yours"}`, ""},
		{404, `{"error":"no draft"}`, ""},
	}
	for _, c := range cases {
		cl, _ := platform(t, status(c.code, c.body))
		_, err := cl.Drafts().Get(ctx, a, "d_1")
		var se statusError
		if !errors.As(err, &se) || se.HTTPStatus() != c.code {
			t.Fatalf("%d: %v", c.code, err)
		}
		if c.detail != "" && se.Details()[c.detail] == nil {
			t.Fatalf("%d: details %v", c.code, se.Details())
		}
	}
	cl, _ := platform(t, status(409, `{"error":"stale","item":"i_1"}`))
	err := cl.Drafts().Abandon(ctx, a, "d_1", "i_1")
	if err == nil || err.Error() != "stale" || err.(*DraftError).Extra["item"] != "i_1" {
		t.Fatalf("%v", err)
	}
}

// The refusals come out of MountDrafts as the in-process service's do.
func TestDraftsThroughMount(t *testing.T) {
	cl, _ := platform(t, status(409, `{"error":"stale","item":"i_1"}`))
	mux := NewRouter()
	MountDrafts(mux, "app", Deps{Drafts: cl.Drafts()})
	req := httptest.NewRequest("POST", DraftsPath+"/d_1/items/i_1/rebase", nil)
	req.Header.Set(HeaderTenant, "t1")
	req.Header.Set(HeaderUser, "u1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"item":"i_1"`) || !strings.Contains(rec.Body.String(), `"error":"stale"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestOutreach(t *testing.T) {
	ctx := context.Background()
	c := Caller{Tenant: "t1", User: "u1"}
	cl, s := platform(t, status(202, `{"id":"m_1","existing":true}`))
	q, err := cl.Outreach().Send(ctx, c, OutreachSendReq{Key: "k", Customer: "c_1", Channel: "sms", Template: "t_1", Purpose: "service", Vars: map[string]string{"a": "b"}})
	if err != nil || q.ID != "m_1" || !q.Existing || s.method != "POST" || s.path != "/app/v1/outreach/send" || !strings.Contains(s.body, `"purpose":"service"`) || !strings.Contains(s.body, `"vars":{"a":"b"}`) {
		t.Fatalf("send %+v %v %+v", q, err, s)
	}
	cl, s = platform(t, status(200, `{"items":[{"kind":"sms","ready":true,"secret":{"set":true},"capabilities":{"identity":"phone"}}]}`))
	chs, err := cl.Outreach().Channels(ctx, c)
	if err != nil || len(chs) != 1 || !chs[0].Ready || !chs[0].Secret.Set || chs[0].Capabilities.Identity != "phone" || s.method != "GET" || s.path != "/app/v1/outreach/channels" {
		t.Fatalf("channels %+v %v %+v", chs, err, s)
	}
	cl, s = platform(t, status(200, `{"items":[{"id":"t_1","channel":"sms","version":3}]}`))
	ts, err := cl.Outreach().Templates(ctx, c, "sms", true)
	if err != nil || len(ts) != 1 || ts[0].Version != 3 || s.path != "/app/v1/outreach/templates" || s.query != "archived=1&channel=sms" {
		t.Fatalf("templates %+v %v %+v", ts, err, s)
	}
	if _, err := cl.Outreach().Templates(ctx, c, "", false); err != nil || s.query != "" {
		t.Fatalf("templates plain %v %+v", err, s)
	}
	cl, s = platform(t, status(200, `{"id":"t 1","name":"N"}`))
	tpl, err := cl.Outreach().Template(ctx, c, "t 1")
	if err != nil || tpl.Name != "N" || s.path != "/app/v1/outreach/templates/t%201" {
		t.Fatalf("template %+v %v %+v", tpl, err, s)
	}
	cl, s = platform(t, status(200, `{"id":"m_1","state":"sent","attempts":1}`))
	m, err := cl.Outreach().Message(ctx, c, "m_1")
	if err != nil || m.State != "sent" || s.path != "/app/v1/outreach/messages/m_1" {
		t.Fatalf("message %+v %v %+v", m, err, s)
	}
	for code, want := range map[int]error{404: ErrNotFound, 422: ErrInvalid, 409: ErrConflict, 403: ErrForbidden} {
		cl, _ := platform(t, status(code, `{"error":"x"}`))
		if _, err := cl.Outreach().Message(ctx, c, "m"); !errors.Is(err, want) {
			t.Fatalf("%d: %v", code, err)
		}
	}
}
