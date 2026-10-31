package appkit

import (
	"context"
	"time"
)

// CalendarProjection is an app's item on people's calendars (the JSON of the platform's projection).
type CalendarProjection struct {
	Ref       string    `json:"ref"` // the fact: k3://{app}/{type}/{id}; one projection per app and ref
	Route     string    `json:"route,omitempty"`
	Owner     string    `json:"owner"`
	Attendees []string  `json:"attendees,omitempty"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	TZ        string    `json:"tz,omitempty"`
	AllDay    bool      `json:"allDay,omitempty"`
	Title     string    `json:"title"`
	TitleEN   string    `json:"titleEn,omitempty"`
	Place     string    `json:"place,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Privacy   string    `json:"privacy,omitempty"` // normal (default), private, health
	State     string    `json:"state,omitempty"`
}

// CalendarPush hands one push of the app's calendar projection to /app/v1/calendar/projections (ADR-0130 §3.2 H7). A later version
// wins; the answer is the calendar item's id, or "stale" when a newer push was applied already. item nil cancels the projection.
// The kit's outbox has no handler registry yet, so the app calls this itself: the push is not queued in its transaction.
func (cl *Client) CalendarPush(ctx context.Context, tenant, ref string, version int64, item *CalendarProjection) (string, error) {
	var out struct {
		Result string `json:"result"`
	}
	body := struct {
		Ref     string              `json:"ref"`
		Version int64               `json:"version"`
		Item    *CalendarProjection `json:"item"`
	}{ref, version, item}
	err := cl.call(ctx, Caller{Tenant: tenant}, "POST", "/app/v1/calendar/projections", nil, body, &out)
	return out.Result, err
}
