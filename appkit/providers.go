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
