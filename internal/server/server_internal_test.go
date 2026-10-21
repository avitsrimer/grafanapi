package server

import (
	"bytes"
	"io/fs"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrowseURL_Token(t *testing.T) {
	assert.Equal(t, "http://127.0.0.1:8080/?token=abc", browseURL("127.0.0.1", 8080, "abc"))
	assert.Equal(t, "http://127.0.0.1:8080/?token=a%26b", browseURL("0.0.0.0", 8080, "a&b"))
}

func TestBrowseURL(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		want       string
	}{
		{name: "empty is wildcard", listenAddr: "", want: "http://127.0.0.1:8080/"},
		{name: "ipv4 wildcard", listenAddr: "0.0.0.0", want: "http://127.0.0.1:8080/"},
		{name: "ipv6 wildcard", listenAddr: "::", want: "http://127.0.0.1:8080/"},
		{name: "ipv6 wildcard long form", listenAddr: "::0", want: "http://127.0.0.1:8080/"},
		{name: "ipv6 wildcard expanded", listenAddr: "0:0:0:0:0:0:0:0", want: "http://127.0.0.1:8080/"},
		{name: "ipv4 loopback", listenAddr: "127.0.0.1", want: "http://127.0.0.1:8080/"},
		{name: "ipv6 loopback", listenAddr: "::1", want: "http://[::1]:8080/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, browseURL(tt.listenAddr, 8080, ""))
		})
	}
}

// The websocket must outlive the server's Read/WriteTimeout: gorilla clears the connection
// deadlines on upgrade while the upgrader's HandshakeTimeout is zero.
func TestServe_WebsocketSurvivesServerTimeouts(t *testing.T) {
	const timeout = 200 * time.Millisecond

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: "https://grafana.example.com"}}
	s := New(Config{ListenAddr: "127.0.0.1", NoColor: true}, gCtx, resources.NewResources())
	s.timeouts = timeouts{readHeader: timeout, read: timeout, write: timeout, idle: timeout}

	ln, err := s.Listen(t.Context())
	require.NoError(t, err)

	ctx := t.Context()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx, ln) }()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, "ws://"+ln.Addr().String()+"/livereload", http.Header{
		"Cookie": {tokenCookiePrefix + strconv.Itoa(s.port) + "=" + s.token},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, resp.Body.Close())

	// What is under test is a deadline, so this is a deliberate wait rather than a synchronisation sleep.
	select {
	case <-time.After(3 * timeout):
	case <-ctx.Done():
		t.Fatal("test context ended early")
	}

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"command":"hello"}`)))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), `"command": "hello"`)
}

func TestIframeTemplate_LiveReloadTargetsPageHost(t *testing.T) {
	var out bytes.Buffer

	// The same variables iframeHandler passes; missingkey=error makes a stale key fail the render.
	err := renderTemplate(&out, "proxy/iframe.html.tmpl", map[string]any{
		"IframeURL":      "/d/uid/slug",
		"CurrentContext": &config.Context{Name: "ctx"},
	})
	require.NoError(t, err)

	assert.Contains(t, out.String(), "host: window.location.hostname")
	assert.Contains(t, out.String(), "Number(window.location.port) || 80")
	assert.NotContains(t, out.String(), "{{")
}

func TestTemplates_UseGrafanapiPaths(t *testing.T) {
	err := fs.WalkDir(embedFS, "embed/templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		contents, err := embedFS.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(contents), "/grafanactl/", path)

		return nil
	})
	require.NoError(t, err)
}
