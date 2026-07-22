// Package config (whitebox, not config_test) is intentional here: these tests drive the
// unexported rotatingRoundTripper type and SessionSource's unexported fields/methods directly
// (per docs/plans/20260722-auto-rotate-session-on-401.md, Task 1).
//
//nolint:testpackage // whitebox test: needs access to unexported rotatingRoundTripper/SessionSource internals
package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestSessionSource_Rotate_FailurePublicationIsAtomic is the regression test for finding 1: before
// the fix, a failed rotation cleared s.inflight and released mu BEFORE call.cookie/call.err were set
// and call.done closed, leaving a window in which a concurrent Rotate call for the same
// (still-unadvanced, since it failed) generation could see inflight == nil and start a second,
// redundant rotate. This drives N goroutines against a rotate endpoint that always fails, gated so
// every straggler arrives while the single winning goroutine's rotation is genuinely in flight, and
// asserts the endpoint is hit exactly once for that concurrent wave and every goroutine observes the
// identical error - proving the single-flight dedup holds even on the failure path.
func TestSessionSource_Rotate_FailurePublicationIsAtomic(t *testing.T) {
	var calls atomic.Int32

	release := make(chan struct{})
	arrived := make(chan struct{})
	var arrivedOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		arrivedOnce.Do(func() { close(arrived) })
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	const goroutines = 20

	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	// Kick off the goroutine that will win the single-flight race and block inside the handler.
	wg.Go(func() {
		_, errs[0] = src.Rotate(t.Context(), 0)
	})

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("rotate handler never received the first request")
	}

	// Now that the winner is confirmed in-flight (blocked in the handler, holding no lock), pile on
	// the rest of the concurrent callers for the same generation - they must join the in-flight call
	// via s.inflight, not start their own.
	for i := 1; i < goroutines; i++ {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = src.Rotate(t.Context(), 0)
		}(i)
	}

	// Give the stragglers a moment to reach the inflight check before releasing the handler.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load(), "exactly one rotate call expected for the concurrent wave")

	for i, err := range errs {
		if assert.Errorf(t, err, "goroutine %d expected an error", i) {
			assert.Containsf(t, err.Error(), "unexpected status 500", "goroutine %d", i)
		}
	}

	assert.Equal(t, 0, store.calls(), "keychain must not be written on a rejected rotation")

	// A later Rotate for the same (still-unadvanced) generation must be able to start a fresh
	// attempt - the earlier failure must not have wedged the inflight/gen state.
	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Equal(t, int32(2), calls.Load(), "a subsequent Rotate call must be able to retry")
}

// TestSessionSource_Rotate_PanicDoesNotDeadlock is the regression test for finding 2: before the
// fix, a panic during the winning goroutine's unlocked doRotate call left s.inflight set forever and
// call.done never closed, permanently deadlocking every future Rotate call on the same
// SessionSource. Injecting a genuine panic from the real doRotate (an unexported method with no
// test seam of its own) is impractical, so this test uses rotateFunc - the unexported field
// runRotate consults in place of doRotate, added specifically so tests can simulate this failure
// mode - to panic on the first call, then asserts: (a) Rotate recovers the panic into an error
// rather than propagating it, and (b) a second, independent Rotate call for the same generation
// completes rather than blocking forever, proving s.inflight/call.done were correctly cleaned up.
func TestSessionSource_Rotate_PanicDoesNotDeadlock(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	var panicked atomic.Bool
	src.rotateFunc = func(ctx context.Context, cookie string) (string, error) {
		if !panicked.Swap(true) {
			panic("simulated doRotate panic")
		}

		return src.doRotate(ctx, cookie)
	}

	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panicked")
	assert.Equal(t, int32(0), calls.Load(), "the panicking call must never have reached the real rotate endpoint")

	// The panic must not have left s.inflight set or call.done unclosed: a fresh Rotate call for the
	// same (still-unadvanced) generation must complete rather than block forever.
	done := make(chan struct{})
	go func() {
		defer close(done)

		newCookie, rerr := src.Rotate(t.Context(), 0)
		assert.NoError(t, rerr)
		assert.Equal(t, testNewCookie, newCookie)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Rotate deadlocked after a panicking rotation - s.inflight/call.done were not cleaned up")
	}

	assert.Equal(t, int32(1), calls.Load())
}

// panickingStore wraps a *fakeStore but panics on Set, simulating a misbehaving keychain.Store
// implementation - the easiest real injection point for exercising the defensive recover in
// (*SessionSource).persist (see TestSessionSource_Rotate_PersistPanicDoesNotCrash), since Set runs
// in the same unlocked, winner-only section of Rotate that doRotate itself runs in.
type panickingStore struct{ *fakeStore }

func (panickingStore) Set(string, string) error {
	panic("simulated keychain Set panic")
}

// TestSessionSource_Rotate_PersistPanicDoesNotCrash locks in persist's defensive recover: by the
// time persist runs, the rotation has already succeeded and been published to every waiter, so a
// panic there cannot deadlock anything - but an unrecovered panic would still crash the whole
// process (an unrecovered panic in any goroutine terminates the program), taking down every other
// in-flight request. This asserts the panic is instead turned into a warning, exactly like a
// returned Set error, and that a later Rotate call is unaffected.
func TestSessionSource_Rotate_PersistPanicDoesNotCrash(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)

	store := panickingStore{newFakeStore()}

	var warnBuf bytes.Buffer
	src := &SessionSource{
		cookie:  testOldCookie,
		server:  server.URL,
		store:   store,
		account: testAccount,
		warn:    &warnBuf,
	}

	newCookie, err := src.Rotate(t.Context(), 0)
	require.NoError(t, err, "a persist panic must not fail the rotation")
	assert.Equal(t, testNewCookie, newCookie)
	assert.Contains(t, warnBuf.String(), "panic")

	gotCookie, gen := src.Current()
	assert.Equal(t, testNewCookie, gotCookie)
	assert.Equal(t, uint64(1), gen)

	// A later Rotate call (for the generation the caller now observes as stale) must be unaffected
	// by the earlier persist panic: it short-circuits with no network call, since gen already
	// advanced past 0.
	newCookie, err = src.Rotate(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, testNewCookie, newCookie)
	assert.Equal(t, int32(1), calls.Load(), "no second network call expected: generation already advanced")
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

	client, err := src.rotateClient()
	require.NoError(t, err)
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

// TestExtractSessionCookie_EmptyValue covers a Set-Cookie header that names grafana_session but
// carries an empty value (e.g. a server clearing the cookie): extractSessionCookie must not treat
// that as a successful rotation - an empty cookie is never a usable session.
func TestExtractSessionCookie_EmptyValue(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", (&http.Cookie{Name: SessionCookieName, Value: "", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())

	_, err := extractSessionCookie(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no "+SessionCookieName+" cookie")
}

// TestSessionSource_Rotate_UnexpectedStatus covers doRotate's default case: a rotate endpoint
// status that is neither 200 nor 401/403 (e.g. a 500 from an upstream outage) is a genuine error,
// distinct from ErrRotateUnauthorized, and must not be persisted to the keychain.
func TestSessionSource_Rotate_UnexpectedStatus(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusInternalServerError)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 500")
	require.NotErrorIs(t, err, ErrRotateUnauthorized, "a 500 is not the same failure mode as a rejected rotation")
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, 0, store.calls(), "keychain must not be written on an unexpected rotate status")
}

// TestSessionSource_Rotate_MalformedServerURL covers buildRotateURL's error path as surfaced
// through the public Rotate method: a server address url.Parse itself rejects.
func TestSessionSource_Rotate_MalformedServerURL(t *testing.T) {
	store := newFakeStore()
	src := NewSessionSource(testOldCookie, "://not-a-valid-url", nil, store, testAccount)

	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid server address")
	assert.Equal(t, 0, store.calls())
}

// TestSessionSource_Rotate_NetworkFailure covers doRotate's client.Do error path: the rotate
// endpoint is unreachable (connection refused), as opposed to reachable-but-rejecting.
func TestSessionSource_Rotate_NetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverURL := server.URL
	server.Close() // nothing listens at serverURL anymore

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, serverURL, nil, store, testAccount)

	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session rotation:")
	assert.Equal(t, 0, store.calls())
}

// TestSessionSource_Rotate_DecouplesFromCallerContextCancellation locks in doRotate's use of
// context.WithoutCancel: a rotation that is already running must complete (and unblock every
// waiter) even though the specific caller that triggered it has since given up.
func TestSessionSource_Rotate_DecouplesFromCallerContextCancellation(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(150 * time.Millisecond) // long enough to outlive the already-canceled caller context below
		http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: testNewCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	callerCtx, cancel := context.WithCancel(t.Context())
	cancel() // the caller's own request context is already dead before Rotate is even invoked

	newCookie, err := src.Rotate(callerCtx, 0)
	require.NoError(t, err, "the rotate network call must not inherit the caller's cancellation")
	assert.Equal(t, testNewCookie, newCookie)
	assert.Equal(t, int32(1), calls.Load())
}

// TestSessionSource_Current_DoesNotBlockDuringRotation is the regression test for the mutex
// restructuring: Current() must return immediately with the OLD cookie while a rotation is
// in-flight in another goroutine, never blocking for the duration of the (potentially many-second)
// network round trip. Before the fix, Rotate held s.mu for its entire network call, so Current()
// - called on every outbound request, including ones unrelated to the 401 that triggered the
// rotation - would stall for as long as the rotate call took.
func TestSessionSource_Current_DoesNotBlockDuringRotation(t *testing.T) {
	release := make(chan struct{})
	rotateStarted := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(rotateStarted)
		<-release
		http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: testNewCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, nil, store, testAccount)

	rotateDone := make(chan struct{})
	go func() {
		defer close(rotateDone)
		_, _ = src.Rotate(t.Context(), 0)
	}()

	select {
	case <-rotateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("rotate handler never received the request")
	}

	// The rotation is now blocked inside the network call, holding no lock. Current() must return
	// immediately with the OLD cookie, not block until the rotation completes.
	currentDone := make(chan struct{})
	go func() {
		defer close(currentDone)

		cookie, gen := src.Current()
		assert.Equal(t, testOldCookie, cookie)
		assert.Equal(t, uint64(0), gen)
	}()

	select {
	case <-currentDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Current() blocked while a rotation was in flight")
	}

	close(release)
	<-rotateDone
}

// generateTestClientCert returns a PEM-encoded self-signed certificate/key pair (RSA, ClientAuth
// EKU) usable both as TLS.CertData/KeyData (the rotate client's own client certificate) and, via
// the returned *x509.Certificate, as the sole entry in a test TLS server's ClientCAs pool.
func generateTestClientCert(t *testing.T) ([]byte, []byte, *x509.Certificate) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "grafanapi-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	return certPEM, keyPEM, cert
}

// TestSessionSource_Rotate_MutualTLS is the end-to-end mTLS wiring test: the rotate client built
// from a TLS config carrying CertData/KeyData actually presents that client certificate, and a
// server that requires and verifies one (tls.RequireAndVerifyClientCert) accepts it.
func TestSessionSource_Rotate_MutualTLS(t *testing.T) {
	clientCertPEM, clientKeyPEM, clientCert := generateTestClientCert(t)

	var sawPeerCert bool

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPeerCert = len(r.TLS.PeerCertificates) > 0
		assert.Equal(t, CookieHeaderValue(testOldCookie), r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: testNewCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))

	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(clientCert)
	server.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	serverCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, server.URL, &TLS{
		CAData:   serverCAPEM,
		CertData: clientCertPEM,
		KeyData:  clientKeyPEM,
	}, store, testAccount)

	newCookie, err := src.Rotate(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, testNewCookie, newCookie)
	assert.True(t, sawPeerCert, "the server must have received the client certificate")
}

// TestSessionSource_Rotate_MalformedCAData ties finding 4 (TLS.ToStdTLSConfig propagating PEM
// errors) to the rotate client: a context configured with garbage CAData must surface an error
// from Rotate rather than silently building a client that trusts the system root pool instead.
func TestSessionSource_Rotate_MalformedCAData(t *testing.T) {
	store := newFakeStore()
	src := NewSessionSource(testOldCookie, "https://example.invalid", &TLS{CAData: []byte("not a certificate")}, store, testAccount)

	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca-data")
	assert.Equal(t, 0, store.calls())
}

// TestSessionSource_Rotate_MalformedClientCertificate is TestSessionSource_Rotate_MalformedCAData's
// counterpart for CertData/KeyData: a malformed client certificate/key pair must surface an error
// rather than silently building an unauthenticated (no client cert) rotate client.
func TestSessionSource_Rotate_MalformedClientCertificate(t *testing.T) {
	store := newFakeStore()
	src := NewSessionSource(testOldCookie, "https://example.invalid", &TLS{
		CertData: []byte("not a certificate"),
		KeyData:  []byte("not a key"),
	}, store, testAccount)

	_, err := src.Rotate(t.Context(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client certificate")
	assert.Equal(t, 0, store.calls())
}

// erroringTransport always fails the RoundTrip with err and a nil response, simulating a
// transport-level failure (connection refused/reset) as opposed to an HTTP-level 401.
type erroringTransport struct {
	err error
}

func (e *erroringTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// TestRotatingRoundTripper_NextTransportErrorNoResponse covers the nil-response short-circuit: when
// the wrapped transport itself fails (no HTTP response at all), rotatingRoundTripper must return
// that error immediately without dereferencing the nil response or attempting a rotation.
func TestRotatingRoundTripper_NextTransportErrorNoResponse(t *testing.T) {
	rotateServer, rotateCalls := newRotateServer(t, http.StatusOK)

	store := newFakeStore()
	src := NewSessionSource(testOldCookie, rotateServer.URL, nil, store, testAccount)

	wantErr := errors.New("connection refused")
	next := &erroringTransport{err: wantErr}
	rt := &rotatingRoundTripper{src: src, next: next}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://target.invalid/resource", nil)
	require.NoError(t, err)

	resp, rerr := rt.RoundTrip(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	assert.Nil(t, resp)
	require.ErrorIs(t, rerr, wantErr)
	assert.Equal(t, int32(0), rotateCalls.Load(), "no rotation should be attempted when the underlying transport itself fails")
}
