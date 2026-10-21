package server

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/grafana/grafana-app-sdk/logging"
)

const (
	httpDefaultPort   = 80
	tokenQueryParam   = "token"
	tokenCookiePrefix = "grafanapi_serve_"
)

// guard is the outermost middleware of the server: it rejects requests that do not address this
// server (DNS rebinding) or do not carry the per-run access token before any route, proxy or
// websocket upgrade runs.
type guard struct {
	hosts      map[string]struct{}
	token      string
	cookieName string
	logger     logging.Logger
}

// middleware enforces, in order: the Host allowlist (421), the access token (401), the session
// cookie and finally the Origin / Sec-Fetch-Site policy (403). A request carrying a token query parameter never reaches
// next, whatever the cookie state, so the token is never logged by the request logger nor
// forwarded to Grafana in a proxied query string.
func (g *guard) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r.Host) {
			g.logger.Warn("rejected request with unexpected Host", "host", r.Host, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "grafanapi serve: unexpected Host header", http.StatusMisdirectedRequest)

			return
		}

		if hasTokenParam(r.URL.RawQuery) {
			g.redeemQueryToken(w, r)

			return
		}

		if !g.authenticated(r) {
			g.reject(w, r)

			return
		}

		if !g.originAllowed(r) {
			g.logger.Warn("rejected cross-origin request",
				"origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"),
				"method", r.Method, "path", r.URL.Path)
			http.Error(w, "grafanapi serve: cross-origin request rejected", http.StatusForbidden)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// redeemQueryToken handles a request that carries the token in its query: a valid token on GET/HEAD
// is exchanged for a session cookie and a redirect to a fixed, query-less location; anything else
// is refused. It never calls the next handler.
func (g *guard) redeemQueryToken(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		!g.tokenValid(values.Get(tokenQueryParam)) {
		g.reject(w, r)

		return
	}

	// No Secure attribute: the server speaks plain http on loopback, where a Secure cookie would be dropped.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec,nolintlint // G124 (newer gosec only): no Secure attribute, see above
		Name:     g.cookieName,
		Value:    g.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Fixed location: never derived from r.URL, so it cannot become an off-site redirect.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// originAllowed reports whether the request's Origin and Sec-Fetch-Site, when present, say it comes from
// this server's own pages. A request with neither header (non-browser clients) passes this step; "null"
// and unparseable Origins do not. Sec-Fetch-Site covers the browser requests that carry no Origin, such
// as a same-site page on another local port loading a subresource with the cookie attached: browsers
// always send it, and anything other than "same-origin" or "none" (user-initiated) is refused.
func (g *guard) originAllowed(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	o, err := url.Parse(origin)
	if err != nil || o.Scheme != "http" {
		return false
	}

	return g.hostAllowed(o.Host)
}

func (g *guard) authenticated(r *http.Request) bool {
	c, err := r.Cookie(g.cookieName)

	return err == nil && g.tokenValid(c.Value)
}

func (g *guard) tokenValid(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(g.token)) == 1
}

// reject logs method and path only: never the query string, the token or the cookie.
func (g *guard) reject(w http.ResponseWriter, r *http.Request) {
	g.logger.Warn("rejected request without a valid access token", "method", r.Method, "path", r.URL.Path)
	http.Error(w, "grafanapi serve: missing or invalid access token; open the URL printed when the server started", http.StatusUnauthorized)
}

func (g *guard) hostAllowed(hostport string) bool {
	_, ok := g.hosts[strings.ToLower(hostport)]

	return ok
}

func newGuard(hosts []string, token string, port int, logger logging.Logger) *guard {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		set[strings.ToLower(h)] = struct{}{}
	}

	return &guard{hosts: set, token: token, cookieName: tokenCookiePrefix + strconv.Itoa(port), logger: logger}
}

// hasTokenParam reports whether the raw query has a token key. It splits on both '&' and ';' and
// tolerates undecodable keys, so a malformed query cannot smuggle the parameter past the guard.
func hasTokenParam(rawQuery string) bool {
	for _, pair := range strings.FieldsFunc(rawQuery, func(c rune) bool { return c == '&' || c == ';' }) {
		key, _, _ := strings.Cut(pair, "=")

		if unescaped, err := url.QueryUnescape(key); err == nil {
			key = unescaped
		}

		if key == tokenQueryParam {
			return true
		}
	}

	return false
}

// allowedHosts returns the lower-cased Host header values (host:port, plus the bare host when the
// port is 80) that name a server bound to listenHost:port.
//
// Loopback names are always allowed, as is an explicit --address. For a wildcard bind every
// non-link-local interface IP is added: IP literals cannot be DNS-rebound. The interface set is a
// snapshot taken when this is called.
func allowedHosts(listenHost string, port int, ifaceAddrs func() ([]net.Addr, error)) ([]string, error) {
	names := []string{"localhost", "127.0.0.1", "::1"}

	switch {
	case isWildcard(listenHost):
		addrs, err := ifaceAddrs()
		if err != nil {
			return nil, fmt.Errorf("could not list interface addresses: %w", err)
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}

			names = append(names, ipNet.IP.String())
		}
	default:
		names = append(names, listenHost)
	}

	seen := make(map[string]struct{}, len(names))
	hosts := make([]string, 0, len(names))

	add := func(h string) {
		h = strings.ToLower(h)
		if _, dup := seen[h]; dup {
			return
		}

		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}

	for _, name := range names {
		add(net.JoinHostPort(name, strconv.Itoa(port)))

		if port == httpDefaultPort {
			if strings.Contains(name, ":") {
				add("[" + name + "]")
			} else {
				add(name)
			}
		}
	}

	return hosts, nil
}

func isWildcard(host string) bool {
	if host == "" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsUnspecified()
}
