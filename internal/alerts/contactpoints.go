package alerts

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
)

// ListContactPoints returns every notification contact point visible to gClient's session,
// sorted by name.
//
// The generated model is returned as-is rather than re-projected onto a local struct: unlike
// alert rules there is nothing to join, and Settings arrives as Grafana chose to serialize it.
//
// Note what that does and does not hide: Grafana replaces the *schema-declared secure fields* of
// each contact-point type (a Slack token, a Pushover key, ...) with "[REDACTED]" server-side, but
// a credential a user happened to embed somewhere else - inside a webhook URL, or in a custom
// header value - is not a secure field and is returned verbatim. Callers rendering Settings must
// not describe it as secret-free.
//
// Errors are wrapped with the operation name; a 401 stays reachable via errors.As as a
// *runtime.APIError, so the stale-session exit-2 rendering still fires.
//
// The returned slice is never nil and never contains a nil element, so `-o json` emits `[]`
// rather than `null` for an empty list and never emits a bare `null` in place of a contact point.
// The payload is a slice of pointers, so a `null` element in the response would otherwise both
// panic the sort comparator and render as `null` in the json/yaml output.
func ListContactPoints(ctx context.Context, gClient *goapi.GrafanaHTTPAPI) (models.ContactPoints, error) {
	if gClient == nil {
		return nil, errors.New("alerts: no grafana client configured")
	}

	resp, err := gClient.Provisioning.GetContactpoints(provisioning.NewGetContactpointsParams().WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("alerts: list contact points: %w", err)
	}

	points := make(models.ContactPoints, 0, len(resp.Payload))

	for _, point := range resp.Payload {
		if point == nil {
			continue
		}

		points = append(points, point)
	}

	slices.SortStableFunc(points, func(a, b *models.EmbeddedContactPoint) int {
		return cmp.Or(
			cmp.Compare(a.Name, b.Name),
			// The UID breaks a tie between two contact points sharing a name, which Grafana
			// allows: a name is a UI grouping key rather than an identifier.
			cmp.Compare(a.UID, b.UID),
		)
	})

	return points, nil
}
