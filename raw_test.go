package raise

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawCall is what the fake platform saw of one call of a raw-bytes operation.
type rawCall struct {
	method, path, rawPath, contentType, accept, idem string
	length                                           int64
	chunked                                          bool
	body                                             []byte
}

type rawFake struct {
	mu    sync.Mutex
	calls []rawCall
	srv   *httptest.Server
}

// newRawFake serves the token endpoint and hands every /app/v1 call to h after recording it.
func newRawFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *rawFake {
	t.Helper()
	f := &rawFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","token_type":"Bearer","expires_in":300,"scope":"app.blobs:write","tenant":"t1","environment":"test"}`))
	})
	mux.HandleFunc("/app/v1/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, rawCall{
			method: r.Method, path: r.URL.Path, rawPath: r.URL.EscapedPath(), contentType: r.Header.Get("Content-Type"),
			accept: r.Header.Get("Accept"), idem: r.Header.Get("Idempotency-Key"), length: r.ContentLength,
			chunked: len(r.TransferEncoding) > 0, body: b,
		})
		n := len(f.calls)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		h(w, r, n)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *rawFake) client() *Client {
	return New("cid", "secret", WithBaseURL(f.srv.URL+"/open/v1"), WithTenant("t1"), WithMaxRetries(2))
}

func writeEnvelope(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req-1")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"no","requestId":"req-1","docUrl":"","details":{}}}`))
}

// blobs.put sends the bytes as they are to PUT /app/v1/blobs/objects/<key>, the key's slashes kept and each segment escaped, with the
// length and the type; an empty type is application/octet-stream.
func TestRawPutSendsTheBytes(t *testing.T) {
	f := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusNoContent) })
	c := f.client()
	data := []byte{0, 1, 2, 0xff, 'h', 'i'}
	if _, err := c.App.Blobs.Put(context.Background(), "apps/shop/报告 1/a b.bin", bytes.NewReader(data), -1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.App.Blobs.Put(context.Background(), "apps/shop/x.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	one := f.calls[0]
	if one.method != "PUT" || one.path != "/app/v1/blobs/objects/apps/shop/报告 1/a b.bin" {
		t.Errorf("method %s path %q", one.method, one.path)
	}
	if one.rawPath != "/app/v1/blobs/objects/apps/shop/%E6%8A%A5%E5%91%8A%201/a%20b.bin" {
		t.Errorf("escaped path %q", one.rawPath)
	}
	if !bytes.Equal(one.body, data) || one.length != int64(len(data)) || one.chunked {
		t.Errorf("body %v length %d chunked %v", one.body, one.length, one.chunked)
	}
	if one.contentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q", one.contentType)
	}
	if one.idem != "" {
		t.Errorf("a PUT carries no Idempotency-Key unless asked: %q", one.idem)
	}
	if two := f.calls[1]; two.contentType != "text/plain" || string(two.body) != "hello" || two.length != 5 {
		t.Errorf("second call %+v", two)
	}
}

// A reader with neither Len nor Seek goes out without a Content-Length (chunked); the platform answers 411 for a put, as a typed Error.
func TestRawPutUnknownLength(t *testing.T) {
	f := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.ContentLength < 0 {
			writeEnvelope(w, http.StatusLengthRequired, "length_required")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	_, err := f.client().App.Blobs.Put(context.Background(), "apps/shop/a", io.MultiReader(strings.NewReader("abc")), -1, "")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusLengthRequired || apiErr.Code != "length_required" || apiErr.RequestID != "req-1" {
		t.Fatalf("err = %v", err)
	}
	if !f.calls[0].chunked || string(f.calls[0].body) != "abc" {
		t.Errorf("call %+v", f.calls[0])
	}
	// the same reader with a size given goes out with it
	if _, err := f.client().App.Blobs.Put(context.Background(), "apps/shop/a", io.MultiReader(strings.NewReader("abc")), 3, ""); err != nil {
		t.Fatal(err)
	}
}

// A 503 is retried with the same bytes when the reader can seek back; a reader that cannot is sent once and the 503 stands.
func TestRawPutRetryRewinds(t *testing.T) {
	f := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			writeEnvelope(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r := bytes.NewReader([]byte("0123456789"))
	_, _ = r.Seek(2, io.SeekStart) // the body starts where the reader stands
	if _, err := f.client().App.Blobs.Put(context.Background(), "apps/shop/a", r, -1, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || string(f.calls[0].body) != "23456789" || string(f.calls[1].body) != "23456789" {
		t.Fatalf("calls %+v", f.calls)
	}

	g := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "0")
		writeEnvelope(w, http.StatusServiceUnavailable, "unavailable")
	})
	_, err := g.client().App.Blobs.Put(context.Background(), "apps/shop/a", io.MultiReader(strings.NewReader("abc")), 3, "")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
	if len(g.calls) != 1 {
		t.Errorf("a reader that cannot seek was sent %d times", len(g.calls))
	}
}

// blobs.get streams the bytes: the body is the caller's to read and close, and the metadata comes from the headers.
func TestRawGetStreamsTheBytes(t *testing.T) {
	mod := time.Date(2026, 11, 2, 8, 30, 0, 0, time.UTC)
	data := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 1024)
	f := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/app/v1/blobs/objects/apps/shop/missing" {
			writeEnvelope(w, http.StatusNotFound, "not_found")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Last-Modified", mod.Format(http.TimeFormat))
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("X-Request-Id", "req-9")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
	c := f.client()
	res, err := c.App.Blobs.Get(context.Background(), "apps/shop/img/a.png")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("body %d bytes, err %v", len(got), err)
	}
	if res.ContentType != "image/png" || res.ContentLength != int64(len(data)) || !res.LastModified.Equal(mod) || res.ETag != `"v1"` || res.Header.Get("X-Request-Id") != "req-9" {
		t.Errorf("res = %+v", res)
	}
	if call := f.calls[0]; call.method != "GET" || call.path != "/app/v1/blobs/objects/apps/shop/img/a.png" || call.accept != "*/*" {
		t.Errorf("call %+v", call)
	}

	_, err = c.App.Blobs.Get(context.Background(), "apps/shop/missing")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "not_found" || apiErr.RequestID != "req-1" {
		t.Fatalf("err = %v", err)
	}
}

// models.transcribe posts the recording with the caller's audio type and an Idempotency-Key, and decodes {text}.
func TestRawTranscribe(t *testing.T) {
	f := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("Content-Type") == "audio/flac" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"model_off","message":"模型未开放","requestId":"req-2","docUrl":"","details":{}}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"下午三点到店"}`))
	})
	c := New("cid", "secret", WithBaseURL(f.srv.URL+"/open/v1"), WithTenant("t1"), WithMaxRetries(0))
	audio := []byte("RIFF....WAVEfmt ")
	out, err := c.App.Models.Transcribe(context.Background(), bytes.NewReader(audio), int64(len(audio)), "audio/wav", WithIdempotencyKey("k-7"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "下午三点到店" {
		t.Errorf("text = %q", out.Text)
	}
	call := f.calls[0]
	if call.method != "POST" || call.path != "/app/v1/models/transcribe" || call.contentType != "audio/wav" || call.idem != "k-7" || !bytes.Equal(call.body, audio) {
		t.Errorf("call %+v", call)
	}
	_, err = c.App.Models.Transcribe(context.Background(), bytes.NewReader(audio), -1, "audio/flac")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "model_off" {
		t.Fatalf("err = %v", err)
	}
	if f.calls[1].idem == "" {
		t.Error("a POST without a key gets one")
	}
}

// An empty or nil body is sent with Content-Length 0.
func TestRawEmptyBody(t *testing.T) {
	f := newRawFake(t, func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusNoContent) })
	if _, err := f.client().App.Blobs.Put(context.Background(), "apps/shop/empty", nil, -1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client().App.Blobs.Put(context.Background(), "apps/shop/empty", bytes.NewReader(nil), -1, ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c.length != 0 || c.chunked || len(c.body) != 0 {
			t.Errorf("call %+v", c)
		}
	}
}
