# Changelog

## 0.2.0 — 2026-10-31

- `Client.App`: the app API (`/app/v1`) as generated resources (`client.App.Customers.Resolve`, `.Visitors`, `.Access`, `.Directory`, `.Ledger`, `.Audit`, `.Installations`, `.Services`, `.Events`, `.Links`), from `docs/api/app-v1.openapi.yaml`. Same credential and tokens; the root is the `/open/v1` base URL with `/app/v1` in its place.
- `Client.Token(ctx, tenant)`: the cached tenant token, for an app backend's calls to the platform's app API (`/app/v1`).

## 0.1.0

- First release of `github.com/21stware/raise-go`.
- Token exchange (`client_credentials` and customer grant), per-tenant cache, refresh 60s early, single-flight.
- Generated resources: platform (whoami, ping), tables (list/get rows), membership `/me` on `ForCustomer`.
- Retries on 429 / 5xx / network errors; `Idempotency-Key` on POST; cursor iterator; typed `*Error`; webhook verify.
