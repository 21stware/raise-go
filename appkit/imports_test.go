package appkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestImportsReport(t *testing.T) {
	cl, s := platform(t, status(200, `{"app":"membership","id":"b 1","label":"old","createdBy":"u1","createdName":"An","createdAt":"2026-11-02T01:00:00Z",`+
		`"updatedAt":"2026-11-02T01:00:00Z","rowsOk":1,"rowsFailed":1,"firstAt":"2019-04-02T08:00:00Z","lastAt":"2019-04-02T08:00:00Z"}`))
	at := time.Date(2019, 4, 2, 8, 0, 0, 0, time.UTC)
	b, err := cl.Imports().Report(context.Background(), tc, "b 1", ImportReport{Label: "old", Items: []ImportReportItem{
		{Key: "m-1", Kind: "member", Status: ImportOK, RecordID: "mem_1", Ref: "k3://membership/member/mem_1", BusinessAt: at, Digest: "d1"},
		{Key: "m-2", Kind: "member", Status: ImportFailed, BusinessAt: at, Digest: "d2", Error: "bad phone"},
	}})
	if err != nil || b.RowsOK != 1 || b.RowsFailed != 1 || b.FirstAt == nil || !b.FirstAt.Equal(at) {
		t.Fatalf("%+v %v", b, err)
	}
	if s.method != "POST" || s.path != "/app/v1/imports/b%201/items" || s.tenant != "t1" {
		t.Fatalf("%+v", *s)
	}
	want := `{"label":"old","items":[{"key":"m-1","kind":"member","status":"ok","recordId":"mem_1","ref":"k3://membership/member/mem_1","businessAt":"2019-04-02T08:00:00Z","digest":"d1"},` +
		`{"key":"m-2","kind":"member","status":"failed","businessAt":"2019-04-02T08:00:00Z","digest":"d2","error":"bad phone"}]}`
	if s.body != want {
		t.Fatalf("body %s", s.body)
	}
	for code, sentinel := range map[int]error{409: ErrConflict, 422: ErrInvalid, 403: ErrForbidden} {
		cl, _ := platform(t, status(code, `{"error":"no"}`))
		if _, err := cl.Imports().Report(context.Background(), tc, "b", ImportReport{}); !errors.Is(err, sentinel) {
			t.Fatalf("%d: %v", code, err)
		}
	}
}
