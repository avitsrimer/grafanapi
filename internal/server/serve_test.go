package server_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

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

	assert.Equal(t, "http://127.0.0.1:"+port+"/", s.URL())
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

	assert.Equal(t, "http://[::1]:"+strconv.Itoa(tcpAddr.Port)+"/", s.URL())
	assert.True(t, strings.HasPrefix(s.URL(), "http://[::1]:"))
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
