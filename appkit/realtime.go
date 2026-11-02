package appkit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/21stware/raise-go/k3"
	rt "github.com/21stware/raise-go/realtime"
	"github.com/21stware/raise-go/realtime/pmdoc"
)

// Projections reads the app's documents' projections from the platform (/app/v1/realtime/docs/{id}/projection), for the remote
// form of its Projector (realtime.doc.projected).
func (cl *Client) Projections() rt.ProjectionSource { return projections{cl} }

type projections struct{ cl *Client }

func (p projections) Projection(ctx context.Context, tenant, id string) (rt.ProjectionView, bool, error) {
	var out rt.ProjectionView
	err := p.cl.call(ctx, Caller{Tenant: tenant}, "GET", "/app/v1/realtime/docs/"+url.PathEscape(id)+"/projection", nil, nil, &out)
	if errors.Is(err, ErrNotFound) {
		return out, false, nil
	}
	return out, err == nil, err
}

// Realtime is the platform's real-time service for an app in a process of its own (ADR-0130 §2.6.1): the same rt.Service an app in
// the platform's process gets from firstparty.Deps.Realtime (in the platform's process), over the app API's /app/v1/realtime routes. manifest is the app's own
// (realtime.docs[] says which kinds, codecs and namespaces it keeps).
//
// What differs from the service in process, and why:
//   - the platform keeps the Hub and serves every sync connection (on the app's origin, behind the app gateway), so Serve, Admit,
//     Kick, Rebuild, ScrubOutdated are ErrUnsupported and Close/CloseAll do nothing; Stats is empty (the platform exports the
//     counters); Serving is always true;
//   - the platform authorises with the access kernel (realtime.docs[].resource), gates connections with realtime.docs[].admit and
//     admits topics with realtime.docs[].topics: HubConfig.Auth, TopicAllowed, Admit, Reauth, Guard (providers.realtimeGuard),
//     Seed, SeedState and Owns are not called;
//   - HubConfig.Project and AfterProjection are not called: the app hears realtime.doc.projected and reads the projection (Projections,
//     rt.ProjectionSource), writing its rows in a transaction of its own;
//   - the transaction arguments (Genesis, GenesisState, Delete) are not used: each call is its own request, committed when it
//     answers. Genesis is idempotent (the same id of the same kind answers the existing document), so an app may call it before its
//     own commit and again on a retry;
//   - BumpEpoch runs before in a transaction the app opens itself (RealtimeService.InTx), then asks for the epoch: the two are not
//     atomic;
//   - Codec answers nothing (the codecs never run in an app).
func (cl *Client) Realtime(manifest *k3.Manifest) rt.Service {
	return &RealtimeService{cl: cl, m: manifest}
}

// RealtimeService is Client.Realtime's rt.Service.
type RealtimeService struct {
	cl *Client
	m  *k3.Manifest
	// InTx opens a transaction of the app's own database for BumpEpoch's before callback (nil: a BumpEpoch with before is
	// ErrUnsupported).
	InTx func(ctx context.Context, tenant string, fn func(tx *sql.Tx) error) error
	// Log receives what a fire-and-forget call (Publish) could not do; nil: slog.Default.
	Log *slog.Logger
}

var _ rt.Service = (*RealtimeService)(nil)

type resourceKey struct{}

// WithRealtimeResource names the resource id a document created with ctx is an instance of (Genesis / GenesisState: POST
// /app/v1/realtime/docs {resource}); without it the document's id is the resource's id.
func WithRealtimeResource(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resourceKey{}, id)
}

func (s *RealtimeService) appID() string {
	if s.m == nil {
		return ""
	}
	return s.m.ID
}

// NewHub implements rt.Service: the Hub of the manifest's kind whose codec is cfg.Codec (yjs-v1 when empty) and whose namespace is
// cfg.Store (the app's id when empty). Two kinds of one codec and namespace are told apart by cfg.App (the kind, or "<app>.<kind>").
func (s *RealtimeService) NewHub(cfg rt.HubConfig) (rt.Hub, error) {
	if s.m == nil || s.m.Realtime == nil {
		return nil, errors.New("appkit: the manifest declares no real-time documents")
	}
	codec := cfg.Codec
	if codec == "" {
		codec = rt.PMCodecName
	}
	store := cfg.Store
	if store == "" {
		store = s.appID()
	}
	var match []k3.RealtimeDocDecl
	for _, d := range s.m.Realtime.Docs {
		if d.Codec == codec && d.RealtimeStore(s.appID()) == store {
			match = append(match, d)
		}
	}
	var decl k3.RealtimeDocDecl
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("appkit: the manifest declares no real-time kind of codec %s in namespace %s", codec, store)
	case 1:
		decl = match[0]
	default:
		found := false
		for _, d := range match {
			if cfg.App == d.Kind || strings.HasSuffix(cfg.App, "."+d.Kind) {
				decl, found = d, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("appkit: several real-time kinds of codec %s in namespace %s; name one in HubConfig.App", codec, store)
		}
	}
	log := s.Log
	if log == nil {
		log = cfg.Log
	}
	if log == nil {
		log = slog.Default()
	}
	return &remoteHub{s: s, kind: decl.Kind, codec: decl.Codec, log: log}, nil
}

// Projection implements rt.ProjectionSource (Client.Projections): an app that hears realtime.doc.projected reads the projection
// through the service it already holds.
func (s *RealtimeService) Projection(ctx context.Context, tenant, id string) (rt.ProjectionView, bool, error) {
	return s.cl.Projections().Projection(ctx, tenant, id)
}

var _ rt.ProjectionSource = (*RealtimeService)(nil)

// Codec implements rt.Service: the codecs never run in an app outside the platform (an Office state's properties: ReadMeta).
func (s *RealtimeService) Codec(string) (rt.Codec, bool) { return nil, false }

// ReadMeta implements rt.MetaReader: POST /app/v1/realtime/codecs/{codec}/meta with the app's tenant token (an Office import checks
// the state the browser built against the file).
func (s *RealtimeService) ReadMeta(ctx context.Context, tenant, codec string, state []byte) (map[string]string, error) {
	var out struct {
		Meta map[string]string `json:"meta"`
	}
	err := s.cl.call(ctx, appCaller(tenant), "POST", "/app/v1/realtime/codecs/"+url.PathEscape(codec)+"/meta", nil,
		map[string]any{"state": base64.StdEncoding.EncodeToString(state)}, &out)
	if err != nil {
		return nil, err
	}
	return out.Meta, nil
}

var _ rt.MetaReader = (*RealtimeService)(nil)

// Heads implements rt.Service: GET /app/v1/realtime/docs/{id} per id (the platform finds the document in the namespaces the app's
// kinds use; store only says which namespace the caller means, and a document of another namespace is left out). SnapSeq,
// PendingBytes and State are not part of the app API's header and stay zero.
func (s *RealtimeService) Heads(ctx context.Context, tenant, store string, ids ...string) (map[string]rt.Head, error) {
	out := map[string]rt.Head{}
	for _, id := range ids {
		hd, ok, err := s.head(ctx, tenant, id)
		if err != nil {
			return nil, err
		}
		if ok && (store == "" || s.storeOf(hd.Kind) == store) {
			out[id] = hd.Head()
		}
	}
	return out, nil
}

func (s *RealtimeService) storeOf(kind string) string {
	if s.m == nil {
		return ""
	}
	d, ok := s.m.Realtime.Doc(kind)
	if !ok {
		return ""
	}
	return d.RealtimeStore(s.appID())
}

// docHead is GET …/docs/{id}.
type docHead struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Codec     string    `json:"codec"`
	Epoch     int       `json:"epoch"`
	SchemaVer int       `json:"schemaVer"`
	HeadSeq   int64     `json:"headSeq"`
	ProjSeq   int64     `json:"projSeq"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (h docHead) Head() rt.Head {
	return rt.Head{Codec: h.Codec, Epoch: h.Epoch, SchemaVer: h.SchemaVer, HeadSeq: h.HeadSeq, ProjSeq: h.ProjSeq, UpdatedAt: h.UpdatedAt}
}

func (s *RealtimeService) head(ctx context.Context, tenant, id string) (docHead, bool, error) {
	var hd docHead
	err := s.cl.call(ctx, Caller{Tenant: tenant}, "GET", docPath(id), nil, nil, &hd)
	if errors.Is(err, ErrNotFound) {
		return hd, false, nil
	}
	return hd, err == nil, err
}

func docPath(id string) string { return "/app/v1/realtime/docs/" + url.PathEscape(id) }

// remoteHub is one kind's Hub over the app API.
type remoteHub struct {
	s     *RealtimeService
	kind  string
	codec string
	log   *slog.Logger
}

var _ rt.Hub = (*remoteHub)(nil)

func (h *remoteHub) call(ctx context.Context, c Caller, method, path string, q url.Values, in, out any) error {
	return h.s.cl.call(ctx, c, method, path, q, in, out)
}

// Serving implements rt.Hub: the platform serves the connections, always.
func (h *remoteHub) Serving() bool { return true }

// Close implements rt.Hub: nothing to close in the app (the platform keeps the Hub).
func (h *remoteHub) Close(int) {}

// CloseAll implements rt.Hub: nothing to close in the app (the platform keeps the connections; Uninstalled closes them).
func (h *remoteHub) CloseAll(int) {}

// Stats implements rt.Hub: the platform exports the Hub's counters; the app has none.
func (h *remoteHub) Stats() rt.Stats { return rt.Stats{} }

// Admit implements rt.Hub: the platform admits connections (realtime.docs[].admit).
func (h *remoteHub) Admit(Caller) error { return rt.ErrUnsupported }

// Serve implements rt.Hub: the platform serves connections (GET /app/v1/realtime/sync on the app's origin).
func (h *remoteHub) Serve(context.Context, rt.Conn, Caller, time.Time) error {
	return rt.ErrUnsupported
}

func appCaller(tenant string) Caller { return Caller{Tenant: tenant} }

// chunks splits ids into the route's batches.
func chunks(ids []string, n int) [][]string {
	var out [][]string
	for len(ids) > n {
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

func (h *remoteHub) Invalidate(ctx context.Context, tenant string, docs ...string) error {
	for _, ids := range chunks(docs, 100) {
		if err := h.call(ctx, appCaller(tenant), "POST", "/app/v1/realtime/docs/invalidate", nil, map[string]any{"kind": h.kind, "ids": ids}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (h *remoteHub) InvalidateTenant(ctx context.Context, tenant string) error {
	return h.call(ctx, appCaller(tenant), "POST", "/app/v1/realtime/connections/invalidate", nil, map[string]any{"kind": h.kind}, nil)
}

// Revoke implements rt.Hub. A document already deleted (DELETE revoked it then) is not an error for reason "deleted".
func (h *remoteHub) Revoke(ctx context.Context, tenant, reason string, docs ...string) error {
	for _, ids := range chunks(docs, 100) {
		err := h.call(ctx, appCaller(tenant), "POST", "/app/v1/realtime/docs/revoke", nil, map[string]any{"kind": h.kind, "ids": ids, "reason": reason}, nil)
		if errors.Is(err, ErrNotFound) && reason == "deleted" {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Kick implements rt.Hub: the app API has no route for it; the platform closes a removed member's connections itself.
func (h *remoteHub) Kick(context.Context, string, string, int, string) error {
	return rt.ErrUnsupported
}

// Uninstalled implements rt.Hub: the platform closes the app's connections of the tenant, of every kind (one call per app is enough).
func (h *remoteHub) Uninstalled(ctx context.Context, tenant string) error {
	return h.call(ctx, appCaller(tenant), "POST", "/app/v1/realtime/connections/uninstalled", nil, nil, nil)
}

// publishTimeout bounds a Publish (fire and forget).
const publishTimeout = 5 * time.Second

// Publish implements rt.Hub: fire and forget, as in process; a failure is logged.
func (h *remoteHub) Publish(tenant, topic string, version int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		if err := h.publish(ctx, tenant, topic, version); err != nil {
			h.log.Warn("appkit: publishing a real-time topic failed", "tenant", tenant, "topic", topic, "err", err)
		}
	}()
}

func (h *remoteHub) publish(ctx context.Context, tenant, topic string, version int64) error {
	return h.call(ctx, appCaller(tenant), "POST", "/app/v1/realtime/topics/"+url.PathEscape(topic)+"/publish", nil, map[string]any{"kind": h.kind, "version": version}, nil)
}

func (h *remoteHub) create(ctx context.Context, d rt.DocRef, body map[string]any) (bool, error) {
	res := d.ID
	if v, ok := ctx.Value(resourceKey{}).(string); ok && v != "" {
		res = v
	}
	body["kind"], body["id"], body["resource"] = h.kind, d.ID, res
	rep, err := h.s.cl.do(ctx, appCaller(d.Tenant), "POST", "/app/v1/realtime/docs", nil, body)
	if err != nil {
		return false, err
	}
	if err := mapStatus(rep.status, rep.hdr, rep.body); err != nil {
		return false, err
	}
	return rep.status == http.StatusCreated, nil
}

// Genesis implements rt.Hub (tx is not used: see Client.Realtime). created is false when the document existed.
func (h *remoteHub) Genesis(ctx context.Context, _ *sql.Tx, d rt.DocRef, doc *pmdoc.Node) (bool, error) {
	body := map[string]any{}
	if doc != nil {
		body["doc"] = doc
	}
	return h.create(ctx, d, body)
}

// GenesisState implements rt.Hub (tx is not used: see Client.Realtime).
func (h *remoteHub) GenesisState(ctx context.Context, _ *sql.Tx, d rt.DocRef, state []byte) (bool, error) {
	return h.create(ctx, d, map[string]any{"state": base64.StdEncoding.EncodeToString(state)})
}

// BumpEpoch implements rt.Hub: before runs first in a transaction of the app's own (InTx), then the platform starts the document
// over; not atomic with the epoch (an epoch that fails leaves what before wrote).
func (h *remoteHub) BumpEpoch(ctx context.Context, c Caller, d rt.DocRef, doc *pmdoc.Node, before func(tx *sql.Tx) error) (int, error) {
	if c.Agent != "" {
		return 0, rt.ErrAgent
	}
	if before != nil {
		if h.s.InTx == nil {
			return 0, fmt.Errorf("%w: BumpEpoch with before needs RealtimeService.InTx", rt.ErrUnsupported)
		}
		if err := h.s.InTx(ctx, d.Tenant, before); err != nil {
			return 0, err
		}
	}
	if c.Tenant == "" {
		c.Tenant = d.Tenant
	}
	body := map[string]any{}
	if doc != nil {
		body["doc"] = doc
	}
	var out struct {
		Epoch int `json:"epoch"`
	}
	if err := h.call(ctx, c, "POST", docPath(d.ID)+"/epoch", nil, body, &out); err != nil {
		return 0, err
	}
	return out.Epoch, nil
}

func (h *remoteHub) StoreHead(ctx context.Context, d rt.DocRef) (rt.Head, error) {
	hd, ok, err := h.s.head(ctx, d.Tenant, d.ID)
	if err != nil {
		return rt.Head{}, err
	}
	if !ok {
		return rt.Head{}, rt.ErrNotFound
	}
	return hd.Head(), nil
}

func (h *remoteHub) State(ctx context.Context, d rt.DocRef) ([]byte, int64, error) {
	var out struct {
		Seq   int64  `json:"seq"`
		Codec string `json:"codec"`
		State string `json:"state"`
	}
	if err := h.call(ctx, appCaller(d.Tenant), "GET", docPath(d.ID)+"/state", nil, nil, &out); err != nil {
		return nil, 0, err
	}
	if out.Codec != "" && out.Codec != h.codec {
		return nil, 0, fmt.Errorf("%w: document is %s, this hub serves %s", rt.ErrWrongCodec, out.Codec, h.codec)
	}
	b, err := base64.StdEncoding.DecodeString(out.State)
	if err != nil {
		return nil, 0, fmt.Errorf("appkit: the platform's state is not base64: %w", err)
	}
	return b, out.Seq, nil
}

// ServerEdit implements rt.Hub: the edit is written as actor (the platform authorises the actor again before it writes). The
// projection's Epoch is not part of the answer and stays zero. EditRepair is the platform's own.
func (h *remoteHub) ServerEdit(ctx context.Context, actor Caller, d rt.DocRef, e rt.Edit, origin string, dry bool) (rt.Projection, int64, error) {
	if actor.Agent != "" {
		return rt.Projection{}, 0, rt.ErrAgent
	}
	edit := map[string]any{}
	switch e.Kind {
	case rt.EditReplace:
		edit["replace"] = e.Doc
	case rt.EditAppend:
		edit["append"] = e.Blocks
	case rt.EditSetTitle:
		edit["title"] = e.Title
	case rt.EditReplaceData:
		edit["data"] = e.Data
	case rt.EditReplaceBoard:
		edit["board"] = e.Board
	default:
		return rt.Projection{}, 0, rt.ErrUnsupported
	}
	if actor.Tenant == "" {
		actor.Tenant = d.Tenant
	}
	var out struct {
		Seq        int64             `json:"seq"`
		Projection rt.ProjectionView `json:"projection"`
	}
	if err := h.call(ctx, actor, "POST", docPath(d.ID)+"/edits", nil, map[string]any{"edit": edit, "origin": origin, "dry": dry}, &out); err != nil {
		return rt.Projection{}, 0, err
	}
	actors := []string{}
	if actor.User != "" {
		actors = []string{actor.User}
	}
	return out.Projection.Projection(actors), out.Seq, nil
}

// maxWait is the longest wait the projection route takes.
const maxWait = 5 * time.Second

// EnsureProjected implements rt.Hub: GET …/projection?minSeq&wait (wait at most 5 s); ErrConflict when the projection is
// not that far in time.
func (h *remoteHub) EnsureProjected(ctx context.Context, d rt.DocRef, minSeq int64, wait time.Duration) error {
	wait = min(max(wait, 0), maxWait)
	q := url.Values{"minSeq": {strconv.FormatInt(minSeq, 10)}, "wait": {strconv.FormatInt(wait.Milliseconds(), 10)}}
	return h.call(ctx, appCaller(d.Tenant), "GET", docPath(d.ID)+"/projection", q, nil, nil)
}

// Rebuild implements rt.Hub: the platform's operators rebuild projections; no app API route.
func (h *remoteHub) Rebuild(context.Context, rt.DocRef) error { return rt.ErrUnsupported }

func (h *remoteHub) Scrub(ctx context.Context, d rt.DocRef) (rt.ScrubResult, error) {
	var out struct {
		Repaired  bool `json:"repaired"`
		Compacted bool `json:"compacted"`
	}
	if err := h.call(ctx, appCaller(d.Tenant), "POST", docPath(d.ID)+"/scrub", nil, nil, &out); err != nil {
		return rt.ScrubResult{}, err
	}
	return rt.ScrubResult{Repaired: out.Repaired, Compacted: out.Compacted}, nil
}

// ScrubOutdated implements rt.Hub: the platform scrubs a tenant's outdated documents itself (it knows its schema); no app API route.
func (h *remoteHub) ScrubOutdated(context.Context, string) (rt.ScrubReport, error) {
	return rt.ScrubReport{}, rt.ErrUnsupported
}

// Delete implements rt.Hub: DELETE per id (tx is not used: see Client.Realtime); a document already gone is not an error. The platform
// revokes the deleted documents from their connections.
func (h *remoteHub) Delete(ctx context.Context, _ *sql.Tx, tenant string, ids ...string) error {
	for _, id := range ids {
		err := h.call(ctx, appCaller(tenant), "DELETE", docPath(id), nil, nil, nil)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}
