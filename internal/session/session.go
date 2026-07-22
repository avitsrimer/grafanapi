// Package session verifies Grafana session cookies against a live Grafana instance, discovers the
// organization ID behind a cookie for on-prem Grafana, and provides the error types used to
// detect and report a stale (no longer accepted) session.
//
// session imports internal/config (VerifyCookie and CurrentOrgID take a *config.Context and reuse
// config.CookieHeaderValue to format the Cookie header); config must never import session back,
// to avoid an import cycle.
package session

import (
	"context"
	"encoding/json"
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

	transport, err := httputils.NewTransport(gCtx)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}

	client := &http.Client{Timeout: verifyTimeout, Transport: transport}

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

// CurrentOrgID discovers the organization ID of gCtx's session cookie against its configured
// Grafana server via GET /api/org. It is the on-prem counterpart of config.DiscoverStackID (which
// covers Grafana Cloud): on-prem Grafana has no /bootdata namespace to discover a stack ID from,
// so callers fall back to asking the org endpoint directly once the cookie is known to be valid.
//
// Like VerifyCookie, the request is issued over the TLS-aware httputils.NewTransport directly,
// with no debug-logging round-tripper wrapped around it, so the session cookie is never written
// to logs.
func CurrentOrgID(ctx context.Context, gCtx *config.Context) (int64, error) {
	if gCtx == nil || gCtx.Grafana == nil {
		return 0, errors.New("session: no grafana context configured")
	}

	orgURL, err := buildOrgURL(gCtx.Grafana.Server)
	if err != nil {
		return 0, fmt.Errorf("session: invalid server address: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orgURL.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("session: building request: %w", err)
	}
	req.Header.Set("Cookie", config.CookieHeaderValue(gCtx.Grafana.SessionCookie))

	transport, err := httputils.NewTransport(gCtx)
	if err != nil {
		return 0, fmt.Errorf("session: %w", err)
	}

	client := &http.Client{Timeout: verifyTimeout, Transport: transport}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("session: discovering organization id: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("session: unexpected status %d discovering organization id", resp.StatusCode)
	}

	var payload struct {
		ID int64 `json:"id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("session: decoding organization id response: %w", err)
	}

	if payload.ID == 0 {
		return 0, errors.New("session: discovered organization id is 0")
	}

	return payload.ID, nil
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

func buildOrgURL(server string) (*url.URL, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	trimmedPath := strings.TrimSuffix(parsed.Path, "/")
	parsed.Path = trimmedPath + "/api/org"
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed, nil
}
