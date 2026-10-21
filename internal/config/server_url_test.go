package config_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerOrigin(t *testing.T) {
	t.Parallel()

	golden := []struct {
		input string
		want  string
	}{
		{"https://grafana.example.com", "https://grafana.example.com:443"},
		{"https://Grafana.Example.com/sub/", "https://grafana.example.com:443"},
		{"HTTPS://grafana.example.com:443/", "https://grafana.example.com:443"},
		{"https://grafana.example.com:0443", "https://grafana.example.com:443"},
		{"https://grafana.example.com.", "https://grafana.example.com:443"},
		{"https://grafana.example.com:", "https://grafana.example.com:443"},
		{"https://user:pw@grafana.example.com:3000/grafana/?x=1#f", "https://grafana.example.com:3000"},
		{"http://grafana.example.com", "http://grafana.example.com:80"},
		{"http://grafana.example.com:443", "http://grafana.example.com:443"},
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000"},
		{"http://[::1]:3000", "http://[::1]:3000"},
		{"https://[::1]", "https://[::1]:443"},
		{"https://[0:0:0:0:0:0:0:1]:8443", "https://[::1]:8443"},
		{"https://xn--bcher-kva.example", "https://xn--bcher-kva.example:443"},
	}

	for _, tc := range golden {
		t.Run("ok "+tc.input, func(t *testing.T) {
			t.Parallel()

			got, err := config.ServerOrigin(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("httptest URL is unchanged", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)

		got, err := config.ServerOrigin(srv.URL)
		require.NoError(t, err)
		assert.Equal(t, srv.URL, got)
	})

	failures := []string{
		"",
		"grafana.example.com",
		"ftp://x",
		"https://",
		"https:grafana.example.com",
		"https://x:abc",
		"https://x:0",
		"https://x:70000",
		"https://x:99999999999999999999",
		"https://bücher.example",
		"://bad",
		"https://.",
		" https://grafana.example.com",
	}

	for _, input := range failures {
		t.Run("error "+input, func(t *testing.T) {
			t.Parallel()

			got, err := config.ServerOrigin(input)
			require.Error(t, err)
			assert.Empty(t, got)
			require.ErrorIs(t, err, config.ErrInvalidServerURL)

			var urlErr *config.ServerURLError
			require.ErrorAs(t, err, &urlErr)
			assert.NotEmpty(t, urlErr.Reason)
		})
	}

	t.Run("errors never echo the input", func(t *testing.T) {
		t.Parallel()

		for _, input := range []string{
			"https://user:secret@myhost.test:abc",
			"ftp://user:secret@myhost.test",
			"https://user:secret@bücher.example",
			"https://user:secret@myhost.test:70000",
		} {
			_, err := config.ServerOrigin(input)
			require.Error(t, err, input)
			assert.NotContains(t, err.Error(), "secret", input)
			assert.NotContains(t, err.Error(), "myhost.test", input)
			assert.NotContains(t, err.Error(), "bücher", input)
		}
	})
}

func TestValidateServerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  string
		wantErr bool
	}{
		{"https", "https://grafana.example.com", false},
		{"https trailing slash", "https://grafana.example.com/", false},
		{"https sub path", "https://grafana.example.com/sub/path", false},
		{"https uppercase", "HTTPS://GRAFANA.EXAMPLE.COM", false},
		{"localhost", "http://localhost:3000", false},
		{"localhost uppercase", "http://LOCALHOST:3000", false},
		{"ipv4 loopback", "http://127.0.0.1:3000", false},
		{"ipv4 loopback range", "http://127.1.2.3", false},
		{"ipv6 loopback", "http://[::1]:3000", false},
		{"uppercase scheme loopback", "HTTP://127.0.0.1", false},
		{"http remote", "http://grafana.example.com", true},
		{"http remote uppercase", "HTTP://grafana.example.com", true},
		{"http non-loopback ipv6", "http://[::2]", true},
		{"http unspecified address", "http://0.0.0.0:3000", true},
		{"http docker service name", "http://grafana:3000", true},
		{"http localhost lookalike", "http://localhost.example.com", true},
		{"userinfo", "https://user:secret@grafana.example.com", true},
		{"non-ascii host", "https://bücher.example", true},
		{"no scheme", "grafana.example.com", true},
		{"no host", "https://", true},
		{"ftp", "ftp://grafana.example.com", true},
		{"unparsable", "://bad", true},
		{"empty", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := config.ValidateServerURL(tc.server)
			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.ErrorIs(t, err, config.ErrInvalidServerURL)
			assert.NotContains(t, err.Error(), "secret")
		})
	}

	t.Run("cleartext message names https and the loopback exception", func(t *testing.T) {
		t.Parallel()

		err := config.ValidateServerURL("http://grafana.example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "https")
		assert.Contains(t, err.Error(), "localhost")
	})

	t.Run("punycode message", func(t *testing.T) {
		t.Parallel()

		err := config.ValidateServerURL("https://bücher.example")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "punycode")
	})

	t.Run("credentials message", func(t *testing.T) {
		t.Parallel()

		err := config.ValidateServerURL("https://user:secret@grafana.example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "credentials")
	})
}

func TestSetSessionCookie(t *testing.T) {
	t.Parallel()

	newRequest := func(t *testing.T, target string) *http.Request {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		require.NoError(t, err)

		return req
	}

	t.Run("same origin sets the cookie", func(t *testing.T) {
		t.Parallel()

		for _, target := range []string{
			"https://grafana.example.com/api/user",
			"https://GRAFANA.example.com/api/user",
			"https://grafana.example.com:443/api/user?x=1",
		} {
			req := newRequest(t, target)
			require.NoError(t, config.SetSessionCookie(req, "https://grafana.example.com", "abc"), target)
			assert.Equal(t, "grafana_session=abc", req.Header.Get("Cookie"), target)
		}
	})

	t.Run("nil header is allocated", func(t *testing.T) {
		t.Parallel()

		req := newRequest(t, "https://grafana.example.com/")
		req.Header = nil
		require.NoError(t, config.SetSessionCookie(req, "https://grafana.example.com", "abc"))
		assert.Equal(t, "grafana_session=abc", req.Header.Get("Cookie"))
	})

	t.Run("different origin is refused", func(t *testing.T) {
		t.Parallel()

		for _, target := range []string{
			"https://other.example/api/user",
			"https://grafana.example.com:8443/api/user",
			"http://grafana.example.com/api/user",
			"https://sub.grafana.example.com/api/user",
		} {
			req := newRequest(t, target)
			err := config.SetSessionCookie(req, "https://grafana.example.com", "abc")
			require.ErrorIs(t, err, config.ErrUnsafeRedirect, target)

			var unsafe *config.UnsafeRedirectError
			require.ErrorAs(t, err, &unsafe, target)
			assert.Equal(t, "https://grafana.example.com:443", unsafe.From, target)
			assert.Empty(t, req.Header.Get("Cookie"), target)
		}
	})

	t.Run("cleartext remote server is refused", func(t *testing.T) {
		t.Parallel()

		req := newRequest(t, "http://grafana.example.com/api/user")
		err := config.SetSessionCookie(req, "http://grafana.example.com", "abc")
		require.ErrorIs(t, err, config.ErrInvalidServerURL)
		assert.Empty(t, req.Header.Get("Cookie"))
	})

	t.Run("loopback http server is accepted", func(t *testing.T) {
		t.Parallel()

		req := newRequest(t, "http://127.0.0.1:3000/api/user")
		require.NoError(t, config.SetSessionCookie(req, "http://127.0.0.1:3000", "abc"))
		assert.Equal(t, "grafana_session=abc", req.Header.Get("Cookie"))
	})

	t.Run("cookie is never in the error", func(t *testing.T) {
		t.Parallel()

		req := newRequest(t, "https://other.example/api/user")
		err := config.SetSessionCookie(req, "https://grafana.example.com", "supersecretcookie")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "supersecretcookie")
	})
}

func TestSameOriginRedirects(t *testing.T) {
	t.Parallel()

	const server = "https://grafana.example.com"

	check := config.SameOriginRedirects(server)

	newRequest := func(t *testing.T, target string) *http.Request {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		require.NoError(t, err)

		return req
	}

	t.Run("same origin is followed", func(t *testing.T) {
		t.Parallel()

		via := []*http.Request{newRequest(t, server+"/a")}
		require.NoError(t, check(newRequest(t, server+"/b?x=1"), via))
		require.NoError(t, check(newRequest(t, "https://GRAFANA.example.com:443/b"), via))
	})

	refused := []struct {
		name   string
		target string
		to     string
	}{
		{"other host", "https://other.example/cb?token=s3cret", "https://other.example:443"},
		{"downgrade of the same host", "http://grafana.example.com/cb?token=s3cret", "http://grafana.example.com:80"},
		{"other port", "https://grafana.example.com:8443/cb?token=s3cret", "https://grafana.example.com:8443"},
	}

	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := check(newRequest(t, tc.target), []*http.Request{newRequest(t, server+"/a")})
			require.ErrorIs(t, err, config.ErrUnsafeRedirect)

			var unsafe *config.UnsafeRedirectError
			require.ErrorAs(t, err, &unsafe)
			assert.Equal(t, "https://grafana.example.com:443", unsafe.From)
			assert.Equal(t, tc.to, unsafe.To)
			assert.NotContains(t, err.Error(), "s3cret")
			assert.NotContains(t, err.Error(), "/cb")
		})
	}

	t.Run("unparsable target origin", func(t *testing.T) {
		t.Parallel()

		req := newRequest(t, server+"/a")
		req.URL.Scheme = "ftp"

		var unsafe *config.UnsafeRedirectError
		require.ErrorAs(t, check(req, nil), &unsafe)
		assert.Equal(t, "<invalid>", unsafe.To)
	})

	t.Run("hop cap", func(t *testing.T) {
		t.Parallel()

		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = newRequest(t, server+"/a")
		}

		err := check(newRequest(t, server+"/b"), via)
		require.Error(t, err)
		require.ErrorContains(t, err, "stopped after 10 redirects")

		require.NoError(t, check(newRequest(t, server+"/b"), via[:9]))
	})

	t.Run("invalid server", func(t *testing.T) {
		t.Parallel()

		err := config.SameOriginRedirects("not a url")(newRequest(t, server+"/b"), nil)
		require.ErrorIs(t, err, config.ErrInvalidServerURL)
		assert.NotContains(t, err.Error(), "not a url")
	})
}
