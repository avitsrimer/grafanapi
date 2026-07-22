package grafana

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Masterminds/semver/v3"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafanapi/internal/config"
)

func ClientFromContext(ctx *config.Context) (*goapi.GrafanaHTTPAPI, error) {
	if ctx == nil {
		return nil, errors.New("no context provided")
	}
	if ctx.Grafana == nil {
		return nil, errors.New("grafana not configured")
	}

	grafanaURL, err := url.Parse(ctx.Grafana.Server)
	if err != nil {
		return nil, err
	}

	cfg := &goapi.TransportConfig{
		Host:     grafanaURL.Host,
		BasePath: strings.TrimLeft(grafanaURL.Path+"/api", "/"),
		Schemes:  []string{grafanaURL.Scheme},
	}

	if ctx.Grafana.TLS != nil {
		cfg.TLSConfig = ctx.Grafana.TLS.ToStdTLSConfig()
	}

	if ctx.Grafana.OrgID != 0 {
		cfg.OrgID = ctx.Grafana.OrgID
	}

	client := goapi.NewHTTPClientWithConfig(strfmt.Default, cfg)

	// The session cookie is injected at the transport level (rather than via the static
	// TransportConfig.HTTPHeaders map used previously) so that a 401 can be observed and the
	// cookie rotated - see internal/config.GrafanaConfig.WrapWithSession. NewHTTPClientWithConfig
	// always builds an *httptransport.Runtime (grafana_http_api_client.go's
	// newTransportWithConfig), so the type assertion below is expected to always succeed; the
	// guard exists to fail loudly instead of silently sending unauthenticated requests if that
	// ever changes upstream.
	rt, ok := client.Transport.(*httptransport.Runtime)
	if !ok {
		return nil, fmt.Errorf("grafana openapi client: unexpected transport type %T, expected *httptransport.Runtime", client.Transport)
	}
	rt.Transport = ctx.Grafana.WrapWithSession(rt.Transport)

	return client, nil
}

func GetVersion(ctx *config.Context) (*semver.Version, error) {
	gClient, err := ClientFromContext(ctx)
	if err != nil {
		return nil, err
	}

	healthResponse, err := gClient.Health.GetHealth()
	if err != nil {
		return nil, err
	}

	return semver.NewVersion(healthResponse.Payload.Version)
}
