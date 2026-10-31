package appkit

import "context"

type headersKey struct{}

// withHeader adds a header to the platform calls made with ctx (the options client's K3-Dry-Run).
func withHeader(ctx context.Context, k, v string) context.Context {
	h := map[string]string{}
	for k0, v0 := range headersOf(ctx) {
		h[k0] = v0
	}
	h[k] = v
	return context.WithValue(ctx, headersKey{}, h)
}

func headersOf(ctx context.Context) map[string]string {
	h, _ := ctx.Value(headersKey{}).(map[string]string)
	return h
}
