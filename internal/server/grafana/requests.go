package grafana

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
)

// proxyClientTimeout bounds the dashboard-proxy request to the configured Grafana server.
const proxyClientTimeout = 10 * time.Second

func AuthenticateAndProxyHandler(cfg *config.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "text/html")

		if cfg.Grafana.Server == "" {
			httputils.Error(r, w, "Error: No Grafana URL configured", errors.New("no Grafana URL configured"), http.StatusBadRequest)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.Grafana.Server+r.URL.Path, nil)
		if err != nil {
			httputils.Error(r, w, http.StatusText(http.StatusInternalServerError), err, http.StatusInternalServerError)
			return
		}

		req.Header.Set("User-Agent", httputils.UserAgent)

		transport, err := httputils.NewTransport(cfg)
		if err != nil {
			httputils.Error(r, w, http.StatusText(http.StatusInternalServerError), err, http.StatusInternalServerError)
			return
		}

		// Built directly from httputils.NewTransport, wrapped only with WrapWithSession (cookie
		// injection and rotate-on-401, see internal/config/session_source.go) and deliberately
		// not wrapped in any debug-logging round-tripper: dumping the full request (headers
		// included, unredacted) would put the session cookie WrapWithSession sets into logs
		// reachable via -vvv. There is no separate AuthenticateRequest call setting the cookie
		// here: WrapWithSession's wrapped transport runs on every request and unconditionally
		// overwrites the Cookie header, so it always wins.
		client := &http.Client{
			Timeout:   proxyClientTimeout,
			Transport: cfg.Grafana.WrapWithSession(transport),
		}

		client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			// Being redirected to the login page means authentication is misconfigured.
			// We interrupt the redirect and let the rest of AuthenticateAndProxyHandler
			// handle that case.
			if strings.HasSuffix(req.URL.Path, "/login") {
				return http.ErrUseLastResponse
			}

			return nil
		}

		resp, err := client.Do(req)
		if err != nil {
			httputils.Error(r, w, http.StatusText(http.StatusInternalServerError), err, http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusFound {
			w.WriteHeader(http.StatusUnauthorized)
			httputils.Write(r, w, []byte(`<html>
<body style="margin-top: 3rem; color: hsla(225deg, 15%, 90%, 0.82);">
	<h1>Authentication error</h1>
	<p>It appears that the Grafana credentials in your configuration are missing or incorrect.</p>
</body>
</html>`))
			return
		}

		body, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		httputils.Write(r, w, body)
	}
}
