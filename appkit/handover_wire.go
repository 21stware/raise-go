package appkit

import "encoding/json"

// A page's request handed over to another app's process (the desk and the till another app serves, ADR-0130 §2.4): the asking app
// sends it to the platform (/app/v1/peers/{desk,till}/http), the platform to the providing app (/k3/providers/{desk,till}/http), as
// the person of the page. The platform's side is its internal/providers (HTTPCall, HTTPAnswer); these are the same JSON.

// HTTPCall is the handed-over request: the page's method, its path under /api/, its query and its body.
type HTTPCall struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Query  string          `json:"query,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// HTTPAnswer is the provider's answer to a handed-over request: its bytes exactly (base64 in JSON), whatever their type.
type HTTPAnswer struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	Body        []byte `json:"body,omitempty"`
}
