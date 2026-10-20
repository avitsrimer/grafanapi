package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/grafana/grafana-app-sdk/logging"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
	"github.com/grafana/grafanapi/internal/logs"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/grafana/grafanapi/internal/server/handlers"
	"github.com/grafana/grafanapi/internal/server/livereload"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = time.Minute
	writeTimeout      = 5 * time.Minute
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 5 * time.Second
)

type Config struct {
	ListenAddr string
	Port       int
	NoColor    bool
}

type Server struct {
	config           Config
	context          *config.Context
	resources        *resources.Resources
	resourceHandlers []handlers.ResourceHandler
	proxy            *httputil.ReverseProxy
	subpath          string
	timeouts         timeouts
	port             int
	token            string
}

// timeouts holds the http.Server limits; it is a field so a whitebox test can shrink them.
type timeouts struct {
	readHeader time.Duration
	read       time.Duration
	write      time.Duration
	idle       time.Duration
}

func New(config Config, context *config.Context, resources *resources.Resources) *Server {
	return &Server{
		config:    config,
		context:   context,
		resources: resources,
		token:     rand.Text(),
		timeouts: timeouts{
			readHeader: readHeaderTimeout,
			read:       readTimeout,
			write:      writeTimeout,
			idle:       idleTimeout,
		},
		resourceHandlers: []handlers.ResourceHandler{
			handlers.NewDashboardProxy(context, resources),
			handlers.NewFoldersProxy(resources),
		},
	}
}

// Listen binds the configured address and records the port actually bound (so port 0 works).
func (s *Server) Listen(ctx context.Context) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(s.config.ListenAddr, strconv.Itoa(s.config.Port)))
	if err != nil {
		return nil, err
	}

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}

	s.port = addr.Port

	return ln, nil
}

// URL returns the URL, including the per-run access token, the server can be browsed at. It is only
// meaningful after Listen.
func (s *Server) URL() string {
	return browseURL(s.config.ListenAddr, s.port, s.token)
}

// Serve serves HTTP on ln until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	h, err := s.handler(ctx)
	if err != nil {
		_ = ln.Close()
		return err
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: s.timeouts.readHeader,
		ReadTimeout:       s.timeouts.read,
		WriteTimeout:      s.timeouts.write,
		IdleTimeout:       s.timeouts.idle,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		_ = srv.Shutdown(shutdownCtx)
	}()

	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}

	return err
}

func (s *Server) handler(ctx context.Context) (http.Handler, error) {
	if s.port == 0 {
		return nil, errors.New("server is not listening: call Listen first")
	}

	assetsFS, err := fs.Sub(embedFS, "embed/assets")
	if err != nil {
		return nil, fmt.Errorf("could not create a sub-tree from the embedded assets FS: %w", err)
	}

	u, err := url.Parse(s.context.Grafana.Server)
	if err != nil {
		return nil, err
	}

	transport, err := httputils.NewTransport(s.context)
	if err != nil {
		return nil, fmt.Errorf("could not build the Grafana proxy transport: %w", err)
	}

	s.subpath = strings.TrimSuffix(u.Path, "/")
	grafanaOrigin := u.Scheme + "://" + u.Host // before Rewrite clears u.Path
	s.proxy = &httputil.ReverseProxy{
		// WrapWithSession injects the session cookie and rotates it on a 401 (see
		// internal/config/session_source.go). It wraps Transport, which runs AFTER Rewrite below,
		// and unconditionally overwrites the Cookie header on every request, so it always wins -
		// there is deliberately no cookie-setting left in Rewrite (internal/server/grafana/
		// requests.go's dashboard-proxy client applies the same reasoning).
		Transport: s.context.Grafana.WrapWithSession(transport),
		Rewrite: func(r *httputil.ProxyRequest) {
			u.Path = "" // to ensure possible sub-paths won't be added twice.
			r.SetURL(u)

			// The browser jar holds the serve token; it must never reach Grafana, and
			// WrapWithSession only supplies a Cookie when the context has a session.
			r.Out.Header.Del("Cookie")

			// The guard has already vetted any Origin as this server's own, which is the real CSRF
			// control: Grafana sees only the proxy (SetURL sends Grafana's Host), so the browser's
			// Origin could never match it. Forward Grafana's own origin instead of deleting the
			// header, so Grafana's check still runs; no Origin in, none out.
			if r.In.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", grafanaOrigin)
			}

			r.Out.Header.Set("User-Agent", httputils.UserAgent)
		},
		// Cookies are scoped by host, not port: a Grafana cookie stored for this host would be sent
		// to every other local service. The preview authenticates through the transport, not the jar.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")

			return nil
		},
	}

	hosts, err := allowedHosts(s.config.ListenAddr, s.port, net.InterfaceAddrs)
	if err != nil {
		return nil, err
	}

	r := chi.NewRouter()

	// The guard is the outermost middleware (chi refuses Use after a route), so it wraps every
	// route - proxy, mocks, local file handlers, websocket - and runs before the request logger.
	r.Use(newGuard(hosts, s.token, s.port, logging.FromContext(ctx)).middleware)

	// Log HTTP requests/responses at debug level
	r.Use(middleware.RequestLogger(&middleware.DefaultLogFormatter{
		Logger:  logs.DecorateAtLevel(logging.FromContext(ctx), slog.LevelInfo),
		NoColor: s.config.NoColor,
	}))
	// Inject the logger in HTTP requests context
	r.Use(func(h http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			//nolint:contextcheck
			r = r.WithContext(logging.Context(r.Context(), logging.FromContext(ctx)))
			h.ServeHTTP(w, r)
		}

		return http.HandlerFunc(fn)
	})

	s.applyStaticProxyConfig(r, s.staticProxyConfig())

	for _, handler := range s.resourceHandlers {
		logging.FromContext(ctx).Debug("registering proxy handler")

		for _, endpoint := range handler.Endpoints(s.proxy) {
			switch endpoint.Method {
			case http.MethodGet:
				r.Get(s.subpath+endpoint.URL, endpoint.Handler)
			case http.MethodPost:
				r.Post(s.subpath+endpoint.URL, endpoint.Handler)
			case http.MethodPut:
				r.Put(s.subpath+endpoint.URL, endpoint.Handler)
			default:
				return nil, fmt.Errorf("unknown endpoint method %s", endpoint.Method)
			}
		}

		s.applyStaticProxyConfig(r, handler.StaticEndpoints())
	}

	// Livereload setup. HandshakeTimeout must stay zero: gorilla then clears the connection
	// deadlines on upgrade, which is what lets the server's Read/WriteTimeout coexist with the
	// long-lived websocket.
	var upgrader = &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(_ *http.Request) bool { return true },
	}

	livereload.Initialize()
	r.Get("/livereload", livereload.Handler(upgrader))

	s.resources.OnChange(func(resource *resources.Resource) {
		logging.FromContext(ctx).Debug("Resource changed in memory", slog.String("component", "livereload"), slog.String("resource", string(resource.Ref())))
		livereload.ReloadResource(resource)
	})

	r.Get("/", s.rootHandler)
	r.Get("/grafanapi/{group}/{version}/{kind}/{name}", s.iframeHandler)
	r.Handle("/grafanapi/assets/*", http.StripPrefix("/grafanapi/assets/", http.FileServer(http.FS(assetsFS))))

	return r, nil
}

func (s *Server) applyStaticProxyConfig(r chi.Router, config handlers.StaticProxyConfig) {
	for _, pattern := range config.ProxyGet {
		r.Get(s.subpath+pattern, s.proxy.ServeHTTP)
	}
	for _, pattern := range config.ProxyPost {
		r.Post(s.subpath+pattern, s.proxy.ServeHTTP)
	}
	for pattern, response := range config.MockGet {
		r.Get(s.subpath+pattern, s.mockHandler(response))
	}
	for pattern, response := range config.MockPost {
		r.Post(s.subpath+pattern, s.mockHandler(response))
	}
}

func (s *Server) staticProxyConfig() handlers.StaticProxyConfig {
	return handlers.StaticProxyConfig{
		ProxyGet: []string{
			"/public/*",
			"/avatar/*",
		},
		MockGet: map[string]string{
			"/api/ma/events":       "[]",
			"/api/live/publish":    "[]",
			"/api/live/list":       "[]",
			"/api/user/orgs":       "[]",
			"/api/search":          "[]",
			"/api/usage/*":         "[]",
			"/api/frontend/assets": "{}",
			"/api/org/preferences": "{}",
			"/api/org/users":       "[]",

			"/api/prometheus/grafana/api/v1/rules": `{
      "status": "success",
      "data": { "groups": [] }
    }`,
			"/api/folders": "[]",
			"/api/recording-rules/writer": `{
      "id": "cojWep7Vz",
      "data_source_uid": "grafanacloud-prom",
      "remote_write_path": "/api/prom/push"
    }`,

			"/apis/banners.grafana.app/v0alpha1/namespaces/{stack}/announcement-banners": `{
      "kind": "AnnouncementBannerList",
      "apiVersion": "banners.grafana.app/v0alpha1",
      "metadata": {"resourceVersion": "29"}
    }`,
		},
		MockPost: map[string]string{
			"/api/frontend-metrics": "[]",
			"/api/search-v2":        "[]",
			"/api/live/publish":     "{}",
			"/api/ma/events":        "null",
		},
	}
}

func (s *Server) rootHandler(w http.ResponseWriter, r *http.Request) {
	templateVars := map[string]any{
		"CurrentContext": s.context,
		"Resources":      s.resources,
	}

	templates = templates.Funcs(template.FuncMap{
		"kindHasProxy": func(kind string) bool {
			for _, handler := range s.resourceHandlers {
				if handler.ResourceType().Kind == kind && handler.ProxyURL("foo") != "" {
					return true
				}
			}
			return false
		},
	})

	err := renderTemplate(w, "proxy/index.html.tmpl", templateVars)
	if err != nil {
		httputils.Error(r, w, "Error while executing template", err, http.StatusInternalServerError)
		return
	}
}

func (s *Server) mockHandler(response string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		httputils.Write(r, w, []byte(response))
	}
}

func (s *Server) iframeHandler(w http.ResponseWriter, r *http.Request) {
	group := chi.URLParam(r, "group")
	version := chi.URLParam(r, "version")
	kind := chi.URLParam(r, "kind")
	name := chi.URLParam(r, "name")

	var handler handlers.ResourceHandler
	for _, candidate := range s.resourceHandlers {
		candidateResourceType := candidate.ResourceType()
		groupVersion := candidateResourceType.GroupVersion

		if candidateResourceType.Kind == kind &&
			groupVersion.Group == group &&
			(groupVersion.Version == version || groupVersion.Version == "") {
			handler = candidate
			break
		}
	}

	msg := fmt.Sprintf("Could not find handler for group=%s, version=%s, kind=%s", group, version, kind)
	if handler == nil {
		httputils.Error(r, w, msg, errors.New(msg), http.StatusInternalServerError)
		return
	}

	templateVars := map[string]any{
		"IframeURL":      s.subpath + handler.ProxyURL(name),
		"CurrentContext": s.context,
		"Port":           s.config.Port,
	}

	if err := renderTemplate(w, "proxy/iframe.html.tmpl", templateVars); err != nil {
		httputils.Error(r, w, "Error while executing template", err, http.StatusInternalServerError)
		return
	}
}

// browseURL is the URL a browser should open: wildcard binds are reached through loopback.
func browseURL(listenAddr string, port int, token string) string {
	host := listenAddr
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/"}
	if token != "" {
		u.RawQuery = url.Values{"token": {token}}.Encode()
	}

	return u.String()
}
