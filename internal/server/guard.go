package server

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/grafana/grafana-app-sdk/logging"
)

const httpDefaultPort = 80

// guard is the outermost middleware of the server: it rejects requests that do not address this
// server (DNS rebinding) before any route, proxy or websocket upgrade runs.
type guard struct {
	hosts  map[string]struct{}
	logger logging.Logger
}

// middleware answers 421 Misdirected Request when the Host header does not name this server.
func (g *guard) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r.Host) {
			g.logger.Warn("rejected request with unexpected Host", "host", r.Host, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "grafanapi serve: unexpected Host header", http.StatusMisdirectedRequest)

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (g *guard) hostAllowed(hostport string) bool {
	_, ok := g.hosts[strings.ToLower(hostport)]

	return ok
}

func newGuard(hosts []string, logger logging.Logger) *guard {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		set[strings.ToLower(h)] = struct{}{}
	}

	return &guard{hosts: set, logger: logger}
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
