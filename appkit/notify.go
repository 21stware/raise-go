package appkit

import (
	"context"
	"log/slog"
	"strings"
)

// What a notice is about, for the channels that push messages outside the platform. The class travels in the context of the Notify
// call (WithNoticeClass), so Tell keeps its signature; every other notice is classified by the platform from its sender and severity.
const (
	ClassApprovalTask   = "approval.task"   // something waits for the person's decision
	ClassApprovalResult = "approval.result" // the outcome of an approval the person started
	ClassMention        = "mention"         // someone mentioned the person in a document
)

type noticeClassKey struct{}

// WithNoticeClass tags the notices sent with the returned context.
func WithNoticeClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, noticeClassKey{}, class)
}

// NoticeClass is the class set with WithNoticeClass ("" when none).
func NoticeClass(ctx context.Context) string {
	c, _ := ctx.Value(noticeClassKey{}).(string)
	return c
}

// Notify sends a notice over /app/v1/notify (Deps.Notify = cl.Notify). Best effort: a refusal is logged, not returned. The class of the
// context (WithNoticeClass) goes with it.
func (cl *Client) Notify(ctx context.Context, tenant string, users []string, title, body, link string) {
	req := map[string]any{"users": users, "title": title}
	if body != "" {
		req["body"] = body
	}
	if link != "" {
		req["link"] = link
	}
	if class := NoticeClass(ctx); class != "" {
		req["class"] = class
	}
	if err := cl.call(ctx, Caller{Tenant: tenant}, "POST", "/app/v1/notify", nil, req, nil); err != nil {
		slog.WarnContext(ctx, "appkit: notice not sent", "tenant", tenant, "people", len(users), "title", title, "err", err)
	}
}

// Tell sends a notice to people of a tenant (nil-safe, best effort; blanks and repeats among users are dropped). link is a route
// inside the app that sends it.
func (d Deps) Tell(ctx context.Context, tenant string, users []string, title, body, link string) {
	if users = uniqueUsers(users); d.Notify != nil && len(users) > 0 {
		d.Notify(ctx, tenant, users, title, body, link)
	}
}

func uniqueUsers(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
