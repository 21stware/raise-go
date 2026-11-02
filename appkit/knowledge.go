package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/21stware/raise-go/blobs"
	"github.com/21stware/raise-go/k3"
	"github.com/21stware/raise-go/provide"
	"github.com/21stware/raise-go/realtime"
)

// What an app that keeps documents needs from the platform besides the /app/v1 clients (ADR-0130 P8 S3, first user: the knowledge
// apps of raise-knowledge): its files as a blobs.Store, the backlinks of its records (Client.Backlinks), and the platform's calls it
// answers — the providers retrieve and embeddings, and the services of realtime.doc.projected, app.installed and permissions.changed.
// The providers backlinks and backlinks.count are serveKinds' (serve_kinds.go).

// Files is the app's part of the tenant's file store as a blobs.Store: Deps.Blobs, with the keys under the app's folder
// (FilesPrefix), which the platform requires. Without Deps.Blobs every call is blobs.ErrUnavailable.
func (d Deps) Files(app string) blobs.Store { return files{d: d, app: app} }

// FilesPrefix is the app's folder in the tenant's file store.
func FilesPrefix(app string) string { return "apps/" + app + "/" }

type files struct {
	d   Deps
	app string
}

func (f files) store(tenant string) (BlobStore, error) {
	if f.d.Blobs == nil || tenant == "" {
		return nil, blobs.ErrUnavailable
	}
	s := f.d.Blobs(tenant)
	if s == nil {
		return nil, blobs.ErrUnavailable
	}
	return s, nil
}

func (f files) key(k string) string { return FilesPrefix(f.app) + k }

// Ready implements blobs.Store: the store is there when Deps.Blobs is (the platform answers for the tenant's own store on the first
// call).
func (f files) Ready(_ context.Context, tenant string) error {
	_, err := f.store(tenant)
	return err
}

func (f files) Put(ctx context.Context, tenant, key string, r io.Reader, size int64, contentType string) error {
	s, err := f.store(tenant)
	if err != nil {
		return err
	}
	return s.Put(ctx, f.key(key), r, size, contentType)
}

func (f files) Get(ctx context.Context, tenant, key string) (io.ReadCloser, blobs.Info, error) {
	s, err := f.store(tenant)
	if err != nil {
		return nil, blobs.Info{}, err
	}
	rc, info, err := s.Get(ctx, f.key(key))
	if errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrNotFound) {
		return nil, blobs.Info{}, errors.Join(blobs.ErrNotFound, err)
	}
	if err != nil {
		return nil, blobs.Info{}, err
	}
	return rc, blobs.Info{Size: info.Size, ContentType: info.ContentType}, nil
}

func (f files) Delete(ctx context.Context, tenant, key string) error {
	s, err := f.store(tenant)
	if err != nil {
		return err
	}
	return s.Delete(ctx, f.key(key))
}

func (f files) DeletePrefix(ctx context.Context, tenant, prefix string) error {
	s, err := f.store(tenant)
	if err != nil {
		return err
	}
	return s.DeletePrefix(ctx, f.key(prefix))
}

// StoreWriteError turns the error of a write to the tenant's file store into the refusal to show: the platform's 413 (the tenant
// filled its plan's storage limit; its message names the limit) is answered as 413 with the platform's words, anything else is
// fallback (the caller's "try again later"). The platform's firstparty.StoreWriteError.
func StoreWriteError(err error, fallback error) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusRequestEntityTooLarge {
		return Status(http.StatusRequestEntityTooLarge, he.Message)
	}
	return fallback
}

// Backlinks lists the other apps' records that mention ref, one of the app's own (/app/v1/backlinks): as the person behind c sees
// them, none on the app's own call. The platform leaves out the calling app's own records.
func (cl *Client) Backlinks(ctx context.Context, c Caller, ref string, limit int) ([]Backlink, error) {
	var out struct {
		Items []Backlink `json:"items"`
	}
	rep, err := cl.do(ctx, c, "GET", "/app/v1/backlinks", url.Values{"ref": {ref}, "limit": {strconv.Itoa(limit)}}, nil)
	if err != nil {
		return nil, err
	}
	if err := mapStatus(rep.status, rep.hdr, rep.body); err != nil {
		return nil, err
	}
	if err := refuseOtherTenant(rep.hdr, c.Tenant); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(rep.body, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// BacklinksOf lists the other apps' records that mention ref, one of the app's own, as the caller sees them (Deps.BacklinksAPI).
// The platform leaves out the calling app's records itself, so app (the asking app, which lists its own) is not sent. Nothing
// without BacklinksAPI.
func (d Deps) BacklinksOf(ctx context.Context, c Caller, ref string, limit int, app string) ([]Backlink, error) {
	_ = app
	if d.BacklinksAPI == nil {
		return nil, nil
	}
	return d.BacklinksAPI(ctx, c, ref, limit)
}

// The providers of knowledge (the platform's internal/providers remote kinds; backlinks are in serve_kinds.go):
//
//	POST /k3/providers/retrieve           {query}          → {"passages": [Passage], "modes": [..]}   for a person
//	POST /k3/providers/embeddings/docs    {after, limit}   → {"docs": [EmbeddingDoc], "next"}         the platform, for a tenant
//	POST /k3/providers/embeddings/chunks  {refs}           → {"docs": [EmbeddingChunks]}              the platform, for a tenant
const (
	PathRetrieve        = "/k3/providers/retrieve"
	PathEmbeddingDocs   = "/k3/providers/embeddings/docs"
	PathEmbeddingChunks = "/k3/providers/embeddings/chunks"
	// EmbeddingPage is the most documents one /embeddings/docs page holds.
	EmbeddingPage = 500
)

// The platform events an app hears through services it declares (trigger event:<kind>), besides realtime.doc.projected
// (k3.EventRealtimeProjected).
const (
	EventAppInstalled       = "app.installed"
	EventPermissionsChanged = "permissions.changed"
)

type (
	// Retrieving offers passages of the app's records for the assistant (providers.retrieve).
	Retrieving interface {
		Retriever(d Deps) provide.Retriever
	}
	// Embedding lists the app's documents and their passages for the platform's semantic index (providers.embeddings).
	Embedding interface {
		Embeddings(d Deps) provide.Embeddings
	}
	// RealtimeProjecting hears realtime.doc.projected (the platform projected one of the app's documents): the app reads the
	// projection (Deps.Realtime is a realtime.ProjectionSource) and writes its rows in a transaction of its own.
	RealtimeProjecting interface {
		RealtimeProjected(ctx context.Context, d Deps, tenant string, ev realtime.Projected) error
	}
	// AccessListening hears access.changed (k3.EventAccessChanged {type, id}): the list of one of the app's declared resources
	// changed in the access kernel, for instance in the platform's share dialog, which writes the kernel without the app's routes
	// (ADR-0130 §2.6.2, implementation note of 2026-11-02: the kernel owns the entries, apps re-read). An app that keeps
	// something the kernel decides (a cache, a visibility mirror, a node's inherit flag) re-reads it here. It is told after the
	// change committed, at least once; an error is retried by the platform. The manifest declares AccessChangedService.
	AccessListening interface {
		AccessChanged(ctx context.Context, d Deps, tenant, typ, id string) error
	}
)

// AccessChangedService is the service of event:access.changed an AccessListening app declares in its manifest (Services).
func AccessChangedService() k3.Service {
	return k3.Service{ID: "access-changed", Title: "权限变化", Trigger: []string{k3.TriggerEvent + k3.EventAccessChanged}, Endpoint: "/k3/services/access-changed"}
}

// serveKnowledge mounts the providers above (retrieve, embeddings) that a's manifest declares and its code provides.
func serveKnowledge(mux *http.ServeMux, a App, d Deps, decl *k3.ProvidersDecl) {
	if decl == nil {
		return
	}
	person := func(path string, in func() any, answer func(r *http.Request, c Caller, in any) (any, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			v := in()
			c, ok := viewer(w, r, v)
			if !ok {
				return
			}
			out, err := answer(r, c, v)
			if err != nil {
				writeProviderErr(w, d, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	platform := func(path string, in func() any, answer func(r *http.Request, tenant string, in any) (any, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			v := in()
			if !platformJSON(w, r, k3.ViaProvider, v) {
				return
			}
			tenant, _ := PlatformCall(r.Context())
			out, err := answer(r, tenant, v)
			if err != nil {
				writeProviderErr(w, d, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	if p, ok := a.(Retrieving); ok && decl.Has("retrieve") {
		if rt := p.Retriever(d); rt != nil {
			type retrieveIn struct {
				Query provide.RetrieveQuery `json:"query"`
			}
			person(PathRetrieve, func() any { return &retrieveIn{} }, func(r *http.Request, c Caller, in any) (any, error) {
				ps, err := rt.Retrieve(r.Context(), c, in.(*retrieveIn).Query)
				if err != nil {
					return nil, err
				}
				if ps == nil {
					ps = []provide.Passage{}
				}
				return map[string]any{"passages": ps, "modes": rt.Modes()}, nil
			})
		}
	}
	if p, ok := a.(Embedding); ok && decl.Has("embeddings") {
		if e := p.Embeddings(d); e != nil {
			type docsIn struct {
				After string `json:"after"`
				Limit int    `json:"limit"`
			}
			platform(PathEmbeddingDocs, func() any { return &docsIn{} }, func(r *http.Request, tenant string, in any) (any, error) {
				v := in.(*docsIn)
				if v.Limit <= 0 || v.Limit > EmbeddingPage {
					v.Limit = EmbeddingPage
				}
				docs, next, err := e.EmbeddingDocs(r.Context(), tenant, v.After, v.Limit)
				if err != nil {
					return nil, err
				}
				if docs == nil {
					docs = []provide.EmbeddingDoc{}
				}
				return map[string]any{"docs": docs, "next": next}, nil
			})
			type chunksIn struct {
				Refs []string `json:"refs"`
			}
			platform(PathEmbeddingChunks, func() any { return &chunksIn{} }, func(r *http.Request, tenant string, in any) (any, error) {
				docs, err := e.EmbeddingChunks(r.Context(), tenant, in.(*chunksIn).Refs)
				if err != nil {
					return nil, err
				}
				if docs == nil {
					docs = []provide.EmbeddingChunks{}
				}
				return map[string]any{"docs": docs}, nil
			})
		}
	}
}

// eventRun is a platform event as the service runtime delivers it: {"input": {"id", "type", "tenant", "data"}}.
type eventRun struct {
	Input struct {
		ID     string          `json:"id"`
		Type   string          `json:"type"`
		Tenant string          `json:"tenant"`
		Data   json.RawMessage `json:"data"`
	} `json:"input"`
}

// servePlatformEvents mounts the app's services of the platform events app.installed, permissions.changed (Deps.Changes) and
// realtime.doc.projected (RealtimeProjecting) that its manifest declares.
func servePlatformEvents(mux *http.ServeMux, a App, d Deps, m *k3.Manifest) {
	var own map[string]AppEventFunc // the services the app answers itself (serveAppEvents mounts them)
	if h, ok := a.(AppEventHandling); ok {
		own = h.AppEvents(d)
	}
	handle := func(kind string, fn func(r *http.Request, in eventRun) error) {
		sv, declared := m.ServiceFor(kind)
		if !declared || own[sv.ID] != nil {
			return
		}
		mux.HandleFunc("POST "+sv.Endpoint, func(w http.ResponseWriter, r *http.Request) {
			var in eventRun
			if !platformJSON(w, r, k3.ViaService, &in) || !sameTenant(w, r, in.Input.Tenant) {
				return
			}
			if err := fn(r, in); err != nil {
				if refusal(err) {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
					return
				}
				d.Logger().Warn("appkit: a platform event was not taken in", "event", kind, "tenant", in.Input.Tenant, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "not yet"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		})
	}
	if d.Changes != nil {
		handle(EventAppInstalled, func(_ *http.Request, in eventRun) error { d.Changes.Installs(in.Input.Tenant); return nil })
		handle(EventPermissionsChanged, func(_ *http.Request, in eventRun) error { d.Changes.Permissions(in.Input.Tenant); return nil })
	}
	if p, ok := a.(RealtimeProjecting); ok {
		handle(k3.EventRealtimeProjected, func(r *http.Request, in eventRun) error {
			var ev realtime.Projected
			if err := json.Unmarshal(in.Input.Data, &ev); err != nil || ev.Doc == "" {
				return Invalid("not a realtime.doc.projected event")
			}
			return p.RealtimeProjected(r.Context(), d, in.Input.Tenant, ev)
		})
	}
	if l, ok := a.(AccessListening); ok {
		handle(k3.EventAccessChanged, func(r *http.Request, in eventRun) error {
			var ev struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(in.Input.Data, &ev); err != nil || ev.Type == "" || ev.ID == "" {
				return Invalid("not an access.changed event")
			}
			return l.AccessChanged(r.Context(), d, in.Input.Tenant, ev.Type, ev.ID)
		})
	}
}
