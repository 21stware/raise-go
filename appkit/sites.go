package appkit

import (
	"context"
	"time"
)

// Sites is what apps read of the tenant's sites (ADR-0066 §1): a site's name and how many requests it served over some days. The
// campaign app shows them next to a campaign instead of joining the site's tables. Client.Sites answers it over
// POST /app/v1/sites/summaries.
type Sites interface {
	// Summaries answers each request in order; a site that does not exist answers an empty name and no requests.
	Summaries(ctx context.Context, c Caller, reqs []SiteUsageReq) ([]SiteSummary, error)
}

// SiteUsageReq asks for a site's requests from the day of From to the day of To (nil: to today).
type SiteUsageReq struct {
	Site string     `json:"site"`
	From time.Time  `json:"from"`
	To   *time.Time `json:"to,omitempty"`
}

// SiteSummary answers a SiteUsageReq.
type SiteSummary struct {
	Site     string `json:"site"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	// State is what visitors get now (ADR-0102 §4): SiteLive (LiveNumber is served), SiteUnpublished (nothing was ever served),
	// SiteEnded (taken down: the 「活动已结束」 page, since EndedAt) or SiteDisabled (the address does not open). "" for a site that
	// does not exist.
	State      string     `json:"state,omitempty"`
	LiveNumber int        `json:"liveNumber,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
}

// The states of a site in a SiteSummary.
const (
	SiteLive        = "live"
	SiteUnpublished = "unpublished"
	SiteEnded       = "ended"
	SiteDisabled    = "disabled"
)

// Sites is the tenant's sites over /app/v1/sites/summaries (ADR-0130 P6): Deps.Sites = cl.Sites(). One answer per request, in order;
// at most 200 requests per call.
func (cl *Client) Sites() Sites { return sites{cl} }

type sites struct{ cl *Client }

var _ Sites = sites{}

func (s sites) Summaries(ctx context.Context, c Caller, reqs []SiteUsageReq) ([]SiteSummary, error) {
	if reqs == nil {
		reqs = []SiteUsageReq{}
	}
	var out struct {
		Summaries []SiteSummary `json:"summaries"`
	}
	err := s.cl.call(ctx, c, "POST", "/app/v1/sites/summaries", nil, map[string]any{"requests": reqs}, &out)
	return out.Summaries, err
}
