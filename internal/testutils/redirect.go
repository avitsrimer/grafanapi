package testutils

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// NewRedirectServer starts an httptest server that answers every request with a redirect of the
// given status code to location. The server is closed by tb.Cleanup.
func NewRedirectServer(tb testing.TB, location string, code int) *httptest.Server {
	tb.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, location, code)
	}))
	tb.Cleanup(server.Close)

	return server
}

// NewCookieTrap starts a CookieTrap that answers every request with 200 and an empty JSON
// object. The server is closed by tb.Cleanup.
func NewCookieTrap(tb testing.TB) *CookieTrap {
	tb.Helper()

	trap := &CookieTrap{}
	trap.Server = httptest.NewServer(http.HandlerFunc(trap.handle))
	tb.Cleanup(trap.Close)

	return trap
}

// CookieTrap is an httptest server standing in for a host that must never be contacted: it
// records how many requests reached it, the Cookie header and the body of the latest one. It is
// safe for concurrent use.
type CookieTrap struct {
	*httptest.Server

	mu     sync.Mutex
	count  int
	cookie string
	body   string
}

// Requests returns the number of requests the trap has received.
func (c *CookieTrap) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.count
}

// LastCookie returns the Cookie header of the latest request, or "" when there was none.
func (c *CookieTrap) LastCookie() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cookie
}

// LastBody returns the body of the latest request, or "" when there was none.
func (c *CookieTrap) LastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.body
}

func (c *CookieTrap) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	c.mu.Lock()
	c.count++
	c.cookie = r.Header.Get("Cookie")
	c.body = string(body)
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}
