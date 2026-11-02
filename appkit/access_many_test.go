package appkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAccessManyPostsPointsInBatches(t *testing.T) {
	calls := 0
	var s *seen
	cl, s := platform(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in struct {
			Points []AccessPoint `json:"points"`
		}
		_ = json.NewDecoder(strings.NewReader(s.body)).Decode(&in)
		res := make([]map[string]any, len(in.Points))
		for i, p := range in.Points {
			res[i] = map[string]any{"Managed": true, "Allowed": strings.HasSuffix(p.Point, ".yes"), "Subject": "mia"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": res})
	})
	points := make([]AccessPoint, AccessPointsMax+3)
	for i := range points {
		points[i] = AccessPoint{Point: fmt.Sprintf("app.a.p%d.no", i)}
	}
	points[1].Point, points[AccessPointsMax+1].Point = "app.a.x.yes", "app.a.y.yes"
	got, err := AccessMany(context.Background(), cl.Access(), "t1", "mia", nil, points)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(got) != len(points) || !got[1].Allowed || got[0].Allowed || !got[AccessPointsMax+1].Allowed || got[2].Subject != "mia" {
		t.Fatalf("calls=%d len=%d %+v", calls, len(got), got[:3])
	}
	if s.method != "POST" || s.path != "/app/v1/access/points" || s.tenant != "t1" {
		t.Fatalf("request %+v", s)
	}
}

func TestAccessManyRefusedAndShort(t *testing.T) {
	cl, _ := platform(t, status(403, `{"error":"this permission is not this app's","code":"forbidden"}`))
	if _, err := AccessMany(context.Background(), cl.Access(), "t1", "mia", nil, []AccessPoint{{Point: "app.b.x"}}); err == nil {
		t.Fatal("a 403 is an error")
	}
	cl, _ = platform(t, status(200, `{"results":[]}`))
	if _, err := AccessMany(context.Background(), cl.Access(), "t1", "mia", nil, []AccessPoint{{Point: "app.a.x"}}); err == nil {
		t.Fatal("fewer results than points is an error")
	}
}

// oneByOne is an Access without AccessMany (a test double written before it).
type oneByOne struct{ asked []string }

func (o *oneByOne) Access(_ context.Context, _, subject string, _ []string, point, typeID string) (AccessResult, error) {
	o.asked = append(o.asked, point+"|"+typeID)
	return AccessResult{Managed: true, Allowed: point == "app.a.b.read", All: true, Subject: subject}, nil
}
func (o *oneByOne) UnitOf(context.Context, string, string) (string, error) { return "", nil }

func TestAccessManyFallsBackToOneCallPerPoint(t *testing.T) {
	o := &oneByOne{}
	got, err := AccessMany(context.Background(), o, "t1", "mia", nil, []AccessPoint{{Point: "app.a.x", Type: "ty"}, {Point: "app.a.b.read"}})
	if err != nil || len(got) != 2 || got[0].Allowed || !got[1].Allowed || strings.Join(o.asked, ",") != "app.a.x|ty,app.a.b.read|" {
		t.Fatalf("%v %+v %v", err, got, o.asked)
	}
}

// batcher counts batch calls.
type batcher struct {
	oneByOne
	batches int
}

func (b *batcher) AccessMany(ctx context.Context, tenant, subject string, roles []string, points []AccessPoint) ([]AccessResult, error) {
	b.batches++
	out := make([]AccessResult, len(points))
	for i, p := range points {
		out[i], _ = b.Access(ctx, tenant, subject, roles, p.Point, p.Type)
	}
	return out, nil
}

func TestCheckManyMatchesCheckWithOneQuestion(t *testing.T) {
	perms := []string{"b.read", "b.write", "c.write"}
	for _, c := range []Caller{
		{Tenant: "t1", User: "mia"},
		{Tenant: "t1", User: "boss", Roles: []string{RoleTenantAdmin}},
		{Tenant: "t1", User: "aud", Roles: []string{RoleTenantAuditor}},
		{Tenant: "t1", User: "mia", Agent: "ag_1"},
	} {
		b := &batcher{}
		az := Authz{App: "a", Perms: b, Defaults: map[string][]string{"c.write": {"tenant.member"}}}
		many, err := az.CheckMany(context.Background(), c, perms)
		if err != nil {
			t.Fatal(err)
		}
		if b.batches > 1 {
			t.Fatalf("%s: %d batches, want at most one", c.User, b.batches)
		}
		for i, p := range perms {
			one, _ := Authz{App: "a", Perms: &oneByOne{}, Defaults: az.Defaults}.Check(context.Background(), c, p)
			if fmt.Sprint(one) != fmt.Sprint(many[i]) {
				t.Fatalf("%s %s: CheckMany %+v, Check %+v", c.User, p, many[i], one)
			}
		}
	}
	// basic mode without a permission model: Defaults decide
	az := Authz{App: "a", Defaults: map[string][]string{"c.write": {"tenant.member:self"}}}
	g, err := az.CheckMany(context.Background(), Caller{Tenant: "t1", User: "mia", Roles: []string{"tenant.member"}}, []string{"c.write", "b.read"})
	if err != nil || !g[0].Allowed || !g[0].Self || g[0].All || g[1].Allowed {
		t.Fatalf("%v %+v", err, g)
	}
}
