package appkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// blobTimeout bounds one transfer of bytes through the platform (the JSON calls keep clientTimeout).
const blobTimeout = 10 * time.Minute

// ErrObjectNotFound is what a BlobStore answers (wrapped) for an object that is not there. A store reached through Client.Blobs
// answers both it and ErrNotFound.
var ErrObjectNotFound = errors.New("object not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key         string
	Size        int64
	ContentType string
	ModTime     time.Time
}

// BlobStore is the tenant's file store as an app reaches it (Deps.Blobs, Client.Blobs). Every key is in the app's folder, apps/{app}/…;
// the platform refuses any other and adds the tenant's own prefix itself.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get streams the object; the caller closes it. A missing object is ErrObjectNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Copy(ctx context.Context, src, dst string) error
	DeletePrefix(ctx context.Context, prefix string) error
	// PresignUpload is a URL that takes exactly size bytes for key until ttl ends (the size is signed in; at most 512 MB, within the
	// tenant's storage).
	PresignUpload(ctx context.Context, key string, size int64, ttl time.Duration) (string, error)
	PresignDownload(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Blobs is the tenant's file store over /app/v1/blobs (ADR-0130 §3.2 H7): Deps.Blobs = cl.Blobs.
func (cl *Client) Blobs(tenant string) BlobStore {
	return blobStore{cl: cl, c: Caller{Tenant: tenant}}
}

type blobStore struct {
	cl *Client
	c  Caller
}

var _ BlobStore = blobStore{}

type blobInfoJSON struct {
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType,omitempty"`
	ModTime     time.Time `json:"modTime"`
}

func (bi blobInfoJSON) info() ObjectInfo {
	return ObjectInfo{Key: bi.Key, Size: bi.Size, ContentType: bi.ContentType, ModTime: bi.ModTime}
}

// objectPath is the path of a key, each segment escaped. A key with an empty, "." or ".." segment is refused here, as the platform
// refuses it: a URL would resolve the segment away and name another object.
func objectPath(route, key string) (string, error) {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("%w: the key has an empty, \".\" or \"..\" segment", ErrInvalid)
		}
		segs[i] = url.PathEscape(s)
	}
	return "/app/v1/blobs/" + route + "/" + strings.Join(segs, "/"), nil
}

func (b blobStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	path, err := objectPath("objects", key)
	if err != nil {
		return err
	}
	resp, err := b.cl.stream(ctx, b.c, "PUT", path, nil, r, size, contentType)
	if err != nil {
		return err
	}
	return replyErr(resp)
}

func (b blobStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	path, err := objectPath("objects", key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	resp, err := b.cl.stream(ctx, b.c, "GET", path, nil, nil, 0, "")
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	if err := replyErr(resp); err != nil {
		return nil, ObjectInfo{}, err
	}
	info := ObjectInfo{Key: key, Size: resp.ContentLength, ContentType: resp.Header.Get("Content-Type")}
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
		info.Size = n
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		info.ModTime = t
	}
	return resp.Body, info, nil
}

func (b blobStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	path, err := objectPath("meta", key)
	if err != nil {
		return ObjectInfo{}, err
	}
	var bi blobInfoJSON
	if err := b.cl.call(ctx, b.c, "GET", path, nil, nil, &bi); err != nil {
		return ObjectInfo{}, objectNotFound(err)
	}
	return bi.info(), nil
}

func (b blobStore) Delete(ctx context.Context, key string) error {
	path, err := objectPath("objects", key)
	if err != nil {
		return err
	}
	return b.cl.call(ctx, b.c, "DELETE", path, nil, nil, nil)
}

func (b blobStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out struct {
		Items []blobInfoJSON `json:"items"`
	}
	if err := b.cl.call(ctx, b.c, "GET", "/app/v1/blobs", url.Values{"prefix": {prefix}}, nil, &out); err != nil {
		return nil, err
	}
	infos := make([]ObjectInfo, 0, len(out.Items))
	for _, bi := range out.Items {
		infos = append(infos, bi.info())
	}
	return infos, nil
}

func (b blobStore) Copy(ctx context.Context, src, dst string) error {
	body := map[string]string{"src": src, "dst": dst}
	return objectNotFound(b.cl.call(ctx, b.c, "POST", "/app/v1/blobs/copy", nil, body, nil))
}

func (b blobStore) DeletePrefix(ctx context.Context, prefix string) error {
	return b.cl.call(ctx, b.c, "POST", "/app/v1/blobs/delete-prefix", nil, map[string]string{"prefix": prefix}, nil)
}

func (b blobStore) PresignUpload(ctx context.Context, key string, size int64, ttl time.Duration) (string, error) {
	return b.presign(ctx, key, http.MethodPut, &size, ttl)
}

func (b blobStore) PresignDownload(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return b.presign(ctx, key, http.MethodGet, nil, ttl)
}

func (b blobStore) presign(ctx context.Context, key, method string, size *int64, ttl time.Duration) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	body := map[string]any{"key": key, "method": method}
	if size != nil {
		body["size"] = *size
	}
	if s := int(ttl / time.Second); s > 0 {
		body["ttlSeconds"] = s
	}
	err := b.cl.call(ctx, b.c, "POST", "/app/v1/blobs/presign", nil, body, &p)
	return p.URL, objectNotFound(err)
}

// objectNotFound adds ErrObjectNotFound to the platform's "not found", so a caller can ask either.
func objectNotFound(err error) error {
	if err != nil && errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w (%w)", err, ErrObjectNotFound)
	}
	return err
}

// replyErr reads and closes an answer that is not 2xx and maps it as call does; a 2xx answer is left open (a GET's body is the object).
func replyErr(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if resp.Request != nil && resp.Request.Method != http.MethodGet {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return objectNotFound(mapStatus(resp.StatusCode, resp.Header, body))
}

// stream is do for bytes: the body is r (size bytes of type ct; nil for none) and the answer is left open for the caller.
func (cl *Client) stream(ctx context.Context, c Caller, method, path string, query url.Values, r io.Reader, size int64, ct string) (*http.Response, error) {
	if cl.Token == nil {
		return nil, errors.New("appkit: no token source")
	}
	token, err := cl.Token(ctx, c)
	if err != nil {
		return nil, err
	}
	base, err := cl.baseURL()
	if err != nil {
		return nil, err
	}
	rel, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	u := base.ResolveReference(rel)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	if !cl.forBase(u) {
		return nil, errors.New("appkit: request is not for the platform")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	if r != nil {
		req.ContentLength = size
		if ct == "" {
			ct = "application/octet-stream"
		}
		req.Header.Set("Content-Type", ct)
		if size == 0 {
			req.Body = http.NoBody // an empty object, not a body of unknown length
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range traceOf(ctx) {
		req.Header.Set(k, v)
	}
	for k, v := range headersOf(ctx) {
		req.Header.Set(k, v)
	}
	if c.Tenant != "" {
		req.Header.Set(HeaderTenant, c.Tenant)
	}
	hc := cl.http()
	if hc.Timeout < blobTimeout {
		hc.Timeout = blobTimeout
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if err := refuseOtherTenant(resp.Header, c.Tenant); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}
