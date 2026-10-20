package config

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// maxRedirects is the redirect cap net/http applies by default. Setting CheckRedirect replaces
// that policy, so SameOriginRedirects re-implements it.
const maxRedirects = 10

// invalidOrigin stands in for an origin that cannot be computed in an UnsafeRedirectError.
const invalidOrigin = "<invalid>"

var (
	// ErrInvalidServerURL is wrapped by every *ServerURLError.
	ErrInvalidServerURL = errors.New("invalid Grafana server URL")
	// ErrUnsafeRedirect is wrapped by every *UnsafeRedirectError.
	ErrUnsafeRedirect = errors.New("refusing to leave the configured Grafana server")
)

// ServerOrigin normalises server to its origin, "<scheme>://<host>:<port>".
//
// It normalises only: it applies no policy, so "http://grafana.example.com" succeeds and userinfo,
// path, query and fragment are ignored. The input must parse with url.Parse (no whitespace
// trimming) as an http or https URL (scheme case-insensitive) with a non-empty, ASCII-only host
// (internationalised domain names must be given in punycode, "xn--...") and an optional port that
// is a decimal number in 1..65535.
//
// The output is exactly:
//   - the scheme, lower-cased;
//   - the host, lower-cased with at most one trailing dot removed; an IP literal is rendered in
//     netip canonical form (so "[0:0:0:0:0:0:0:1]" becomes "[::1]");
//   - the port, ALWAYS explicit: the given port as a plain decimal ("0443" becomes "443"), else
//     the scheme default (https 443, http 80);
//   - host and port joined with net.JoinHostPort (IPv6 bracketed), no trailing slash, no path.
//
// This format is persisted by the Keychain origin binding and must never change.
//
// Every failure is a *ServerURLError (errors.Is(err, ErrInvalidServerURL)) whose Reason never
// echoes the input; the returned string is empty on error.
func ServerOrigin(server string) (string, error) {
	u, err := parseServer(server)
	if err != nil {
		return "", err
	}

	return originOf(u)
}

// ValidateServerURL reports whether server may carry the session cookie: ServerOrigin succeeds,
// the scheme is https (or http on a loopback host: localhost, 127.0.0.0/8 or ::1), and the URL
// holds no userinfo. A path or trailing slash is allowed. Every failure is a *ServerURLError with
// a fixed Reason that never echoes the input.
func ValidateServerURL(server string) error {
	u, err := parseServer(server)
	if err != nil {
		return err
	}

	if _, err := originOf(u); err != nil {
		return err
	}

	if u.User != nil {
		return &ServerURLError{Reason: "server must not contain credentials (user:password@); authenticate with grafanapi login"}
	}

	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}

	// originOf succeeded, so the host is non-empty and ASCII.
	host, _ := normalizeHost(u.Hostname())
	if !isLoopbackHost(host) {
		return &ServerURLError{Reason: "server must use https:// (plain http:// is only allowed for localhost, 127.0.0.1 and ::1)"}
	}

	return nil
}

// SetSessionCookie sets "Cookie: grafana_session=<cookie>" on req if and only if
// ValidateServerURL(server) passes and req.URL has the same origin as server. Otherwise it returns
// a *ServerURLError or *UnsafeRedirectError and leaves req untouched. It is the single place
// outside the rotating round-tripper that writes the cookie header.
func SetSessionCookie(req *http.Request, server, cookie string) error {
	if err := checkSessionTarget(req.URL, server); err != nil {
		return err
	}

	if req.Header == nil {
		req.Header = make(http.Header)
	}

	req.Header.Set("Cookie", CookieHeaderValue(cookie))

	return nil
}

// SameOriginRedirects returns an http.Client CheckRedirect func that refuses any hop leaving
// server's origin with an *UnsafeRedirectError (origins only, never a path or query), returns a
// *ServerURLError if server itself is not a usable URL, and stops after 10 redirects like
// net/http's default policy. Relative Locations arrive here already resolved to absolute URLs.
func SameOriginRedirects(server string) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}

		from, err := ServerOrigin(server)
		if err != nil {
			return err
		}

		to, err := originOf(req.URL)
		if err != nil {
			return &UnsafeRedirectError{From: from, To: invalidOrigin}
		}

		if to != from {
			return &UnsafeRedirectError{From: from, To: to}
		}

		return nil
	}
}

// ServerURLError is a server URL that cannot be used. Reason is a fixed text and never echoes
// the offending URL, which may contain credentials.
type ServerURLError struct {
	Reason string
}

// UnsafeRedirectError is a request or redirect that leaves the configured server origin. From and
// To are ServerOrigin-format origins (To may be "<invalid>"), never a path or query.
type UnsafeRedirectError struct {
	From, To string
}

// Error implements error.
func (e *ServerURLError) Error() string {
	return "invalid Grafana server URL: " + e.Reason
}

// Unwrap returns ErrInvalidServerURL.
func (e *ServerURLError) Unwrap() error {
	return ErrInvalidServerURL
}

// Error implements error.
func (e *UnsafeRedirectError) Error() string {
	return "refusing to leave the configured Grafana server " + e.From + " for " + e.To
}

// Unwrap returns ErrUnsafeRedirect.
func (e *UnsafeRedirectError) Unwrap() error {
	return ErrUnsafeRedirect
}

// checkSessionTarget reports whether the cookie may be sent to target: server must pass
// ValidateServerURL and target must share its origin.
func checkSessionTarget(target *url.URL, server string) error {
	if err := ValidateServerURL(server); err != nil {
		return err
	}

	from, err := ServerOrigin(server)
	if err != nil {
		return err
	}

	if target == nil {
		return &UnsafeRedirectError{From: from, To: invalidOrigin}
	}

	to, err := originOf(target)
	if err != nil {
		return &UnsafeRedirectError{From: from, To: invalidOrigin}
	}

	if to != from {
		return &UnsafeRedirectError{From: from, To: to}
	}

	return nil
}

// isLoopbackHost reports whether the normalised host is "localhost" or a loopback IP.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	addr, err := netip.ParseAddr(host)

	return err == nil && addr.IsLoopback()
}

// normalizeHost lower-cases raw, removes at most one trailing dot and canonicalises an IP
// literal. It reports false for an empty or non-ASCII host.
func normalizeHost(raw string) (string, bool) {
	for i := range len(raw) {
		if raw[i] >= 0x80 {
			return "", false
		}
	}

	host := strings.TrimSuffix(strings.ToLower(raw), ".")
	if host == "" {
		return "", false
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.String()
	}

	return host, true
}

// originOf is the normalisation behind ServerOrigin.
func originOf(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", &ServerURLError{Reason: "server must be an absolute URL such as https://grafana.example.com"}
	}

	if u.Hostname() == "" {
		return "", &ServerURLError{Reason: "server must be an absolute URL such as https://grafana.example.com"}
	}

	host, ok := normalizeHost(u.Hostname())
	if !ok {
		if strings.TrimSuffix(u.Hostname(), ".") == "" {
			return "", &ServerURLError{Reason: "server must be an absolute URL such as https://grafana.example.com"}
		}

		return "", &ServerURLError{Reason: "server host must be ASCII; give an internationalised domain name in its punycode (xn--) form"}
	}

	port := "443"
	if scheme == "http" {
		port = "80"
	}

	if given := u.Port(); given != "" {
		n, err := strconv.Atoi(given)
		if err != nil || n < 1 || n > 65535 {
			return "", &ServerURLError{Reason: "server port must be a number between 1 and 65535"}
		}

		port = strconv.Itoa(n)
	}

	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// parseServer parses server with url.Parse, mapping a failure to a *ServerURLError that does not
// echo the input.
func parseServer(server string) (*url.URL, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, &ServerURLError{Reason: "server must be an absolute URL such as https://grafana.example.com"}
	}

	return u, nil
}
