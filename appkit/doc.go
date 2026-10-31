// Package appkit is the app side of the platform contract for an app server that runs in a process of its own, in its own repository
// (ADR-0130 §2.2–§2.5, P4). It is the platform's first-party kit (internal/firstparty) and the app's half of internal/remoteapp, copied
// and pruned to what such an app uses, with the same names: an app built on the kit changes only its import path (firstparty.X →
// appkit.X).
//
// What an app handler may rely on (the same as in the platform's process):
//   - CallerOf(r) is the verified tenant, person and roles (Inbound sets them from the App token the gateway forwards); it is absent
//     for an anonymous request;
//   - tenant data is only touched through Tx, which scopes the transaction so row-level security applies;
//   - a call made by the platform's agent on a person's behalf (Caller.Agent) must not change anything.
//
// What the platform asks of the app (its providers, site actions, deal events, data events and ops) is served by Serve, by what the
// app's manifest declares; what the app asks of the platform (/app/v1) is Client. The wire is the platform's: the paths, the bodies and
// the assertions are byte for byte those of internal/providers, internal/appapi and the draft service.
//
// It imports the standard library and raise-go's own packages only: the app chooses its SQL driver (the SQL it runs is PostgreSQL's).
package appkit
