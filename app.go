package raise

import (
	"context"
	"net/url"
	"strings"
)

// appRoot is the app API's root (/app/v1) next to the /open/v1 root the client was given: https://api.example.com/open/v1 becomes
// https://api.example.com/app/v1.
func appRoot(openBase string) string {
	return strings.TrimSuffix(strings.TrimRight(openBase, "/"), "/open/v1") + "/app/v1"
}

// doApp is do against the app API's root. The credential, the tenant tokens, the retries and the Idempotency-Key are the open API's.
func (c *Client) doApp(ctx context.Context, method, path string, query url.Values, body, out any, ro requestOptions) error {
	ro.root = appRoot(c.baseURL)
	return c.do(ctx, method, path, query, body, out, ro)
}
