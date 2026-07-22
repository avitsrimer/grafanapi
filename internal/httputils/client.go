package httputils

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/grafana/grafanapi/internal/config"
)

// NewTransport builds a *http.Transport for gCtx, TLS-configured from gCtx.Grafana.TLS when set.
// It returns an error rather than silently falling back to a plain system-trust TLS config when
// gCtx.Grafana.TLS contains malformed CAData/CertData/KeyData (see TLS.ToStdTLSConfig).
func NewTransport(gCtx *config.Context) (*http.Transport, error) {
	//nolint:gosec
	tlsConfig := &tls.Config{InsecureSkipVerify: false}

	if gCtx.Grafana != nil && gCtx.Grafana.TLS != nil {
		cfg, err := gCtx.Grafana.TLS.ToStdTLSConfig()
		if err != nil {
			return nil, err
		}

		tlsConfig = cfg
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}, nil
}
