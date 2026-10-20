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
