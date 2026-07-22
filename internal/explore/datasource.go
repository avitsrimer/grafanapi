package explore

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-openapi/runtime"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/datasources"
	"github.com/grafana/grafana-openapi-client-go/models"
)

// ResolveDataSource resolves ref to a datasource: it is tried first as a UID,
// then (on a 404) as a name. When neither matches, the returned error lists
// the available datasources (name, uid, type) so a typo is easy to spot.
//
// Any non-404 error from the underlying client (network failure, 401, 500,
// ...) is returned unchanged so that session rotation and error rendering
// upstream behave exactly as they do for every other command.
func ResolveDataSource(client *goapi.GrafanaHTTPAPI, ref string) (*models.DataSource, error) {
	byUID, err := client.Datasources.GetDataSourceByUID(ref)
	if err == nil {
		return byUID.Payload, nil
	}

	var uidNotFound *datasources.GetDataSourceByUIDNotFound
	if !errors.As(err, &uidNotFound) {
		return nil, err
	}

	byName, err := client.Datasources.GetDataSourceByName(ref)
	if err == nil {
		return byName.Payload, nil
	}

	// Unlike GetDataSourceByUID, the generated GetDataSourceByName reader has
	// no typed *GetDataSourceByNameNotFound response - a 404 falls through
	// its switch's default case and comes back as a bare *runtime.APIError.
	// Verified by reading get_data_source_by_name_responses.go: the reader
	// only special-cases 200/401/403/500, so this is the only way to detect
	// a name-lookup miss.
	var apiErr *runtime.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
		return nil, err
	}

	return nil, notFoundError(client, ref)
}

// notFoundError builds the "datasource not found" error for ResolveDataSource,
// listing every available datasource as "name (uid, type)" sorted by name. If
// the listing call itself fails, that failure is wrapped and returned instead.
func notFoundError(client *goapi.GrafanaHTTPAPI, ref string) error {
	list, err := client.Datasources.GetDataSources()
	if err != nil {
		return fmt.Errorf("datasource %q not found: listing available datasources: %w", ref, err)
	}

	items := list.Payload
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	var msg strings.Builder
	fmt.Fprintf(&msg, "datasource %q not found; available datasources:", ref)
	for _, ds := range items {
		fmt.Fprintf(&msg, "\n  %s (%s, %s)", ds.Name, ds.UID, ds.Type)
	}

	return errors.New(msg.String())
}
