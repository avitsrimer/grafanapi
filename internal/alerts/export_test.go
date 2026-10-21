package alerts

import (
	"context"

	goapi "github.com/grafana/grafana-openapi-client-go/client"
)

// PushWithScopeForTest is push with the scoping step supplied by the test, so a doctored scope output
// can be fed to the push and the guard shown to stand between it and a write.
func PushWithScopeForTest(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	local Bundle,
	opts PushOptions,
	scope func(local, remote Bundle, uids []string) (Bundle, Bundle, error),
) (Result, error) {
	return push(ctx, gClient, local, opts, scope)
}

// ScopeBundlesForTest exposes scopeBundles.
func ScopeBundlesForTest(local, remote Bundle, uids []string) (Bundle, Bundle, error) {
	return scopeBundles(local, remote, uids)
}
