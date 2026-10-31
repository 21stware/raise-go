# Changelog

## 0.4.0 — 2026-11-01

- `appkit`: four more `/app/v1` clients, the platform's whole contract for each (general, not trimmed to one app; first used by the campaign app in `raise-growth`, later by the rehab apps): `Client.Entitlements()` (`EntitlementClient` with `Dry`, `GrantVia`, `HasGranter`, `RelinkStore`; `EntTemplate`, `TemplateReq`, `GrantReq`, `GrantResult`, `GrantItem`, `EntGrant`, `RedeemReq`, `RedeemResult`, `ReserveReq`, `EntReservation`, `EntRefundReq`, `EntRefundResult`, `TransferReq`, the kind/source/state constants, `ErrNoGranter`, `EntRefused`; dry run by `K3-Dry-Run`), `Client.Vault()` (`Vault`: encrypt, decrypt, shred under the app's own `<kind>:<id>`), `Client.Sites()` (`Sites`, `SiteUsageReq`, `SiteSummary`) and `Client.Stores()` (`OrgStores`, `OrgStore`); `Deps.EntitlementAPI` with `Deps.EntitlementClient()`, `Deps.Vault`, `Deps.Sites`, `Deps.Stores`. Same names as the platform's `firstparty`.

## 0.3.0 — 2026-10-31

- `k3`: the manifest, the platform's assertions and keys (generated from the platform's protocol package by `tools/raisego`).
- `provide`: what an app answers when the platform asks (the provider contracts of `k3.ProvidersDecl`), generated the same way.
- `appkit` (v0, may change): the app side of the platform contract for an app server in its own repository — router, `Caller` from the platform's App token, ops and drafts, tenant-scoped `Tx`, the ledger outbox, deal events, site actions, data events (`tenant.deleted`, `site.deleted`), `Serve` for the providers a manifest declares, and `Client` for `/app/v1`. Standard library only; the app picks its SQL driver. First user: `raise-distribution` (ADR-0130 P4).
- `appkit`: the new `/app/v1` clients: `Client.Blobs(tenant)` (`BlobStore`, `ObjectInfo`, `ErrObjectNotFound`), `Client.Notify` with `WithNoticeClass` / `NoticeClass` and `Deps.Tell`, `Client.Drafts()` (a `DraftService` over HTTP, `DraftError`, `Create`) and `Client.Outreach()` (`OutreachClient`); `Deps.Blobs`, `Deps.Notify`, `Deps.OutreachAPI`.
- `appkit`: more `/app/v1` clients: `Client.Options()` (`OptionStore`, `OptionList`, `OptionsChange`; dry run by `K3-Dry-Run`), `Client.Model()` (`Model`, `ModelClient`, `ErrModelOff`), `Client.Pay()` (`Pay`, `PayOrder`, `PayRefund`, `PayCreateReq`, `PayRefundReq`), `Client.Approvals()` (`Approvals`, over the `k3` approval types) and `Client.CalendarPush` (`CalendarProjection`); `Deps.OptionsAPI`, `Deps.Model`, `Deps.Pay`, `Deps.Approvals`. The outbox has no handler registry yet, so a calendar push is not queued in the app's transaction.
- `Client.App.Resources`: the access kernel's tree model on the same routes (a type declared with a parent, or another type's parent): `ReplaceEntries`, `Mine`, `Reconcile`, `agent` / `reasons` on `Check`, `agent` on `Visible`; `Get` takes `AppResourcesGetParams` (`user`, `agent`).

## 0.2.0 — 2026-10-31

- `Client.App`: the app API (`/app/v1`) as generated resources (`client.App.Customers.Resolve`, `.Visitors`, `.Access`, `.Directory`, `.Ledger`, `.Audit`, `.Installations`, `.Services`, `.Events`, `.Links`), from `docs/api/app-v1.openapi.yaml`. Same credential and tokens; the root is the `/open/v1` base URL with `/app/v1` in its place.
- `Client.Token(ctx, tenant)`: the cached tenant token, for an app backend's calls to the platform's app API (`/app/v1`).

## 0.1.0

- First release of `github.com/21stware/raise-go`.
- Token exchange (`client_credentials` and customer grant), per-tenant cache, refresh 60s early, single-flight.
- Generated resources: platform (whoami, ping), tables (list/get rows), membership `/me` on `ForCustomer`.
- Retries on 429 / 5xx / network errors; `Idempotency-Key` on POST; cursor iterator; typed `*Error`; webhook verify.
