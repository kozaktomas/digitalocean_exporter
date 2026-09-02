package doclient

import "context"

// noCollector labels a request made outside any collector's refresh: a probe
// at startup, or a test. It is a value rather than an empty label so that the
// series still sums with the rest.
const noCollector = "none"

// collectorKey is the context key the refreshing collector's name travels
// under. An unexported struct type cannot collide with a key another package
// puts on the same context.
type collectorKey struct{}

// WithCollector returns a copy of ctx carrying the name of the collector whose
// refresh is about to run, so that the requests the refresh makes can be
// attributed to it.
//
// The context is the only way the name can reach the transport: an API request
// carries nothing that identifies its caller, and the path does not either —
// `limits` and `droplets` both read /v2/droplets, and both monitoring
// collectors read /v2/monitoring. An empty name is ignored, which leaves the
// request labelled as noCollector.
func WithCollector(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, collectorKey{}, name)
}

// collectorName returns the collector recorded in ctx by WithCollector, or
// noCollector when the request was made outside a refresh.
func collectorName(ctx context.Context) string {
	name, ok := ctx.Value(collectorKey{}).(string)
	if !ok || name == "" {
		return noCollector
	}
	return name
}
