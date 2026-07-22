package config

// SessionCookieName is the name of the Grafana session cookie attached to every outbound
// request on both transport paths (the k8s dynamic REST client and the openapi client).
const SessionCookieName = "grafana_session"

// CookieHeaderValue formats cookie as a "Cookie" HTTP header value carrying the Grafana session
// cookie, e.g. CookieHeaderValue("abc123") returns "grafana_session=abc123".
//
// This helper lives in package config (a leaf package with no dependency on internal/session) so
// that internal/session, internal/grafana, and internal/server/grafana can all import it without
// creating an import cycle back into internal/session.
func CookieHeaderValue(cookie string) string {
	return SessionCookieName + "=" + cookie
}
