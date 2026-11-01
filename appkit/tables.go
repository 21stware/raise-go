package appkit

import (
	"context"
	"errors"
	"net/http"

	"github.com/21stware/raise-go/k3"
	"github.com/21stware/raise-go/provide"
)

// System tables (providers.tables, ADR-0096 §2.3, ADR-0130 P11 S2): an app shows its own data in the 多维表 as tables by implementing
// TableProviding and declaring providers.tables. The contract is raise-go/provide's, the platform's first-party apps answer the same.

type (
	TableCaller    = provide.TableCaller
	TableField     = provide.TableField
	TableSpec      = provide.TableSpec
	TableQuery     = provide.TableQuery
	RowWrite       = provide.RowWrite
	TableProvider  = provide.TableProvider
	ObjectRecord   = provide.ObjectRecord
	ObjectFilter   = provide.ObjectFilter
	ObjectPage     = provide.ObjectPage
	ObjectFieldDef = provide.ObjectFieldDef
	ObjectProblem  = provide.ObjectProblem
	ObjectError    = provide.ObjectError
)

// The system tables' names, write kinds and callers (provide's).
const (
	SystemTablePrefix   = provide.SystemTablePrefix
	TableInsert         = provide.TableInsert
	TableUpdate         = provide.TableUpdate
	TableDelete         = provide.TableDelete
	TableViaStaff       = provide.TableViaStaff
	TableViaVisitor     = provide.TableViaVisitor
	TableViaIntegration = provide.TableViaIntegration
)

// TableProviding offers system tables (providers.tables).
type TableProviding interface {
	TableProvider(d Deps) TableProvider
}

// TableRefused is the 404 a provider answers for a row the caller cannot reach (or that does not exist).
func TableRefused() error { return provide.TableRefused() }

// TableNotWritable is the refusal of a write the table does not map to an op.
func TableNotWritable(table, op string) error { return provide.TableNotWritable(table, op) }

// TableInvalid is a 422 for one column.
func TableInvalid(field, msg string) error { return provide.TableInvalid(field, msg) }

// The paths of providers.tables (the platform's internal/providers/remote_tables.go is the caller's side):
//
//	POST /k3/providers/tables/specs  {caller}                → {"tables": [TableSpec]}
//	POST /k3/providers/tables/list   {caller, table, query}  → ObjectPage
//	POST /k3/providers/tables/get    {caller, table, id}     → ObjectRecord
//	POST /k3/providers/tables/write  {caller, table, write}  → ObjectRecord
//
// caller is {via, customer?, site?, agent?, opVia?}; the person of a staff call is the assertion's. A refusal (*ObjectError) answers
// its status with {"error", "code", "problems", "refused": true}.
const (
	PathTableSpecs = "/k3/providers/tables/specs"
	PathTableList  = "/k3/providers/tables/list"
	PathTableGet   = "/k3/providers/tables/get"
	PathTableWrite = "/k3/providers/tables/write"
)

// The wire forms (the platform's providers.TableCallerWire, TableCall, TableSpecsAnswer, TableRefusal).
type (
	tableCallerWire struct {
		Via      string `json:"via"`
		Customer string `json:"customer,omitempty"`
		Site     string `json:"site,omitempty"`
		Agent    string `json:"agent,omitempty"`
		OpVia    string `json:"opVia,omitempty"`
	}
	tableCall struct {
		Caller tableCallerWire `json:"caller"`
		Table  string          `json:"table,omitempty"`
		ID     string          `json:"id,omitempty"`
		Query  *TableQuery     `json:"query,omitempty"`
		Write  *RowWrite       `json:"write,omitempty"`
	}
	tableSpecsAnswer struct {
		Tables []TableSpec `json:"tables"`
	}
	tableRefusal struct {
		Error    string          `json:"error"`
		Code     string          `json:"code,omitempty"`
		Problems []ObjectProblem `json:"problems,omitempty"`
		Refused  bool            `json:"refused"`
	}
)

// serveTables answers providers.tables for an app that provides and declares it.
func serveTables(mux *http.ServeMux, a App, decl *k3.ProvidersDecl, d Deps) {
	p, ok := a.(TableProviding)
	if !ok || !decl.Has("tables") {
		return
	}
	tp := p.TableProvider(d)
	if tp == nil {
		return
	}
	handle := func(path string, answer func(ctx context.Context, c TableCaller, in tableCall) (any, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var in tableCall
			if !platformJSON(w, r, k3.ViaProvider, &in) {
				return
			}
			pc, _ := PlatformOf(r.Context())
			switch in.Caller.Via {
			case TableViaStaff:
				if pc.Caller.User == "" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "a staff call answers for a person"})
					return
				}
			case TableViaVisitor, TableViaIntegration:
			default:
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown caller"})
				return
			}
			c := pc.Caller
			c.Tenant = pc.Tenant
			c.Agent, c.Via = in.Caller.Agent, in.Caller.OpVia
			out, err := answer(r.Context(), TableCaller{Caller: c, Via: in.Caller.Via, Customer: in.Caller.Customer, Site: in.Caller.Site}, in)
			if err != nil {
				var oe *ObjectError
				if errors.As(err, &oe) {
					status := oe.Status
					if status < 400 || status > 599 {
						status = http.StatusInternalServerError
					}
					writeJSON(w, status, tableRefusal{Error: oe.Message, Code: oe.Code, Problems: oe.Problems, Refused: true})
					return
				}
				writeProviderErr(w, d, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	handle(PathTableSpecs, func(ctx context.Context, c TableCaller, _ tableCall) (any, error) {
		specs, err := tp.Tables(ctx, c)
		return tableSpecsAnswer{Tables: specs}, err
	})
	handle(PathTableList, func(ctx context.Context, c TableCaller, in tableCall) (any, error) {
		var q TableQuery
		if in.Query != nil {
			q = *in.Query
		}
		return tp.List(ctx, c, in.Table, q)
	})
	handle(PathTableGet, func(ctx context.Context, c TableCaller, in tableCall) (any, error) {
		return tp.Get(ctx, c, in.Table, in.ID)
	})
	handle(PathTableWrite, func(ctx context.Context, c TableCaller, in tableCall) (any, error) {
		var rw RowWrite
		if in.Write != nil {
			rw = *in.Write
		}
		return tp.Write(ctx, c, in.Table, rw)
	})
}
