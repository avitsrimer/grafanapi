package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

const (
	// DefaultContextName is the name of the default context.
	DefaultContextName = "default"
)

// Config holds the information needed to connect to remote Grafana instances.
type Config struct {
	// Source contains the path to the config file parsed to populate this struct.
	Source string `json:"-" yaml:"-"`

	// Contexts is a map of context configurations, indexed by name.
	Contexts map[string]*Context `json:"contexts" yaml:"contexts"`

	// CurrentContext is the name of the context currently in use.
	CurrentContext string `json:"current-context" yaml:"current-context"`
}

func (config *Config) HasContext(name string) bool {
	return config.Contexts[name] != nil
}

// GetCurrentContext returns the current context.
// If the current context is not set, it returns an error.
func (config *Config) GetCurrentContext() *Context {
	return config.Contexts[config.CurrentContext]
}

// SetContext adds a new context to the Grafana config.
// If a context with the same name already exists, it is overwritten.
func (config *Config) SetContext(name string, makeCurrent bool, context Context) {
	if config.Contexts == nil {
		config.Contexts = make(map[string]*Context)
	}

	config.Contexts[name] = &context

	if makeCurrent {
		config.CurrentContext = name
	}
}

// Context holds the information required to connect to a remote Grafana instance.
type Context struct {
	Name string `json:"-" yaml:"-"`

	Grafana *GrafanaConfig `json:"grafana,omitempty" yaml:"grafana,omitempty"`
}

func (context *Context) Validate() error {
	if context.Grafana == nil || context.Grafana.IsEmpty() {
		return ValidationError{
			Path:    fmt.Sprintf("$.contexts.'%s'", context.Name),
			Message: "grafana config is required",
		}
	}

	return context.Grafana.Validate(context.Name)
}

// ToRESTConfig returns a REST config for the context.
func (context *Context) ToRESTConfig(ctx context.Context) NamespacedRESTConfig {
	return NewNamespacedRESTConfig(ctx, *context)
}

type GrafanaConfig struct {
	// Server is the address of the Grafana server (https://hostname:port/path).
	// Required.
	Server string `env:"GRAFANA_SERVER" json:"server,omitempty" yaml:"server,omitempty"`

	// OrgID specifies the organization targeted by this config.
	// Note: required when targeting an on-prem Grafana instance.
	// See StackID for Grafana Cloud instances.
	OrgID int64 `env:"GRAFANA_ORG_ID" json:"org-id,omitempty" yaml:"org-id,omitempty"`

	// StackID specifies the Grafana Cloud stack targeted by this config.
	// Note: required when targeting a Grafana Cloud instance.
	// See OrgID for on-prem Grafana instances.
	StackID int64 `env:"GRAFANA_STACK_ID" json:"stack-id,omitempty" yaml:"stack-id,omitempty"`

	// TLS contains TLS-related configuration settings.
	TLS *TLS `json:"tls,omitempty" yaml:"tls,omitempty"`

	// SessionCookie holds the resolved Grafana session cookie (see
	// internal/config.CookieHeaderValue) for the current context. It is populated at config-load
	// time from the platform Keychain (internal/keychain) and is never serialized to disk: no env
	// tag (the cookie must never be a flag or env var) and json/yaml "-" tags.
	SessionCookie string `json:"-" yaml:"-"`

	// Session holds the shared, mutable SessionSource that rotates SessionCookie on a 401 and
	// re-persists the fresh value to the Keychain. It is populated alongside SessionCookie during
	// credential resolution (only when a cookie was actually loaded from the Keychain) and is
	// never serialized to disk: no env tag, json/yaml "-" tags. Like SessionCookie, it must be
	// zeroed in IsEmpty() so a resolved source never affects emptiness.
	Session *SessionSource `json:"-" yaml:"-"`
}

func (grafana GrafanaConfig) validateNamespace(contextName string) error {
	if grafana.OrgID != 0 {
		return nil
	}

	discoveredStackID, discoveryErr := DiscoverStackID(context.Background(), grafana)

	if grafana.StackID == 0 {
		if discoveryErr != nil {
			return ValidationError{
				Path:    fmt.Sprintf("$.contexts.'%s'.grafana", contextName),
				Message: fmt.Sprintf("missing contexts.%[1]s.org-id or contexts.%[1]s.stack-id", contextName),
				Suggestions: []string{
					"Specify the Grafana Org ID for on-prem Grafana",
					"Specify the Grafana Cloud Stack ID for Grafana Cloud",
				},
			}
		}

		return nil
	}

	// If discovery failed but grafana.StackID is set, we proceed with the configured StackID
	//nolint:nilerr // We intentionally ignore the error when StackID is configured
	if discoveryErr != nil {
		return nil
	}

	if discoveredStackID != grafana.StackID {
		return ValidationError{
			Path:    fmt.Sprintf("$.contexts.'%s'.grafana", contextName),
			Message: fmt.Sprintf("mismatched contexts.%[1]s.stack-id, discovered %d - was %d in config", contextName, discoveredStackID, grafana.StackID),
			Suggestions: []string{
				"Specify the correct Grafana Cloud Stack ID for Grafana Cloud or omit the stack-id param",
			},
		}
	}

	return nil
}

func (grafana GrafanaConfig) Validate(contextName string) error {
	if grafana.Server == "" {
		return ValidationError{
			Path:    fmt.Sprintf("$.contexts.'%s'.grafana", contextName),
			Message: "server is required",
			Suggestions: []string{
				"Set the address of the Grafana server to connect to",
			},
		}
	}

	if err := grafana.validateNamespace(contextName); err != nil {
		return err
	}

	return nil
}

func (grafana GrafanaConfig) IsEmpty() bool {
	// SessionCookie is populated from the Keychain independently of the file contents (see
	// ResolveSessionCookie / ResolveContextSessionCookie), so it must not affect emptiness: a
	// stale/orphaned Keychain entry for an otherwise-empty "grafana: {}" block must still report
	// IsEmpty() == true, so Context.Validate() surfaces "grafana config is required" rather than
	// the more confusing "server is required".
	grafana.SessionCookie = ""
	grafana.Session = nil

	return grafana == GrafanaConfig{}
}

// TLS contains settings to enable transport layer security.
type TLS struct {
	// InsecureSkipTLSVerify disables the validation of the server's SSL certificate.
	// Enabling this will make your HTTPS connections insecure.
	Insecure bool `json:"insecure-skip-verify,omitempty" yaml:"insecure-skip-verify,omitempty"`

	// ServerName is passed to the server for SNI and is used in the client to check server
	// certificates against. If ServerName is empty, the hostname used to contact the
	// server is used.
	ServerName string `json:"server-name,omitempty" yaml:"server-name,omitempty"`

	// CertData holds PEM-encoded bytes (typically read from a client certificate file).
	// Note: this value is base64-encoded in the config file and will be
	// automatically decoded.
	CertData []byte `json:"cert-data,omitempty" yaml:"cert-data,omitempty"`
	// KeyData holds PEM-encoded bytes (typically read from a client certificate key file).
	// Note: this value is base64-encoded in the config file and will be
	// automatically decoded.
	KeyData []byte `datapolicy:"secret" json:"key-data,omitempty" yaml:"key-data,omitempty"`
	// CAData holds PEM-encoded bytes (typically read from a root certificates bundle).
	// Note: this value is base64-encoded in the config file and will be
	// automatically decoded.
	CAData []byte `json:"ca-data,omitempty" yaml:"ca-data,omitempty"`

	// NextProtos is a list of supported application level protocols, in order of preference.
	// Used to populate tls.Config.NextProtos.
	// To indicate to the server http/1.1 is preferred over http/2, set to ["http/1.1", "h2"] (though the server is free to ignore that preference).
	// To use only http/1.1, set to ["http/1.1"].
	NextProtos []string `json:"next-protos,omitempty" yaml:"next-protos,omitempty"`
}

// ToStdTLSConfig builds a "crypto/tls".Config from cfg, including the full TLS material: a root
// CA pool from CAData (when set) and a client certificate from CertData/KeyData (when both are
// set), in addition to Insecure/ServerName/NextProtos. Every direct HTTP client built from a
// GrafanaConfig's TLS settings (the session-rotation client, the bootdata discovery client) must
// use this helper so an mTLS/custom-CA context authenticates identically on every transport path.
func (cfg *TLS) ToStdTLSConfig() *tls.Config {
	tlsConfig := &tls.Config{
		//nolint:gosec
		InsecureSkipVerify: cfg.Insecure,
		ServerName:         cfg.ServerName,
		NextProtos:         cfg.NextProtos,
	}

	if len(cfg.CAData) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(cfg.CAData) {
			tlsConfig.RootCAs = pool
		}
	}

	if len(cfg.CertData) > 0 && len(cfg.KeyData) > 0 {
		if cert, err := tls.X509KeyPair(cfg.CertData, cfg.KeyData); err == nil {
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}

	return tlsConfig
}

// Minify returns a trimmed down version of the given configuration containing
// only the current context and the relevant options it directly depends on.
func Minify(config Config) (Config, error) {
	minified := config

	if config.CurrentContext == "" {
		return Config{}, errors.New("current-context must be defined in order to minify")
	}

	minified.Contexts = make(map[string]*Context, 1)
	for name, ctx := range config.Contexts {
		if name == minified.CurrentContext {
			minified.Contexts[name] = ctx
		}
	}

	return minified, nil
}
