// Package appapi holds the request examples of the developer docs' App API page (docs/developer/zh/app-api.md and
// docs/developer/en/app-api.md): one function per resource of client.App, each taking the client and the request's context.
//
// The code between "// example: <name>" and "// end example", one tab less indented, is what the docs show. Two tests keep the docs and
// this file the same and the examples working:
//   - appapi_test.go (this module) runs every function against a fake platform and checks the route it called;
//   - docs/api/devdocs_examples_test.go (the platform's module) checks that every Go block of both pages is one of these regions, that
//     every region is on both pages, and that the two pages show the same code.
//
// Change an example here first, then copy the region into both pages.
package appapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/21stware/raise-go"
)

// Access asks one permission point of this app for the caller.
func Access(ctx context.Context, c *raise.Client) error {
	// example: access
	acc, err := c.App.Access.Check(ctx, &raise.AppAccessCheckParams{Point: "app.shop-sync.orders.read"})
	if err != nil {
		return err
	}
	fmt.Println(acc.Allowed, acc.Units)
	// end example
	return nil
}

// Approvals submits an object of this app for approval.
func Approvals(ctx context.Context, c *raise.Client) error {
	// example: approvals
	ref, err := c.App.Approvals.Submit(ctx, &raise.AppApprovalSubmitRequest{
		Kind:    "refund",
		Subject: "k3://shop-sync/refund/rf_1",
	})
	if err != nil {
		return err
	}
	fmt.Println(ref.Id, ref.Status)
	// end example
	return nil
}

// Audit appends one entry to the tenant's audit chain.
func Audit(ctx context.Context, c *raise.Client) error {
	// example: audit
	_, err := c.App.Audit.Append(ctx, &raise.AppAuditRequest{
		Action:   "shop-sync.order.cancel",
		Resource: "k3://shop-sync/order/1001",
		Diff:     map[string]any{"status": "cancelled"},
	})
	// end example
	return err
}

// Blobs stores bytes in the app's folder and reads them back.
func Blobs(ctx context.Context, c *raise.Client) error {
	// example: blobs
	data := []byte("hello")
	if _, err := c.App.Blobs.Put(ctx, "apps/shop-sync/notes/a.txt", bytes.NewReader(data), int64(len(data)), "text/plain"); err != nil {
		return err
	}
	obj, err := c.App.Blobs.Get(ctx, "apps/shop-sync/notes/a.txt")
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	if _, err := io.Copy(os.Stdout, obj.Body); err != nil {
		return err
	}
	// end example
	return nil
}

// Calendar puts one fact of this app on its owner's calendar.
func Calendar(ctx context.Context, c *raise.Client) error {
	// example: calendar
	start := time.Date(2026, 11, 3, 15, 0, 0, 0, time.Local)
	_, err := c.App.Calendar.Push(ctx, &raise.AppCalendarPush{
		Ref:     "k3://shop-sync/visit/v_1",
		Version: 1,
		Item: &raise.AppCalendarProjection{
			Ref: "k3://shop-sync/visit/v_1", Owner: "u_1", Title: "到店取货",
			Start: start, End: start.Add(30 * time.Minute),
		},
	})
	// end example
	return err
}

// Customers finds or creates the customer behind a phone number.
func Customers(ctx context.Context, c *raise.Client) error {
	// example: customers
	ref, err := c.App.Customers.Resolve(ctx, &raise.AppCustomerResolveRequest{
		Identities: []raise.AppCustomerIdentity{{Kind: "phone", Value: "13800000000"}},
	}, nil)
	if err != nil {
		return err
	}
	fmt.Println(ref.Id, ref.Created)
	// end example
	return nil
}

// Deals hands a sale of this app to the platform.
func Deals(ctx context.Context, c *raise.Client) error {
	// example: deals
	_, err := c.App.Deals.Emit(ctx, &raise.AppDealEvent{
		Key:         "sale-1001",
		Kind:        "sale",
		AmountCents: 9900,
		At:          time.Now(),
		Customer:    "cu_1",
		Source:      raise.AppDealSource{App: "shop-sync", Ref: "k3://shop-sync/order/1001"},
	})
	// end example
	return err
}

// Directory lists the tenant's members.
func Directory(ctx context.Context, c *raise.Client) error {
	// example: directory
	members, err := c.App.Directory.Members(ctx)
	if err != nil {
		return err
	}
	for _, m := range members.Items {
		fmt.Println(m.UserId, m.Name)
	}
	// end example
	return nil
}

// Drafts drafts a change for a member to review.
func Drafts(ctx context.Context, c *raise.Client) error {
	// example: drafts
	draft, err := c.App.Drafts.Create(ctx, &raise.AppDraftCreateRequest{
		Summary: "补录一笔订单",
		Items: []raise.AppDraftItemInput{
			{Op: "shop-sync.create_order", Proposed: map[string]any{"customer": "cu_1", "amountCents": 9900}},
		},
	})
	if err != nil {
		return err
	}
	fmt.Println(draft.Id, draft.Status)
	// end example
	return nil
}

// Entitlements grants a customer one entitlement from a template.
func Entitlements(ctx context.Context, c *raise.Client) error {
	// example: entitlements
	res, err := c.App.Entitlements.Grant(ctx, &raise.AppEntitlementGrantRequest{
		Template: "tpl_1",
		Holder:   "cu_1",
		Key:      "order-1001",
	})
	if err != nil {
		return err
	}
	fmt.Println(len(res.Grants), res.Replay)
	// end example
	return nil
}

// Errors reports an error of the app to the developer's error list.
func Errors(ctx context.Context, c *raise.Client) error {
	// example: errors
	_, err := c.App.Errors.Report(ctx, &raise.AppErrorReport{
		Errors: []raise.AppErrorOccurrence{{Kind: "panic", Message: "nil map", Route: "/api/orders", At: time.Now()}},
	})
	// end example
	return err
}

// Events emits one open event declared in the manifest.
func Events(ctx context.Context, c *raise.Client) error {
	// example: events
	ack, err := c.App.Events.Emit(ctx, &raise.AppEmitRequest{
		Type: "shop-sync.order.created",
		Key:  "order-1001",
		Data: map[string]any{"orderId": "1001"},
	})
	if err != nil {
		return err
	}
	fmt.Println(ack.Written)
	// end example
	return nil
}

// Imports reports one chunk of an import of old records.
func Imports(ctx context.Context, c *raise.Client) error {
	// example: imports
	batch, err := c.App.Imports.Report(ctx, "batch-2026-11", &raise.AppImportReport{
		Items: []raise.AppImportReportItem{{
			Key: "row-1", Kind: "order", Status: "ok", Digest: "sha256:…",
			RecordId: "1001", BusinessAt: time.Now(),
		}},
	})
	if err != nil {
		return err
	}
	fmt.Println(batch.RowsOk, batch.RowsFailed)
	// end example
	return nil
}

// Installations lists the tenants that installed the app.
func Installations(ctx context.Context, c *raise.Client) error {
	// example: installations
	inst, err := c.App.Installations.List(ctx)
	if err != nil {
		return err
	}
	fmt.Println(inst.Tenants)
	// end example
	return nil
}

// Intent turns a person's sentence into a proposal for one of the app's ops.
func Intent(ctx context.Context, c *raise.Client) error {
	// example: intent
	res, err := c.App.Intent.Parse(ctx, &raise.AppIntentRequest{
		Op:   "shop-sync.create_order",
		Text: "张三明天下午买两盒，99 元",
	})
	if err != nil {
		return err
	}
	fmt.Println(res.Input, res.Missing)
	// end example
	return nil
}

// Ledger posts one entry of this app to the tenant's books.
func Ledger(ctx context.Context, c *raise.Client) error {
	// example: ledger
	posted, err := c.App.Ledger.Post(ctx, &raise.AppLedgerPostRequest{
		SourceApp:   "shop-sync",
		SourceRef:   "k3://shop-sync/order/1001",
		Kind:        "purchase",
		Direction:   "in",
		AmountCents: 9900,
		Method:      "wechat",
	})
	if err != nil {
		return err
	}
	fmt.Println(posted.Key)
	// end example
	return nil
}

// Links searches the records of another app.
func Links(ctx context.Context, c *raise.Client) error {
	// example: links
	found, err := c.App.Links.Search(ctx, &raise.AppLinksSearchParams{App: "wiki", Q: "退货"})
	if err != nil {
		return err
	}
	for _, l := range found.Items {
		fmt.Println(l.Ref, l.Title)
	}
	// end example
	return nil
}

// Models asks the tenant's service model, and turns a recording into text.
func Models(ctx context.Context, c *raise.Client) error {
	// example: models
	out, err := c.App.Models.Complete(ctx, &raise.AppModelRequest{Scene: "order-summary", User: "把这条留言缩成一句话：……"})
	if err != nil {
		return err
	}
	fmt.Println(out.Text)

	audio, err := os.Open("note.wav")
	if err != nil {
		return err
	}
	defer audio.Close()
	heard, err := c.App.Models.Transcribe(ctx, audio, -1, "audio/wav")
	if err != nil {
		return err
	}
	fmt.Println(heard.Text)
	// end example
	return nil
}

// Notify sends a notice to members.
func Notify(ctx context.Context, c *raise.Client) error {
	// example: notify
	_, err := c.App.Notify.Send(ctx, &raise.AppNotifyRequest{
		Users: []string{"u_1"},
		Title: "新订单 1001",
		Link:  "/apps/shop-sync/orders/1001",
	})
	// end example
	return err
}

// Options reads one of the app's option lists.
func Options(ctx context.Context, c *raise.Client) error {
	// example: options
	list, err := c.App.Options.Get(ctx, "pickup-points", nil)
	if err != nil {
		return err
	}
	fmt.Println(list.Items)
	// end example
	return nil
}

// Org lists the members the caller may see for one of the app's points.
func Org(ctx context.Context, c *raise.Client) error {
	// example: org
	page, err := c.App.Org.Members(ctx, &raise.AppOrgMembersParams{Point: "app.shop-sync.orders.read"})
	if err != nil {
		return err
	}
	for _, m := range page.Items {
		fmt.Println(m.Id, m.Name, m.Units)
	}
	// end example
	return nil
}

// Outreach sends a customer a message from a template.
func Outreach(ctx context.Context, c *raise.Client) error {
	// example: outreach
	msg, err := c.App.Outreach.Send(ctx, &raise.AppOutreachSendRequest{
		Customer: "cu_1",
		Channel:  "sms",
		Purpose:  "service",
		Template: "tpl_pickup",
		Key:      "pickup-1001",
		Vars:     map[string]string{"order": "1001"},
	})
	if err != nil {
		return err
	}
	fmt.Println(msg.Id, msg.Existing)
	// end example
	return nil
}

// Pay opens a payment order.
func Pay(ctx context.Context, c *raise.Client) error {
	// example: pay
	order, err := c.App.Pay.Create(ctx, &raise.AppPayCreateRequest{
		Channel:     "wechat",
		AmountCents: 9900,
		Subject:     "订单 1001",
		SourceRef:   "k3://shop-sync/order/1001",
	})
	if err != nil {
		return err
	}
	fmt.Println(order.Id, order.Status, order.CodeUrl)
	// end example
	return nil
}

// Peers asks another app, through the platform, which rooms are free.
func Peers(ctx context.Context, c *raise.Client) error {
	// example: peers
	from := time.Date(2026, 11, 3, 14, 0, 0, 0, time.Local)
	free, err := c.App.Peers.HoldsFree(ctx, &raise.AppPeerHoldsFreeRequest{
		Items: []string{"room-1", "room-2"},
		From:  from,
		To:    from.Add(time.Hour),
	})
	if err != nil {
		return err
	}
	for _, it := range free.Items {
		fmt.Println(it.Item, it.Free)
	}
	// end example
	return nil
}

// Realtime creates a shared document of a kind the manifest declares.
func Realtime(ctx context.Context, c *raise.Client) error {
	// example: realtime
	doc, err := c.App.Realtime.CreateDoc(ctx, &raise.AppRealtimeDocCreate{
		Kind:     "note",
		Resource: "nb_1",
		Data:     map[string]any{"title": "周会"},
	})
	if err != nil {
		return err
	}
	fmt.Println(doc.Id, doc.HeadSeq)
	// end example
	return nil
}

// Resources checks a person's access to the app's resources in the access kernel.
func Resources(ctx context.Context, c *raise.Client) error {
	// example: resources
	ans, err := c.App.Resources.Check(ctx, "shop-sync.list", &raise.AppResourceCheckRequest{
		Ids:   []string{"ls_1", "ls_2"},
		Level: "edit",
	})
	if err != nil {
		return err
	}
	fmt.Println(ans.Levels)
	// end example
	return nil
}

// Secrets encrypts a value for one of the app's subjects.
func Secrets(ctx context.Context, c *raise.Client) error {
	// example: secrets
	sealed, err := c.App.Secrets.Encrypt(ctx, &raise.AppSecretEncryptRequest{
		Subject:   "recipient:f_123",
		Plaintext: "MTM4MDAwMDAwMDA=", // base64
	})
	if err != nil {
		return err
	}
	fmt.Println(sealed.Ciphertext)
	// end example
	return nil
}

// Services starts a run of another app's service.
func Services(ctx context.Context, c *raise.Client) error {
	// example: services
	run, err := c.App.Services.Call(ctx, "campaign", "claimed", &raise.AppServiceCallRequest{
		Input: map[string]any{"customer": "cu_1"},
	})
	if err != nil {
		return err
	}
	fmt.Println(run.Id, run.Status)
	// end example
	return nil
}

// Sites reads the usage summary of a site.
func Sites(ctx context.Context, c *raise.Client) error {
	// example: sites
	sums, err := c.App.Sites.Summaries(ctx, &raise.AppSiteSummariesRequest{
		Requests: []raise.AppSiteUsageRequest{{Site: "site_1", From: time.Now().AddDate(0, 0, -7)}},
	})
	if err != nil {
		return err
	}
	fmt.Println(len(sums.Summaries))
	// end example
	return nil
}

// Stores lists the tenant's stores.
func Stores(ctx context.Context, c *raise.Client) error {
	// example: stores
	stores, err := c.App.Stores.List(ctx, nil)
	if err != nil {
		return err
	}
	for _, s := range stores.Stores {
		fmt.Println(s.Id, s.Name)
	}
	// end example
	return nil
}

// Visitors reads which customer a site visitor is.
func Visitors(ctx context.Context, c *raise.Client) error {
	// example: visitors
	v, err := c.App.Visitors.Get(ctx, "site_1", "vis_1")
	if err != nil {
		return err
	}
	fmt.Println(v.Customer, v.Verified)
	// end example
	return nil
}
