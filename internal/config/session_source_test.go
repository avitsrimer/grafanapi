// Package config (whitebox, not config_test) is intentional here: these tests drive the
// unexported rotatingRoundTripper type and SessionSource's unexported fields/methods directly
// (per docs/plans/20260722-auto-rotate-session-on-401.md, Task 1).
//
//nolint:testpackage // whitebox test: needs access to unexported rotatingRoundTripper/SessionSource internals
package config

import (
	"bytes"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory keychain.Store for tests local to package config (session_source_test.go
// is whitebox so it can construct SessionSource literals and drive rotatingRoundTripper directly).
type fakeStore struct {
	mu       sync.Mutex
	values   map[string]string
	setErr   error
	setCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]string{}}
}

func (f *fakeStore) Set(account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}

	f.values[account] = secret

	return nil
}

func (f *fakeStore) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return v, nil
}

func (f *fakeStore) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.values, account)

	return nil
}

// value returns the secret stored under testAccount, the fixed account name every test in this
// file uses.
func (f *fakeStore) value() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[testAccount]

	return v, ok
}

func (f *fakeStore) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.setCalls
}

// scriptedTransport is a hand-rolled http.RoundTripper (not a live server) used as the "next"
// transport wrapped by rotatingRoundTripper. It returns 200 when the inbound Cookie header
// carries newCookie, and 401 otherwise, recording every cookie value it observed.
type scriptedTransport struct {
	mu        sync.Mutex
	newCookie string
	seen      []string
}

func (t *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	cookie := req.Header.Get("Cookie")
	t.seen = append(t.seen, cookie)
	t.mu.Unlock()

	if cookie == CookieHeaderValue(t.newCookie) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}

	return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("unauthorized"))}, nil
}

func (t *scriptedTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.seen)
}

// testOldCookie is the cookie value every newRotateServer test uses as the "stale" session
// cookie sent on the first attempt (before rotation).
const testOldCookie = "old-cookie"

// testAccount is the fixed keychain account name used by every SessionSource built in this file.
const testAccount = "acct"

// testNewCookie is the cookie value every newRotateServer test rotates to.
const testNewCookie = "new-cookie"

// newRotateServer returns an httptest.Server implementing POST /api/user/auth-tokens/rotate:
// it asserts the inbound cookie equals testOldCookie, counts calls, and on success (status ==
// http.StatusOK) sets Set-Cookie: grafana_session=testNewCookie; otherwise it responds with
// status and no Set-Cookie header.
func newRotateServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/user/auth-tokens/rotate", r.URL.Path)
		assert.Equal(t, CookieHeaderValue(testOldCookie), r.Header.Get("Cookie"))

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}

		http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: testNewCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(server.Close)

	return server, &calls
}

func TestSessionSource_Rotate_HappyPath(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	newCookie, err := src.Rotate(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, testNewCookie, newCookie)
	assert.Equal(t, int32(1), calls.Load())

	gotCookie, gen := src.Current()
	assert.Equal(t, testNewCookie, gotCookie)
	assert.Equal(t, uint64(1), gen)

	stored, ok := store.value()
	require.True(t, ok)
	assert.Equal(t, testNewCookie, stored)
}

func TestSessionSource_Rotate_ShortCircuitsWhenAlreadyRotated(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	// Simulate another goroutine having already rotated past generation 0.
	src.mu.Lock()
	src.cookie = testNewCookie
	src.gen = 1
	src.mu.Unlock()

	cookie, err := src.Rotate(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, testNewCookie, cookie)
	assert.Equal(t, int32(0), calls.Load(), "no network call expected when generation already advanced")
}

func TestSessionSource_Rotate_Rejected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, _ := newRotateServer(t, status)

			store := newFakeStore()
			src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

			_, err := src.Rotate(t.Context(), 0)
			require.ErrorIs(t, err, ErrRotateUnauthorized)

			assert.Equal(t, 0, store.calls(), "keychain must not be written on a rejected rotation")
		})
	}
}

func TestSessionSource_Rotate_KeychainPersistFailure(t *testing.T) {
	server, _ := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	store.setErr = errors.New("keychain: boom")

	var warnBuf bytes.Buffer
	src := &SessionSource{
		cookie:  testOldCookie,
		server:  server.URL,
		store:   store,
		account: testAccount,
		warn:    &warnBuf,
	}

	newCookie, err := src.Rotate(t.Context(), 0)
	require.NoError(t, err, "a keychain persist failure must not fail the rotation")
	assert.Equal(t, testNewCookie, newCookie)
	assert.Contains(t, warnBuf.String(), "keychain: boom")

	gotCookie, gen := src.Current()
	assert.Equal(t, testNewCookie, gotCookie, "the in-memory cookie remains authoritative")
	assert.Equal(t, uint64(1), gen)
}

func TestSessionSource_Rotate_TrustsCustomCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, CookieHeaderValue(testOldCookie), r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: testNewCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, &TLS{CAData: caPEM}, store, testAccount)

	newCookie, err := src.Rotate(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, testNewCookie, newCookie)
}

func TestSessionSource_RotateClient_NoLoggingRoundTripper(t *testing.T) {
	src := NewSessionSource("cookie", "https://example.invalid", nil, newFakeStore(), testAccount)

	client := src.rotateClient()
	require.NotNil(t, client)

	// The rotate client must be built directly from a *http.Transport - never wrapped in a
	// request-dumping/logging round-tripper, so the cookie is never written anywhere.
	_, ok := client.Transport.(*http.Transport)
	assert.True(t, ok, "rotate client transport must be a plain *http.Transport")
	assert.Equal(t, rotateTimeout, client.Timeout)
}

func TestRotatingRoundTripper_HappyPath(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, store, testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://target.invalid/resource", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), rotateCalls.Load())
	require.Len(t, next.seen, 2)
	assert.Equal(t, CookieHeaderValue(testOldCookie), next.seen[0])
	assert.Equal(t, CookieHeaderValue(testNewCookie), next.seen[1])

	stored, ok := store.value()
	require.True(t, ok)
	assert.Equal(t, testNewCookie, stored)
}

func TestRotatingRoundTripper_SingleFlight(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, store, testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{src: src, next: next}

	const goroutines = 20

	var wg sync.WaitGroup
	statuses := make([]int, goroutines)

	for i := range goroutines {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			// assert (not require) here: require calls t.FailNow from a non-test goroutine,
			// which is unsafe - see https://pkg.go.dev/testing#T.FailNow.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://target.invalid/resource", nil)
			if !assert.NoError(t, err) {
				return
			}

			resp, rerr := rt.RoundTrip(req)
			if !assert.NoError(t, rerr) {
				return
			}
			defer resp.Body.Close()

			statuses[idx] = resp.StatusCode
		}(i)
	}

	wg.Wait()

	for _, status := range statuses {
		assert.Equal(t, http.StatusOK, status)
	}

	assert.Equal(t, int32(1), rotateCalls.Load(), "exactly one rotate call expected across concurrent 401s")
}

func TestRotatingRoundTripper_RotateRejected(t *testing.T) {
	rotateServer, _ := newRotateServer(t, http.StatusUnauthorized)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, store, testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://target.invalid/resource", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, 1, next.callCount(), "no retry should be attempted when rotation fails")

	_, ok := store.value()
	assert.False(t, ok, "keychain must not be written when rotation fails")
}

// opaqueReader wraps an io.Reader in a type http.NewRequest does not special-case for GetBody,
// simulating a genuinely non-rewindable request body.
type opaqueReader struct {
	io.Reader
}

func TestRotatingRoundTripper_NonRewindableBody(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, store, testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://target.invalid/resource",
		opaqueReader{strings.NewReader("payload")})
	require.NoError(t, err)
	require.Nil(t, req.GetBody, "precondition: opaqueReader must defeat http.NewRequest's GetBody detection")

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the original 401 must surface, unretried")
	assert.Equal(t, 1, next.callCount(), "no retry should be attempted for a non-rewindable body")
	assert.Equal(t, int32(1), rotateCalls.Load(), "rotation should still happen so the next request benefits")

	stored, ok := store.value()
	require.True(t, ok)
	assert.Equal(t, testNewCookie, stored)
}

func TestRotatingRoundTripper_GetBodyError(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, store, testAccount)

	next := &scriptedTransport{newCookie: testNewCookie}
	rt := &rotatingRoundTripper{src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://target.invalid/resource", strings.NewReader("payload"))
	require.NoError(t, err)
	require.NotNil(t, req.GetBody)

	req.GetBody = func() (io.ReadCloser, error) {
		return nil, errors.New("getbody: boom")
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the original 401 must surface, unretried")
	assert.Equal(t, 1, next.callCount(), "no retry should be attempted when GetBody fails")
	assert.Equal(t, int32(1), rotateCalls.Load(), "rotation should still happen so the next request benefits")
}

func TestWrapWithSession(t *testing.T) {
	t.Run("prefers rotating round tripper when Session is set", func(t *testing.T) {
		rotateServer, _ := newRotateServer(t, http.StatusOK)
		src := NewSessionSource(testOldCookie, rotateServer.URL, nil, newFakeStore(), testAccount)

		cfg := &GrafanaConfig{SessionCookie: "static-cookie", Session: src}
		next := &recorderRoundTripper{}

		wrapped := cfg.WrapWithSession(next)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://target.invalid/resource", nil)
		require.NoError(t, err)

		resp, err := wrapped.RoundTrip(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.NotNil(t, next.req)
		assert.Equal(t, CookieHeaderValue(testOldCookie), next.req.Header.Get("Cookie"))
	})

	t.Run("falls back to the static cookie round tripper", func(t *testing.T) {
		cfg := &GrafanaConfig{SessionCookie: "static-cookie"}
		next := &recorderRoundTripper{}

		wrapped := cfg.WrapWithSession(next)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://target.invalid/resource", nil)
		require.NoError(t, err)

		resp, err := wrapped.RoundTrip(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.NotNil(t, next.req)
		assert.Equal(t, CookieHeaderValue("static-cookie"), next.req.Header.Get("Cookie"))
	})

	t.Run("returns next unchanged when neither is set", func(t *testing.T) {
		cfg := &GrafanaConfig{}
		next := &recorderRoundTripper{}

		wrapped := cfg.WrapWithSession(next)
		assert.Same(t, http.RoundTripper(next), wrapped)
	})
}

// recorderRoundTripper captures the last request it saw and returns a canned 200 response.
type recorderRoundTripper struct {
	req *http.Request
}

func (rt *recorderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.req = req

	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func TestBuildRotateURL(t *testing.T) {
	u, err := buildRotateURL("https://grafana.example.com/grafana/")
	require.NoError(t, err)
	assert.Equal(t, "https://grafana.example.com/grafana/api/user/auth-tokens/rotate", u.String())

	u, err = buildRotateURL("https://grafana.example.com")
	require.NoError(t, err)
	assert.Equal(t, "https://grafana.example.com/api/user/auth-tokens/rotate", u.String())
}

func TestExtractSessionCookie(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", (&http.Cookie{Name: SessionCookieName, Value: testNewCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())

	cookie, err := extractSessionCookie(resp)
	require.NoError(t, err)
	assert.Equal(t, testNewCookie, cookie)

	emptyResp := &http.Response{Header: http.Header{}}
	_, err = extractSessionCookie(emptyResp)
	require.Error(t, err)
}
