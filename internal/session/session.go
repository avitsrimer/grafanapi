// Package session verifies Grafana session cookies against a live Grafana instance and provides
// the error types used to detect and report a stale (no longer accepted) session.
//
// session imports internal/config (VerifyCookie takes a *config.Context and reuses
// config.CookieHeaderValue to format the Cookie header); config must never import session back,
// to avoid an import cycle.
package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
)

// verifyTimeout bounds the GET /api/user request VerifyCookie issues. Without it, an
// unreachable/firewalled Grafana host would hang the calling command (login, login update, and
// any centralized re-verification) indefinitely instead of failing with a clear network error;
// every other HTTP client in this codebase (the bootdata client in internal/config/stack_id.go)
// sets one too.
const verifyTimeout = 10 * time.Second

// VerifyCookie checks whether gCtx's session cookie is still accepted by its configured Grafana
// server via GET /api/user. It returns nil on 200 OK, ErrUnauthorized on 401, and a wrapped error
// for any other failure (network error, unexpected status, timeout, ...).
//
// The verification request is issued over the TLS-aware httputils.NewTransport directly, with no
// debug-logging round-tripper wrapped around it, so the session cookie is never written to logs.
func VerifyCookie(ctx context.Context, gCtx *config.Context) error {
	if gCtx == nil || gCtx.Grafana == nil {
		return errors.New("session: no grafana context configured")
	}

	verifyURL, err := buildUserURL(gCtx.Grafana.Server)
	if err != nil {
		return fmt.Errorf("session: invalid server address: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL.String(), nil)
	if err != nil {
		return fmt.Errorf("session: building request: %w", err)
	}
	req.Header.Set("Cookie", config.CookieHeaderValue(gCtx.Grafana.SessionCookie))

	client := &http.Client{Timeout: verifyTimeout, Transport: httputils.NewTransport(gCtx)}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("session: verifying cookie: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		return fmt.Errorf("session: unexpected status %d verifying cookie", resp.StatusCode)
	}
}

func buildUserURL(server string) (*url.URL, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	trimmedPath := strings.TrimSuffix(parsed.Path, "/")
	parsed.Path = trimmedPath + "/api/user"
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed, nil
}
