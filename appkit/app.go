package appkit

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/21stware/raise-go/k3"
)

// App is one app served by Serve.
type App interface {
	// ID is the app's id and the label of its host ({id}.apps.{base}).
	ID() string
	// Manifest is what the catalogue publishes and the workspace reads (name, nav, permission points, entities for the
	// agent). base is the deployment's base domain (the app's web url is https://{id}.apps.{base}).
	Manifest(version, base string) *k3.Manifest
	// Handler serves everything under {id}.apps.{base}/api/ (and anything else the gateway lets through). The static
	// interface is served by the edge gateway, not here.
	Handler(d Deps) http.Handler
}

// Listing is how the app is described in the market and on the workspace's cards. An app that implements Listed keeps this
// up to date at every start (the build is the source of truth).
type Listing struct {
	Summary     string // one line, at most 200 characters
	Description string // at most 4000
	Category    string // at most 40
	Icon        string // an icon name from @21stware/raise-design/icons' APP_ICONS (database, bar-chart-3, file-text, clipboard-list, users ...)
}

// Listed is implemented by an App that describes itself.
type Listed interface{ Listing() Listing }

// Gated is implemented by an app every page of which needs one of its permission points (product review 2026-10-28, C1): the workspace
// lists the app only for a person who holds at least one of the points its manifest declares, by Authz's rules (the administrator holds
// every point, the auditor every read point). PermissionDefaults is the app's Authz.Defaults. An app with pages that need no point (what
// was shared with a person, a page of their own) does not implement it and is listed for everyone.
type Gated interface{ PermissionDefaults() map[string][]string }

// Deps is everything the platform gives an app that runs in a process of its own: its own database and the platform's services over
// /app/v1 (Client). Fields may be nil (tests, a deployment without a database); an app must then answer 503 for what it cannot do
// rather than panic.
type Deps struct {
	// DB is the app's own database, reached with its own role (row-level security applies; ADR-0130 §2.5).
	DB *sql.DB
	// Schema is the app's own database schema (app_<id>): the outbox and the op results the kit writes are that schema's.
	Schema string
	// Perms is the tenant's permission model (Client.Access); nil means the platform roles decide alone.
	Perms Access
	// Dir lists the tenant's people (Client.Directory).
	Dir Directory
	// Drafts is the platform's draft service as the app's pages reach it (MountDrafts). Nil when it is not wired: the draft routes
	// then answer 503.
	Drafts DraftService
	// SiteVisitors is who the visitors of the tenant's sites are (Client.Visitors).
	SiteVisitors SiteVisitors
	// CustomerAPI is the customer centre (Client.Customers); CustomerClient() answers it.
	CustomerAPI CustomerClient
	// LedgerAPI is where the app's ledger postings go (Client.PostLedger; Outbox.LedgerAPI).
	LedgerAPI func(ctx context.Context, c Caller, req PostReq) (string, error)
	// LinksAPI is the platform's links layer (Client.Links); LinksLayer() answers it.
	LinksAPI Links
	// AuditAPI is where the app's audit entries go (Client.Audit: the tenant's chain, as the caller); Record writes through it.
	AuditAPI func(ctx context.Context, c Caller, action, resource string, diff any) error
	// Blobs is the tenant's file store (Client.Blobs); keys are in the app's folder, apps/{app}/…. Nil when it is not wired.
	Blobs func(tenant string) BlobStore
	// Notify tells people of a tenant something (Client.Notify); best effort, and nil-safe through Tell. The notice's class is the
	// context's (WithNoticeClass).
	Notify func(ctx context.Context, tenant string, users []string, title, body, link string)
	// OutreachAPI is the platform's outreach service (Client.Outreach): messages to customers by template. Nil when it is not wired.
	OutreachAPI OutreachClient
	// OptionsAPI is the tenant's option lists (Client.Options). Nil when it is not wired.
	OptionsAPI OptionStore
	// Model is the tenant's service model (Client.Model); nil when it is not wired.
	Model Model
	// Pay is the payment service (Client.Pay). Nil when it is not wired.
	Pay Pay
	// Approvals is the approval service (Client.Approvals); the app's kinds are its manifest's approvals. Nil when it is not wired.
	Approvals Approvals
	// EntitlementAPI is the entitlement service (Client.Entitlements); EntitlementClient() answers it. Nil when it is not wired.
	EntitlementAPI EntitlementClient
	// Vault is the platform's secret store (Client.Vault): subjects are the app's own <kind>:<id>. Nil when it is not wired.
	Vault Vault
	// Sites is what the app reads of the tenant's sites (Client.Sites). Nil when it is not wired.
	Sites Sites
	// Stores is the organisation's stores (Client.Stores). Nil when it is not wired.
	Stores OrgStores
	// Shutdown takes hooks the process runs when it stops (Every's jobs register their own). Nil-safe.
	Shutdown *ShutdownHooks
	Log      *slog.Logger
	Base     string
	Now      func() time.Time
}

// Clock is the time to use.
func (d Deps) Clock() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Logger never returns nil.
func (d Deps) Logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Record writes one entry on the tenant's chain. A failure is logged, not returned: the business action already happened.
func (d Deps) Record(ctx context.Context, c Caller, action, resource string, diff any) {
	if d.AuditAPI == nil {
		return
	}
	if err := d.AuditAPI(ctx, c, action, resource, diff); err != nil {
		d.Logger().Warn("appkit: audit append failed", "action", action, "err", err)
	}
}

// DefaultCapabilities are what every first-party interface asks the workspace for.
var DefaultCapabilities = []string{"presence", "nav.open", "ui.toast", "ui.modal", "draft.read"} // draft.read: the record page's banner opens the platform's draft page (ADR-0054 §5)

// NewManifest starts a manifest for an app: level 1 (linkable entities: the agent reads them), shown in the workspace panel as an
// iframe at https://{id}.apps.{base}. The caller adds nav, permissions, entities, modules.
func NewManifest(id, name, version, base string) *k3.Manifest {
	return &k3.Manifest{
		Protocol: k3.Protocol, ID: id, Version: version, Level: k3.L1Linkable, Name: name, Embed: "iframe",
		Web: &k3.WebEntry{URL: "https://" + id + ".apps." + base, Capabilities: DefaultCapabilities, Wide: true, OwnNav: true},
	}
}

// ShutdownHooks are run when the process stops (the periodic jobs of Every register here).
type ShutdownHooks struct {
	mu sync.Mutex
	fs []func(context.Context)
}

// Add registers f. Safe on a nil receiver (nothing is registered).
func (h *ShutdownHooks) Add(f func(context.Context)) {
	if h == nil || f == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fs = append(h.fs, f)
}

// Run calls every hook at once and waits until they have returned or ctx is done.
func (h *ShutdownHooks) Run(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	fs := append([]func(context.Context){}, h.fs...)
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, f := range fs {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-ctx.Done():
	}
}
