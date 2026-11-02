package appkit

import (
	"context"
	"net/url"
	"time"
)

// History imports an app runs in its own process (ADR-0081, ADR-0130 P11 §0 4). The app applies the old system's rows with its own
// commands in its own database and reports each chunk here (POST /app/v1/imports/{batch}/items, scope app.imports:write): the platform
// lists the batch with every other app's (the administrator's batches page), records the provenance of each record a row wrote
// (via "import": the 来源 panel, Drafts().Provenance) and audits the chunk. Only a page session of a tenant administrator reports:
// the caller is the person who imports.

// ImportReport is one chunk of a batch.
type ImportReport struct {
	Label string             `json:"label,omitempty"` // the batch's description ("" keeps the one it has)
	Items []ImportReportItem `json:"items"`           // 1 to 500 rows, keys unique
}

// ImportReportItem is what became of one row of the batch.
type ImportReportItem struct {
	Key        string    `json:"key"`                // the old system's key of the row: unique in its batch
	Kind       string    `json:"kind"`               // what the row is (the app's kinds)
	Status     string    `json:"status"`             // ImportOK or ImportFailed
	RecordID   string    `json:"recordId,omitempty"` // ok: the id of the main record the row wrote
	Ref        string    `json:"ref,omitempty"`      // ok: its k3://{app}/… reference; the record gets a provenance row
	BusinessAt time.Time `json:"businessAt"`         // the row's business time
	Digest     string    `json:"digest"`             // the app's hash of the row as sent: a row sent again must be the same row
	Error      string    `json:"error,omitempty"`    // failed: why, as the importer reads it
}

// The status of a reported row.
const (
	ImportOK     = "ok"
	ImportFailed = "failed"
)

// ImportBatch is a batch as the platform's batches page shows it.
type ImportBatch struct {
	App         string     `json:"app"`
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	CreatedBy   string     `json:"createdBy"`
	CreatedName string     `json:"createdName"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	RowsOK      int        `json:"rowsOk"`
	RowsFailed  int        `json:"rowsFailed"`
	FirstAt     *time.Time `json:"firstAt,omitempty"`
	LastAt      *time.Time `json:"lastAt,omitempty"`
}

// Imports reports history imports to the platform (Deps.Imports, Client.Imports).
type Imports interface {
	// Report records one chunk of batch as c (a page session of a tenant administrator). A refused chunk is ErrInvalid (422, nothing
	// kept), ErrConflict (409: a key already imported ok as another row) or ErrForbidden; the answer is the batch as it is now.
	Report(ctx context.Context, c Caller, batch string, rep ImportReport) (ImportBatch, error)
}

// Imports is the history import report over /app/v1/imports: Deps.Imports = cl.Imports().
func (cl *Client) Imports() Imports { return importsClient{cl} }

type importsClient struct{ cl *Client }

func (i importsClient) Report(ctx context.Context, c Caller, batch string, rep ImportReport) (ImportBatch, error) {
	var out ImportBatch
	err := i.cl.call(ctx, c, "POST", "/app/v1/imports/"+url.PathEscape(batch)+"/items", nil, rep, &out)
	return out, err
}
