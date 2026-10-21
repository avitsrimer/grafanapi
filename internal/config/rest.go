package config

import (
	"context"
	"net/http"

	authlib "github.com/grafana/authlib/types"
	"k8s.io/client-go/rest"
)

// NamespacedRESTConfig is a REST config with a namespace.
// TODO: move to app SDK?
type NamespacedRESTConfig struct {
	rest.Config

	Namespace string
}

// NewNamespacedRESTConfig creates a new namespaced REST config.
func NewNamespacedRESTConfig(ctx context.Context, cfg Context) NamespacedRESTConfig {
	rcfg := rest.Config{
		// TODO add user agent
		// UserAgent: cfg.UserAgent.ValueString(),
		Host:            cfg.Grafana.Server,
		APIPath:         "/apis",
		TLSClientConfig: rest.TLSClientConfig{},
		// TODO: make configurable
		QPS:   50,
		Burst: 100,
	}

	if cfg.Grafana.TLS != nil {
		// Kubernetes really is wonderful, huh.
		// tl;dr it has its own TLSClientConfig,
		// and it's not compatible with the one from the "crypto/tls" package.
		rcfg.TLSClientConfig = rest.TLSClientConfig{
			Insecure:   cfg.Grafana.TLS.Insecure,
			ServerName: cfg.Grafana.TLS.ServerName,
			CertData:   cfg.Grafana.TLS.CertData,
			KeyData:    cfg.Grafana.TLS.KeyData,
			CAData:     cfg.Grafana.TLS.CAData,
			NextProtos: cfg.Grafana.TLS.NextProtos,
		}
	}

	// Always delegate to WrapWithSession rather than duplicating its Session/SessionCookie
	// selection here: when neither is set, WrapWithSession's default case returns rt unchanged, so
	// installing the closure unconditionally is behaviorally identical to skipping it, without
	// rest.go needing to know which GrafanaConfig fields WrapWithSession switches on.
	grafana := cfg.Grafana
	rcfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return grafana.WrapWithSession(rt)
	}

	// Namespace
	var namespace string

	discoveredStackID, err := DiscoverStackID(ctx, *cfg.Grafana)

	if err == nil {
		// even if cfg.Grafana.OrgID was set - we ignore it, discoveredStackID takes precedent
		namespace = authlib.CloudNamespaceFormatter(discoveredStackID)
	} else {
		if cfg.Grafana.OrgID != 0 {
			namespace = authlib.OrgNamespaceFormatter(cfg.Grafana.OrgID)
		} else {
			namespace = authlib.CloudNamespaceFormatter(cfg.Grafana.StackID)
		}
	}

	return NamespacedRESTConfig{
		Config:    rcfg,
		Namespace: namespace,
	}
}

// sessionCookieRoundTripper injects the Grafana session cookie into every outbound request
// that targets the configured server's origin before delegating to the wrapped RoundTripper.
// A request to any other origin (a redirect hop, say) is refused without being sent.
type sessionCookieRoundTripper struct {
	server string
	cookie string
	next   http.RoundTripper
}

func (rt *sessionCookieRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if err := SetSessionCookie(clone, rt.server, rt.cookie); err != nil {
		closeRequestBody(req)

		return nil, err
	}

	return rt.next.RoundTrip(clone)
}

// closeRequestBody closes req.Body when there is one: the http.RoundTripper contract requires
// RoundTrip to close the body even when it returns an error without sending the request.
func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
