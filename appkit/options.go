package appkit

import (
	"context"
	"net/url"
)

// HeaderDryRun asks the platform for a preview of a write.
const HeaderDryRun = "K3-Dry-Run"

// OptionList is one of the app's option lists (a form's choices the tenant may change: where a candidate came from, a refund's reason).
// The field stays text; the list only decides what the form offers.
type OptionList struct {
	Key      string   // the list key in the routes ("candidate-source")
	Title    string   // what the list is called in the app's words
	MaxLen   int      // characters per option
	Defaults []string // what a tenant starts with
}

// OptionsChange is a change of a list: Items replaces it as a whole (nil: not given), Add appends one option.
type OptionsChange struct {
	Items *[]string
	Add   string
}

// OptionStore is the tenant's option lists as an app reaches them (Deps.OptionsAPI, Client.Options). app is the calling app's own
// (the platform uses the token's). Save with dry true only previews: nothing is kept.
type OptionStore interface {
	Load(ctx context.Context, c Caller, app string, l OptionList) ([]string, error)
	Save(ctx context.Context, c Caller, app string, l OptionList, ch OptionsChange, dry bool) (old, now []string, err error)
}

// Options is the tenant's option lists over /app/v1/options (ADR-0130 §3.2 H7): Deps.OptionsAPI = cl.Options().
func (cl *Client) Options() OptionStore { return optionsClient{cl} }

type optionsClient struct{ cl *Client }

func (o optionsClient) Load(ctx context.Context, c Caller, _ string, l OptionList) ([]string, error) {
	q := url.Values{}
	for _, d := range l.Defaults {
		q.Add("default", d)
	}
	var out struct {
		Items []string `json:"items"`
	}
	err := o.cl.call(ctx, c, "GET", "/app/v1/options/"+pathEscape(l.Key), q, nil, &out)
	return out.Items, err
}

func (o optionsClient) Save(ctx context.Context, c Caller, _ string, l OptionList, ch OptionsChange, dry bool) (old, now []string, err error) {
	if dry {
		ctx = withHeader(ctx, HeaderDryRun, "1")
	}
	body := struct {
		Title    string    `json:"title,omitempty"`
		MaxLen   int       `json:"maxLen,omitempty"`
		Defaults []string  `json:"defaults,omitempty"`
		Items    *[]string `json:"items,omitempty"`
		Add      string    `json:"add,omitempty"`
	}{l.Title, l.MaxLen, l.Defaults, ch.Items, ch.Add}
	var out struct {
		Old []string `json:"old"`
		Now []string `json:"now"`
	}
	err = o.cl.call(ctx, c, "PUT", "/app/v1/options/"+pathEscape(l.Key), nil, body, &out)
	return out.Old, out.Now, err
}
