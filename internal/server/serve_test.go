package server_test

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/grafana/grafanapi/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(listenAddr string) *server.Server {
	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: "https://grafana.example.com"}}

	return server.New(server.Config{ListenAddr: listenAddr, Port: 0, NoColor: true}, gCtx, resources.NewResources())
}

func TestServer_ListenReportsBoundPort(t *testing.T) {
	s := newTestServer("127.0.0.1")

	ln, err := s.Listen(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NotEqual(t, "0", port)

	assert.True(t, strings.HasPrefix(s.URL(), "http://127.0.0.1:"+port+"/?token="), s.URL())
}

func TestServer_ListenIPv6Loopback(t *testing.T) {
	s := newTestServer("::1")

	ln, err := s.Listen(t.Context())
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)

	assert.True(t, strings.HasPrefix(s.URL(), "http://[::1]:"+strconv.Itoa(tcpAddr.Port)+"/?token="), s.URL())
}

func TestServer_ServeReturnsNilAfterContextCancel(t *testing.T) {
	s := newTestServer("127.0.0.1")

	ln, err := s.Listen(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx, ln) }()

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-t.Context().Done():
		t.Fatal("test context ended before Serve returned")
	}
}

func TestServer_RejectsForeignHost(t *testing.T) {
	var backendHits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		backendHits.Add(1)
	}))
	t.Cleanup(backend.Close)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL}}
	s := server.New(server.Config{ListenAddr: "127.0.0.1", NoColor: true}, gCtx, resources.NewResources())

	ln, err := s.Listen(t.Context())
	require.NoError(t, err)

	go func() { _ = s.Serve(t.Context(), ln) }()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+ln.Addr().String()+"/api/ds/query", nil)
	require.NoError(t, err)

	req.Host = "evil.example.com:" + port

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusMisdirectedRequest, resp.StatusCode)
	assert.Zero(t, backendHits.Load())
}

type tokenFixture struct {
	server  *server.Server
	addr    string
	backend *atomic.Int32
}

func startTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()

	hits := &atomic.Int32{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL}}
	s := server.New(server.Config{ListenAddr: "127.0.0.1", NoColor: true}, gCtx, resources.NewResources())

	ln, err := s.Listen(t.Context())
	require.NoError(t, err)

	go func() { _ = s.Serve(t.Context(), ln) }()

	return &tokenFixture{server: s, addr: ln.Addr().String(), backend: hits}
}

type result struct {
	status   int
	finalURL string
}

func (f *tokenFixture) do(t *testing.T, client *http.Client, method, target string) result {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return result{status: resp.StatusCode, finalURL: resp.Request.URL.String()}
}

func jarClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{Jar: jar}
}

func TestServer_AccessToken(t *testing.T) {
	f := startTokenFixture(t)
	client := jarClient(t)

	// Redeem the printed URL: 303 to / (followed), then the index is served.
	resp := f.do(t, client, http.MethodGet, f.server.URL())
	assert.Equal(t, http.StatusOK, resp.status)
	assert.Equal(t, "http://"+f.addr+"/", resp.finalURL)

	// The cookie now authenticates a proxied POST.
	resp = f.do(t, client, http.MethodPost, "http://"+f.addr+"/api/ds/query")
	assert.Equal(t, http.StatusOK, resp.status)
	assert.Equal(t, int32(1), f.backend.Load())

	// Re-opening the printed URL with the cookie already set redeems again and never proxies.
	noFollow := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp = f.do(t, noFollow, http.MethodGet, f.server.URL())
	assert.Equal(t, http.StatusSeeOther, resp.status)
	assert.Equal(t, int32(1), f.backend.Load())
}

func TestServer_TokenQueryNeverReachesBackend(t *testing.T) {
	f := startTokenFixture(t)
	client := jarClient(t)

	f.do(t, client, http.MethodGet, f.server.URL())

	u, err := url.Parse(f.server.URL())
	require.NoError(t, err)

	resp := f.do(t, client, http.MethodPost, "http://"+f.addr+"/api/ds/query?"+u.RawQuery)
	assert.Equal(t, http.StatusUnauthorized, resp.status)
	assert.Zero(t, f.backend.Load())
}

func TestServer_RejectsMissingToken(t *testing.T) {
	f := startTokenFixture(t)

	resp := f.do(t, &http.Client{}, http.MethodPost, "http://"+f.addr+"/api/ds/query")
	assert.Equal(t, http.StatusUnauthorized, resp.status)
	assert.Zero(t, f.backend.Load())

	_, wsResp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws://"+f.addr+"/livereload", nil)
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	require.NoError(t, wsResp.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, wsResp.StatusCode)
}

type recordedRequest struct {
	origin     string
	hasOrigin  bool
	cookie     string
	hasCookie  bool
	requestURI string
}

type postResult struct {
	status int
	header http.Header
}

type proxyFixture struct {
	addr     string
	backend  string
	token    string
	mu       *sync.Mutex
	requests *[]recordedRequest
}

func startProxyFixture(t *testing.T, sessionCookie string) *proxyFixture {
	t.Helper()

	var (
		mu       sync.Mutex
		requests []recordedRequest
	)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, recordedRequest{
			origin:     r.Header.Get("Origin"),
			hasOrigin:  len(r.Header.Values("Origin")) > 0,
			cookie:     r.Header.Get("Cookie"),
			hasCookie:  len(r.Header.Values("Cookie")) > 0,
			requestURI: r.RequestURI,
		})
		mu.Unlock()

		w.Header().Add("Set-Cookie", "backend_cookie=x; Path=/")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: sessionCookie}}
	s := server.New(server.Config{ListenAddr: "127.0.0.1", NoColor: true}, gCtx, resources.NewResources())

	ln, err := s.Listen(t.Context())
	require.NoError(t, err)

	go func() { _ = s.Serve(t.Context(), ln) }()

	u, err := url.Parse(s.URL())
	require.NoError(t, err)

	return &proxyFixture{addr: ln.Addr().String(), backend: backend.URL, token: u.Query().Get("token"), mu: &mu, requests: &requests}
}

func (f *proxyFixture) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]recordedRequest(nil), *f.requests...)
}

// post sends a POST /api/ds/query with the serve cookie plus extra, and an optional Origin.
func (f *proxyFixture) post(t *testing.T, origin, extraCookies string) postResult {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+f.addr+"/api/ds/query", nil)
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(f.addr)
	require.NoError(t, err)

	cookie := "grafanapi_serve_" + port + "=" + f.token
	if extraCookies != "" {
		cookie += "; " + extraCookies
	}

	req.Header.Set("Cookie", cookie)

	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return postResult{status: resp.StatusCode, header: resp.Header}
}

func TestServer_ProxyRewritesVettedOrigin(t *testing.T) {
	f := startProxyFixture(t, "")

	resp := f.post(t, "http://"+f.addr, "")
	require.Equal(t, http.StatusOK, resp.status)

	got := f.recorded()
	require.Len(t, got, 1)
	assert.True(t, got[0].hasOrigin)
	assert.Equal(t, f.backend, got[0].origin)
}

func TestServer_ProxySendsNoOriginWhenNoneArrived(t *testing.T) {
	f := startProxyFixture(t, "")

	resp := f.post(t, "", "")
	require.Equal(t, http.StatusOK, resp.status)

	got := f.recorded()
	require.Len(t, got, 1)
	assert.False(t, got[0].hasOrigin)
}

func TestServer_RejectsForeignOrigin(t *testing.T) {
	f := startProxyFixture(t, "")

	resp := f.post(t, "http://evil.example.com", "")

	assert.Equal(t, http.StatusForbidden, resp.status)
	assert.Empty(t, f.recorded())
	assert.Empty(t, resp.header.Get("Access-Control-Allow-Origin"))
}

func TestServer_BrowserCookieNeverReachesBackend(t *testing.T) {
	t.Run("no session configured", func(t *testing.T) {
		f := startProxyFixture(t, "")

		f.post(t, "", "other=1")

		got := f.recorded()
		require.Len(t, got, 1)
		assert.False(t, got[0].hasCookie, got[0].cookie)
	})

	t.Run("session configured", func(t *testing.T) {
		f := startProxyFixture(t, "fixture-session")

		f.post(t, "", "other=1")

		got := f.recorded()
		require.Len(t, got, 1)
		assert.Equal(t, "grafana_session=fixture-session", got[0].cookie)
	})
}

func TestServer_BackendSetCookieIsStripped(t *testing.T) {
	f := startProxyFixture(t, "")

	resp := f.post(t, "", "")

	require.Len(t, f.recorded(), 1)
	assert.Empty(t, resp.header.Values("Set-Cookie"))
}
