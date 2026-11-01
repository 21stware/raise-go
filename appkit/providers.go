package appkit

import (
	"context"
	"time"

	"github.com/21stware/raise-go/provide"
)

// What an app answers when the platform asks (ADR-0130 §2.4): the contracts are raise-go/provide's. An app provides a kind by
// implementing the matching ...ing interface below AND declaring it in its manifest (k3.ProvidersDecl); Serve answers only what is
// both.

type SourceField = provide.SourceField

type Source = provide.Source

type DataProvider = provide.DataProvider

type Link = provide.Link

type LinkProvider = provide.LinkProvider

type Links = provide.Links

type Referrers = provide.Referrers

type Promoter = provide.Promoter

type ReferralPoster = provide.ReferralPoster

type ReferralCode = provide.ReferralCode

type ReferralCodes = provide.ReferralCodes

type (
	// DataProviding offers sources (分析; providers.sources).
	DataProviding interface {
		DataProvider(d Deps) DataProvider
	}
	// LinkProviding offers the app's linkable records (providers.links).
	LinkProviding interface {
		LinkProvider(d Deps) LinkProvider
	}
	// Attributing offers who sent a site visitor (providers.referrer); its Referrers may also be the app's ReferralCodes
	// (providers.referralCodes).
	Attributing interface {
		Referrers(d Deps) Referrers
	}
)

// The kinds an app in a process of its own answers since ADR-0130 P11 (the platform's internal/providers/remote_kinds.go and
// registry_customer_assert.go are the caller's side).

type PeopleQuery = provide.PeopleQuery

type PersonCandidate = provide.PersonCandidate

type PeopleSource = provide.PeopleSource

type Backlink = provide.Backlink

type BacklinkProvider = provide.BacklinkProvider

type BacklinkCounter = provide.BacklinkCounter

type Holding = provide.Holding

type HoldingsProvider = provide.HoldingsProvider

type AgendaItem = provide.AgendaItem

type AgendaProvider = provide.AgendaProvider

type WorkQuery = provide.WorkQuery

type WorkProvider = provide.WorkProvider

type DeskProvider = provide.DeskProvider

type TillProvider = provide.TillProvider

type Responsibility = provide.Responsibility

type HandoverProvider = provide.HandoverProvider

type HoldRequest = provide.HoldRequest

type HoldResult = provide.HoldResult

type FreeItem = provide.FreeItem

type HoldableItem = provide.HoldableItem

type ResourceHolds = provide.ResourceHolds

type HoldReleased = provide.HoldReleased

type HoldWatcher = provide.HoldWatcher

type CustomerAsserter = provide.CustomerAsserter

type AmbiguousCustomerError = provide.AmbiguousCustomerError

// The providers' sentinels (raise-go/provide's, so the app's provider and the platform mean the same error).
var (
	ErrNoCard       = provide.ErrNoCard       // a WorkProvider's card id it does not know (404)
	ErrHoldConflict = provide.ErrHoldConflict // a hold moved onto a taken span (409)
	ErrBadPhone     = provide.ErrBadPhone     // a phone number a CustomerAsserter cannot read (422 bad_phone)
)

// The ...ing interfaces of these kinds: the method names are those of the platform's first-party App (internal/providers.Local), so
// one app's code reads the same on either kit. Each may answer nil (not provided).
type (
	// PeopleSourcing offers people candidates (providers.people).
	PeopleSourcing interface {
		PeopleSource(d Deps) PeopleSource
	}
	// Backlinking offers what mentions a record (providers.backlinks); a BacklinkProvider that is also a BacklinkCounter answers the
	// count for delete protection itself.
	Backlinking interface {
		BacklinkProvider(d Deps) BacklinkProvider
	}
	// Holdable offers what people hold of the app (providers.holdings).
	Holdable interface {
		HoldingsProvider(d Deps) HoldingsProvider
	}
	// Agendable offers a person's agenda (providers.agenda).
	Agendable interface {
		AgendaProvider(d Deps) AgendaProvider
	}
	// Workable offers the work items and cards of the workbench (providers.work).
	Workable interface {
		WorkProvider(d Deps) WorkProvider
	}
	// Deskable offers the redemption desk (providers.desk).
	Deskable interface {
		DeskProvider(d Deps) DeskProvider
	}
	// Tillable offers the till (providers.till).
	Tillable interface {
		TillProvider(d Deps) TillProvider
	}
	// HandingOver offers the offboarding handover (providers.handover).
	HandingOver interface {
		HandoverProvider(d Deps) HandoverProvider
	}
	// ResourceHolder offers resource holds (providers.holds).
	ResourceHolder interface {
		ResourceHolds(d Deps) ResourceHolds
	}
	// HoldWatching hears that a hold it made was released (providers.holdWatch).
	HoldWatching interface {
		HoldWatcher(d Deps) HoldWatcher
	}
	// CustomerAsserting offers the customer a phone number is under (providers.customerAssert).
	CustomerAsserting interface {
		CustomerAsserter(d Deps) CustomerAsserter
	}
)

// ReferrerWait is how long a sale waits for the referrer's answer. Attribution is best effort: the distribution app reconciles the
// referrer with the buyer's locked upline when the deal event arrives (ADR-0052 §3), so a missing answer costs nothing that matters.
const ReferrerWait = 300 * time.Millisecond

// LinksLayer is the links layer the app reaches: the platform's over HTTP (Deps.LinksAPI); nothing is found without one.
func (d Deps) LinksLayer() Links {
	if d.LinksAPI != nil {
		return d.LinksAPI
	}
	return noLinks{}
}

type noLinks struct{}

func (noLinks) SearchLinksIn(context.Context, Caller, string, string, int) ([]Link, error) {
	return nil, nil
}
func (noLinks) ResolveLink(context.Context, Caller, string) (Link, bool, error) {
	return Link{}, false, nil
}

// AppInstalled reports whether the tenant has another app installed and switched on: all an app may ask about a peer without
// reading its data. The platform's links layer answers it (Client.Links); false without one or when the platform cannot tell.
func (d Deps) AppInstalled(ctx context.Context, tenant, app string) bool {
	if in, ok := d.LinksAPI.(interface {
		Installed(ctx context.Context, tenant, app string) bool
	}); ok {
		return in.Installed(ctx, tenant, app)
	}
	return false
}
