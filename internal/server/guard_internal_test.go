package server

import (
	"fmt"
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

const testToken = "test-token-0123456789"

type recordingLogger struct {
	logging.NoOpLogger

	lines []string
}

func (l *recordingLogger) Warn(msg string, ctx ...any) {
	l.lines = append(l.lines, fmt.Sprint(append([]any{msg}, ctx...)...))
}

func testGuard() *guard {
	return newGuard([]string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"}, testToken, 8080, &logging.NoOpLogger{})
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
	g := testGuard()

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
			req.Header.Set("Cookie", g.cookieName+"="+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, tt.want == http.StatusOK, called)
		})
	}
}

func serveGuarded(t *testing.T, g *guard, method, target string, cookie bool) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	called := false
	h := g.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	req.Host = "127.0.0.1:8080"

	if cookie {
		req.Header.Set("Cookie", g.cookieName+"="+testToken)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec, called
}

func TestGuardMiddleware_Token(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		cookie     bool
		wantCode   int
		wantNext   bool
		wantCookie bool
	}{
		{name: "no cookie no query", method: http.MethodGet, target: "/", wantCode: http.StatusUnauthorized},
		{name: "valid cookie no query", method: http.MethodGet, target: "/", cookie: true, wantCode: http.StatusOK, wantNext: true},
		{name: "valid token GET", method: http.MethodGet, target: "/?token=" + testToken, wantCode: http.StatusSeeOther, wantCookie: true},
		{name: "valid token HEAD", method: http.MethodHead, target: "/?token=" + testToken, wantCode: http.StatusSeeOther, wantCookie: true},
		{name: "valid token POST", method: http.MethodPost, target: "/?token=" + testToken, wantCode: http.StatusUnauthorized},
		{name: "wrong token GET", method: http.MethodGet, target: "/?token=nope", wantCode: http.StatusUnauthorized},
		{name: "empty token GET", method: http.MethodGet, target: "/?token=", wantCode: http.StatusUnauthorized},
		{name: "protocol-relative path", method: http.MethodGet, target: "//evil.example.com/?token=" + testToken, wantCode: http.StatusSeeOther, wantCookie: true},
		{name: "cookie plus token GET", method: http.MethodGet, target: "/?token=" + testToken, cookie: true, wantCode: http.StatusSeeOther, wantCookie: true},
		{name: "cookie plus token POST", method: http.MethodPost, target: "/api/ds/query?token=" + testToken, cookie: true, wantCode: http.StatusUnauthorized},
		{name: "cookie plus wrong token GET", method: http.MethodGet, target: "/?token=nope", cookie: true, wantCode: http.StatusUnauthorized},
		{name: "token after other param", method: http.MethodPost, target: "/x?a=1&token=" + testToken, cookie: true, wantCode: http.StatusUnauthorized},
		{name: "semicolon separated token", method: http.MethodGet, target: "/x?a=1;token=" + testToken, cookie: true, wantCode: http.StatusUnauthorized},
		{name: "escaped key", method: http.MethodGet, target: "/x?%74oken=" + testToken, cookie: true, wantCode: http.StatusSeeOther, wantCookie: true},
		{name: "undecodable value", method: http.MethodGet, target: "/x?token=%zz", cookie: true, wantCode: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, called := serveGuarded(t, testGuard(), tt.method, tt.target, tt.cookie)

			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Equal(t, tt.wantNext, called)

			if !tt.wantCookie {
				assert.Empty(t, rec.Result().Cookies())
				return
			}

			assert.Equal(t, "/", rec.Header().Get("Location"))
			assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))

			cookies := rec.Result().Cookies()
			require.Len(t, cookies, 1)

			c := cookies[0]
			assert.Equal(t, "grafanapi_serve_8080", c.Name)
			assert.Equal(t, testToken, c.Value)
			assert.Equal(t, "/", c.Path)
			assert.True(t, c.HttpOnly)
			assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
			assert.False(t, c.Secure)
			assert.Zero(t, c.MaxAge)
			assert.True(t, c.Expires.IsZero())
		})
	}
}

func TestGuardMiddleware_WrongCookie(t *testing.T) {
	g := testGuard()
	h := g.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not run") }))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Cookie", g.cookieName+"=wrong")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), testToken)
}

func TestGuardMiddleware_HostCheckRunsFirst(t *testing.T) {
	g := testGuard()
	h := g.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not run") }))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Host = "evil.example.com:8080"
	req.Header.Set("Cookie", g.cookieName+"="+testToken)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMisdirectedRequest, rec.Code)
}

func TestGuardMiddleware_RejectionLogOmitsToken(t *testing.T) {
	logger := &recordingLogger{}
	g := newGuard([]string{"127.0.0.1:8080"}, testToken, 8080, logger)

	// A wrong token is the realistic leak: the attacker-supplied value must not be logged either.
	serveGuarded(t, g, http.MethodPost, "/api/ds/query?token="+testToken, true)
	serveGuarded(t, g, http.MethodGet, "/?token=guess-value", false)

	require.Len(t, logger.lines, 2)

	for _, line := range logger.lines {
		assert.NotContains(t, line, testToken)
		assert.NotContains(t, line, "guess-value")
		assert.NotContains(t, line, "token=")
	}
}
