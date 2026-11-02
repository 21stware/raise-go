package appkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/21stware/raise-go/access"
	"github.com/21stware/raise-go/k3"
)

// The kernel owns the entries, apps re-read (ADR-0130 §2.6.2, implementation note of 2026-11-02): the tree client names roots and
// nodes, reads a node back and pages a person's raw levels; an AccessListening app hears access.changed.

func TestTreeNamesRootsAndNodes(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(204, ""))
	if err := cl.Tree().PutRoot(ctx, "t1", "wiki.space", "s1", sdk.RootSpec{Name: "销售资料"}); err != nil {
		t.Fatal(err)
	}
	if b := jsonMap(t, s.body); b["name"] != "销售资料" {
		t.Fatalf("root %q", s.body)
	}
	cl, s = platform(t, status(200, `{"changed":{}}`))
	if _, err := cl.Tree().PutNode(ctx, "t1", "wiki.page", "n1", sdk.NodeSpec{Root: "s1", Inherit: true, Name: "开店清单"}); err != nil {
		t.Fatal(err)
	}
	if b := jsonMap(t, s.body); b["name"] != "开店清单" {
		t.Fatalf("node %q", s.body)
	}
}

func TestTreeNode(t *testing.T) {
	ctx := context.Background()
	cl, s := platform(t, status(200, `{"type":"wiki.page","id":"n2","name":"子页","root":"s1","parent":"n1","inherit":false,"set":"x","level":"manage"}`))
	n, err := cl.Tree().(sdk.NodeReader).Node(ctx, "t1", "wiki.page", "n2")
	if err != nil || n.ID != "n2" || n.Root != "s1" || n.Parent != "n1" || n.Inherit || n.Name != "子页" {
		t.Fatalf("%+v %v", n, err)
	}
	if s.method != "GET" || s.path != "/app/v1/access/wiki.page/n2" || s.tenant != "t1" || s.query != "" {
		t.Fatalf("%+v", s)
	}
	cl, _ = platform(t, status(404, `{"error":"not found"}`))
	if _, err := cl.Tree().(sdk.NodeReader).Node(ctx, "t1", "wiki.page", "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
}

func TestTreeLevels(t *testing.T) {
	ctx := context.Background()
	var gotCaller Caller
	cl, s := platform(t, status(200, `{"ids":["a","b"],"next":"b","levels":{"a":"manage","b":"view"}}`))
	cl.Token = func(_ context.Context, c Caller) (string, error) { gotCaller = c; return "tok", nil }
	c := Caller{Tenant: "t1", User: "u1"}
	lv, err := cl.Tree().(sdk.Lister).Levels(ctx, c, "wiki.page", sdk.View, "a0")
	if err != nil || len(lv.Levels) != 2 || lv.Levels["a"] != sdk.Manage || lv.Levels["b"] != sdk.View || lv.Next != "b" {
		t.Fatalf("%+v %v", lv, err)
	}
	if s.method != "GET" || s.path != "/app/v1/access/wiki.page/visible" || s.query != "cursor=a0&level=view&limit=1000&raw=true&user=u1" || gotCaller.User != "u1" {
		t.Fatalf("%+v", s)
	}
	// a platform that predates raw levels answers ids alone: refused, never read as raw levels
	cl, _ = platform(t, status(200, `{"ids":["a"],"next":""}`))
	if _, err := cl.Tree().(sdk.Lister).Levels(ctx, c, "wiki.page", sdk.View, ""); err == nil {
		t.Fatal("ids without levels")
	}
	if _, err := cl.Tree().(sdk.Lister).Levels(ctx, c, "wiki.page", 0, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("min 0: %v", err)
	}
}

type accessCall struct{ tenant, typ, id string }

type listeningApp struct {
	kbAppImpl
	calls chan accessCall
	err   error
}

func (a *listeningApp) AccessChanged(_ context.Context, _ Deps, tenant, typ, id string) error {
	a.calls <- accessCall{tenant, typ, id}
	return a.err
}

func TestServeAccessChanged(t *testing.T) {
	sv := AccessChangedService()
	if sv.Endpoint != "/k3/services/access-changed" || len(sv.Trigger) != 1 || sv.Trigger[0] != "event:access.changed" {
		t.Fatalf("%+v", sv)
	}
	app := &listeningApp{kbAppImpl: kbAppImpl{manifest: kbManifest(nil, sv)}, calls: make(chan accessCall, 4)}
	p := newPlatformSrv(t, app, Deps{})

	code, body := p.call(sv.Endpoint, k3.ViaService, "t1", "", runBody("t1", k3.EventAccessChanged, `{"type":"wiki.page","id":"n1"}`))
	if code != 200 || strings.TrimSpace(body) != `{"ok":true}` {
		t.Fatalf("%d %s", code, body)
	}
	if c := <-app.calls; c != (accessCall{"t1", "wiki.page", "n1"}) {
		t.Fatalf("%+v", c)
	}
	for name, data := range map[string]string{"no id": `{"type":"wiki.page"}`, "no type": `{"id":"n1"}`, "not an object": `"x"`} {
		if code, _ := p.call(sv.Endpoint, k3.ViaService, "t1", "", runBody("t1", k3.EventAccessChanged, data)); code != 422 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	if len(app.calls) != 0 {
		t.Fatal("a malformed event reached the app")
	}
	if code, _ := p.call(sv.Endpoint, k3.ViaService, "t1", "", runBody("t2", k3.EventAccessChanged, `{"type":"wiki.page","id":"n1"}`)); code != 403 {
		t.Fatalf("another tenant: %d", code)
	}
	// a failure is retried by the platform (500)
	app.err = errors.New("kernel away")
	if code, _ := p.call(sv.Endpoint, k3.ViaService, "t1", "", runBody("t1", k3.EventAccessChanged, `{"type":"wiki.page","id":"n1"}`)); code != 500 {
		t.Fatalf("failure: %d", code)
	}
}
