// Package config (whitebox, not config_test) is intentional here: these tests drive the
// unexported round-trippers and their server fields directly.
//
//nolint:testpackage // whitebox test: needs access to unexported sessionCookieRoundTripper/rotatingRoundTripper
package config

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeRecorder is a request body that records whether it was closed.
type closeRecorder struct {
	io.Reader

	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)

	return nil
}

// countingTransport answers 200 and records how many requests reached it.
type countingTransport struct {
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls.Add(1)

	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSessionRoundTrippers_RefuseUnsafeTargets(t *testing.T) {
	const server = "https://grafana.example.com"

	tests := []struct {
		name    string
		server  string
		target  string
		wantErr error
	}{
		{name: "other host", server: server, target: "https://other.example.com/api", wantErr: ErrUnsafeRedirect},
		{name: "other port", server: server, target: "https://grafana.example.com:8443/api", wantErr: ErrUnsafeRedirect},
		{name: "scheme downgrade", server: server, target: "http://grafana.example.com/api", wantErr: ErrUnsafeRedirect},
		{name: "cleartext remote server", server: "http://grafana.example.com", target: "http://grafana.example.com/api", wantErr: ErrInvalidServerURL},
	}

	for _, tc := range tests {
		for _, kind := range []string{"static", "rotating"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)
				next := &countingTransport{}

				var rt http.RoundTripper
				if kind == "static" {
					rt = &sessionCookieRoundTripper{server: tc.server, cookie: testOldCookie, next: next}
				} else {
					src := NewSessionSource(testOldCookie, rotateServer.URL, nil, newFakeStore(), testAccount)
					rt = &rotatingRoundTripper{server: tc.server, src: src, next: next}
				}

				body := &closeRecorder{Reader: strings.NewReader("payload")}
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.target, body)
				require.NoError(t, err)

				resp, err := rt.RoundTrip(req)
				if resp != nil {
					defer resp.Body.Close()
				}

				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, resp)
				assert.Zero(t, next.calls.Load(), "next must never be called")
				assert.Zero(t, rotateCalls.Load(), "an unsafe request must never trigger a rotation")
				assert.True(t, body.closed.Load(), "the request body must be closed on refusal")
			})
		}
	}
}

func TestSessionRoundTrippers_AllowEquivalentOrigin(t *testing.T) {
	tests := []struct {
		name   string
		server string
		target string
	}{
		{name: "host case", server: "https://grafana.example.com", target: "https://GRAFANA.example.com/api"},
		{name: "explicit default port", server: "https://grafana.example.com", target: "https://grafana.example.com:443/api"},
		{name: "server with sub-path", server: "https://grafana.example.com/grafana/", target: "https://grafana.example.com/api"},
	}

	for _, tc := range tests {
		for _, kind := range []string{"static", "rotating"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				rotateServer, _ := newRotateServer(t, http.StatusOK)
				next := &recorderRoundTripper{}

				var rt http.RoundTripper
				if kind == "static" {
					rt = &sessionCookieRoundTripper{server: tc.server, cookie: testOldCookie, next: next}
				} else {
					src := NewSessionSource(testOldCookie, rotateServer.URL, nil, newFakeStore(), testAccount)
					rt = &rotatingRoundTripper{server: tc.server, src: src, next: next}
				}

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.target, nil)
				require.NoError(t, err)

				resp, err := rt.RoundTrip(req)
				require.NoError(t, err)
				defer resp.Body.Close()

				require.NotNil(t, next.req)
				assert.Equal(t, CookieHeaderValue(testOldCookie), next.req.Header.Get("Cookie"))
			})
		}
	}
}

func TestRotatingRoundTripper_RetryCarriesNewCookieOnServerOrigin(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, newFakeStore(), testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{server: rotateServer.URL, src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rotateServer.URL+"/resource", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{CookieHeaderValue(testOldCookie), CookieHeaderValue(testNewCookie)}, next.seen)
	assert.Equal(t, int32(1), rotateCalls.Load(), "exactly one rotation")
}

func TestRotatingRoundTripper_ReplaysBodyOnServerOrigin(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, newFakeStore(), testAccount)

	var bodies []string

	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}

		bodies = append(bodies, string(data))

		status := http.StatusUnauthorized
		if req.Header.Get("Cookie") == CookieHeaderValue(testNewCookie) {
			status = http.StatusOK
		}

		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	rt := &rotatingRoundTripper{server: rotateServer.URL, src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rotateServer.URL+"/resource", bytes.NewReader([]byte("payload")))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"payload", "payload"}, bodies)
	assert.Equal(t, int32(1), rotateCalls.Load())
}

// TestRotatingRoundTripper_ServerDiffersFromSourceServer pins the shape the explore/alerts tests
// use: the API server (rt.server) and the rotate server (src.server) are different loopback
// origins. The guard follows rt.server; rotation still goes to src.server.
func TestRotatingRoundTripper_ServerDiffersFromSourceServer(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)
	apiServer, _ := newRotateServer(t, http.StatusOK)
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, newFakeStore(), testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{server: apiServer.URL, src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiServer.URL+"/resource", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), rotateCalls.Load(), "rotation hits src.server")

	// A request to the rotate server's origin is off rt.server and is refused.
	other, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rotateServer.URL+"/resource", nil)
	require.NoError(t, err)

	_, err = rt.RoundTrip(other) //nolint:bodyclose // refused, no response
	require.ErrorIs(t, err, ErrUnsafeRedirect)
}
