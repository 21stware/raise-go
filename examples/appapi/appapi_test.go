package appapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/21stware/raise-go"
)

// examples is every example with the routes it must call, in order.
var examples = []struct {
	name   string
	run    func(context.Context, *raise.Client) error
	routes []string
}{
	{"access", Access, []string{"GET /app/v1/access"}},
	{"approvals", Approvals, []string{"POST /app/v1/approvals"}},
	{"audit", Audit, []string{"POST /app/v1/audit"}},
	{"blobs", Blobs, []string{"PUT /app/v1/blobs/objects/apps/shop-sync/notes/a.txt", "GET /app/v1/blobs/objects/apps/shop-sync/notes/a.txt"}},
	{"calendar", Calendar, []string{"POST /app/v1/calendar/projections"}},
	{"customers", Customers, []string{"POST /app/v1/customers/resolve"}},
	{"deals", Deals, []string{"POST /app/v1/deals"}},
	{"directory", Directory, []string{"GET /app/v1/members"}},
	{"drafts", Drafts, []string{"POST /app/v1/drafts"}},
	{"entitlements", Entitlements, []string{"POST /app/v1/entitlements/grants"}},
	{"errors", Errors, []string{"POST /app/v1/errors"}},
	{"events", Events, []string{"POST /app/v1/events/emit"}},
	{"imports", Imports, []string{"POST /app/v1/imports/batch-2026-11/items"}},
	{"installations", Installations, []string{"GET /app/v1/installations"}},
	{"intent", Intent, []string{"POST /app/v1/intent/parse"}},
	{"ledger", Ledger, []string{"POST /app/v1/ledger/posts"}},
	{"links", Links, []string{"GET /app/v1/links"}},
	{"models", Models, []string{"POST /app/v1/models/complete", "POST /app/v1/models/transcribe"}},
	{"notify", Notify, []string{"POST /app/v1/notify"}},
	{"options", Options, []string{"GET /app/v1/options/pickup-points"}},
	{"org", Org, []string{"GET /app/v1/org/members"}},
	{"outreach", Outreach, []string{"POST /app/v1/outreach/send"}},
	{"pay", Pay, []string{"POST /app/v1/pay/orders"}},
	{"peers", Peers, []string{"POST /app/v1/peers/holds/free"}},
	{"realtime", Realtime, []string{"POST /app/v1/realtime/docs"}},
	{"resources", Resources, []string{"POST /app/v1/access/shop-sync.list/check"}},
	{"secrets", Secrets, []string{"POST /app/v1/secrets/encrypt"}},
	{"services", Services, []string{"POST /app/v1/services/campaign/claimed/calls"}},
	{"sites", Sites, []string{"POST /app/v1/sites/summaries"}},
	{"stores", Stores, []string{"GET /app/v1/stores"}},
	{"visitors", Visitors, []string{"GET /app/v1/site-visitors/site_1/vis_1"}},
}

// Every example runs against a fake platform that answers {} (or bytes for blobs.get) and calls exactly the routes it should.
func TestExamplesCallTheirRoutes(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":300,"scope":"","tenant":"t1","environment":"test"}`))
	})
	mux.HandleFunc("/app/v1/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodPut || r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/app/v1/blobs/objects/") {
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := raise.New("cid", "secret", raise.WithBaseURL(srv.URL+"/open/v1"), raise.WithTenant("t1"), raise.WithMaxRetries(0))

	// the models example reads note.wav from the working directory
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.wav"), []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	for _, ex := range examples {
		mu.Lock()
		seen = nil
		mu.Unlock()
		if err := ex.run(context.Background(), c); err != nil {
			t.Errorf("%s: %v", ex.name, err)
			continue
		}
		mu.Lock()
		got := append([]string(nil), seen...)
		mu.Unlock()
		if !reflect.DeepEqual(got, ex.routes) {
			t.Errorf("%s called %v, want %v", ex.name, got, ex.routes)
		}
	}
}

// Every resource of client.App has an example, and every example in the source is in the table above.
func TestEveryResourceHasAnExample(t *testing.T) {
	src, err := os.ReadFile("appapi.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	api := reflect.TypeOf(raise.AppAPI{})
	for i := 0; i < api.NumField(); i++ {
		if f := api.Field(i).Name; !strings.Contains(text, "c.App."+f+".") {
			t.Errorf("no example calls client.App.%s", f)
		}
	}
	if n := strings.Count(text, "\t// example: "); n != len(examples) {
		t.Errorf("appapi.go has %d example regions, the table %d", n, len(examples))
	}
	for _, ex := range examples {
		if !strings.Contains(text, "\t// example: "+ex.name+"\n") {
			t.Errorf("no region named %s", ex.name)
		}
	}
}
