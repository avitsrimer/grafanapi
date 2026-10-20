package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-app-sdk/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeIfaces(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		addrs := make([]net.Addr, 0, len(cidrs))

		for _, c := range cidrs {
			ip, ipNet, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}

			addrs = append(addrs, &net.IPNet{IP: ip, Mask: ipNet.Mask})
		}

		return addrs, nil
	}
}

func TestAllowedHosts(t *testing.T) {
	loopback := []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"}

	tests := []struct {
		name   string
		listen string
		port   int
		ifaces func() ([]net.Addr, error)
		want   []string
	}{
		{name: "loopback bind", listen: "127.0.0.1", port: 8080, want: loopback},
		{name: "localhost bind", listen: "localhost", port: 8080, want: loopback},
		{name: "explicit address", listen: "192.168.1.10", port: 8080, want: append(append([]string{}, loopback...), "192.168.1.10:8080")},
		{
			name: "wildcard skips link-local", listen: "0.0.0.0", port: 8080,
			ifaces: fakeIfaces("10.0.0.5/24", "fe80::1/64", "2001:db8::5/64"),
			want:   append(append([]string{}, loopback...), "10.0.0.5:8080", "[2001:db8::5]:8080"),
		},
		{
			name: "empty listen is wildcard", listen: "", port: 8080,
			ifaces: fakeIfaces("10.0.0.5/24"),
			want:   append(append([]string{}, loopback...), "10.0.0.5:8080"),
		},
		{
			name: "port 80 adds bare hosts", listen: "127.0.0.1", port: 80,
			want: []string{"localhost:80", "localhost", "127.0.0.1:80", "127.0.0.1", "[::1]:80", "[::1]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := allowedHosts(tt.listen, tt.port, tt.ifaces)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestAllowedHosts_InterfaceError(t *testing.T) {
	_, err := allowedHosts("0.0.0.0", 8080, func() ([]net.Addr, error) { return nil, assert.AnError })
	require.ErrorIs(t, err, assert.AnError)
}

func TestGuardMiddleware_Host(t *testing.T) {
	g := newGuard([]string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"}, &logging.NoOpLogger{})

	tests := []struct {
		host string
		want int
	}{
		{"localhost:8080", http.StatusOK},
		{"127.0.0.1:8080", http.StatusOK},
		{"[::1]:8080", http.StatusOK},
		{"LOCALHOST:8080", http.StatusOK},
		{"evil.example.com:8080", http.StatusMisdirectedRequest},
		{"127.0.0.1:9999", http.StatusMisdirectedRequest},
		{"", http.StatusMisdirectedRequest},
		{"::1:8080", http.StatusMisdirectedRequest},
		{"localhost.:8080", http.StatusMisdirectedRequest},
		{"[::ffff:127.0.0.1]:8080", http.StatusMisdirectedRequest},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			called := false
			h := g.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true

				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, tt.want == http.StatusOK, called)
		})
	}
}
