package config

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/grafana/grafanapi/internal/keychain"
)

// ResolveSessionCookie returns an Override that populates the current context's session cookie
// from store (see ResolveContextSessionCookie). It is a no-op if there is no current context, or
// if the current context has no Grafana config.
//
// Callers that load a single (current) context — Options.LoadConfig/LoadRESTConfig — use this
// Override. Callers that iterate every context — "config check" — call
// ResolveContextSessionCookie directly for each one instead, since an Override only ever sees the
// current context.
func ResolveSessionCookie(store keychain.Store) Override {
	return func(cfg *Config) error {
		if cfg.CurrentContext == "" || !cfg.HasContext(cfg.CurrentContext) {
			return nil
		}

		return ResolveContextSessionCookie(store, cfg.Contexts[cfg.CurrentContext])
	}
}

// ResolveContextSessionCookie populates gCtx.Grafana.SessionCookie and gCtx.Grafana.Session from
// store, keyed by keychain.Account(gCtx.Name). A keychain.ErrNotFound result leaves both nil/empty
// rather than failing: the context may simply not have completed `grafanapi login` yet. Any other
// store error is returned as-is. The SessionSource is only constructed when a cookie was actually
// loaded, so unauthenticated contexts and login flows never get one (see session_source.go).
//
// The stored item is a keychain credential payload (see keychain.DecodeCredential): a legacy
// bare-cookie item and a payload bound to an origin both load their cookie. A malformed payload is
// returned as an error that never carries the raw item.
func ResolveContextSessionCookie(store keychain.Store, gCtx *Context) error {
	if gCtx == nil || gCtx.Grafana == nil {
		return nil
	}

	raw, err := store.Get(keychain.Account(gCtx.Name))
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return nil
		}

		return err
	}

	cred, err := keychain.DecodeCredential(raw)
	if err != nil {
		return fmt.Errorf("session for context %q: %w", gCtx.Name, err)
	}

	cookie := cred.Cookie

	gCtx.Grafana.SessionCookie = cookie
	gCtx.Grafana.Session = NewSessionSource(cookie, gCtx.Grafana.Server, gCtx.Grafana.TLS, store, keychain.Account(gCtx.Name))

	return nil
}

// LegacyBindAllowed reports whether a legacy (origin-less) Keychain item may be bound to the
// context's effective server on first use. It is true only when that server provably comes from
// the user's own configuration file: no --config (explicitConfigFile), and none of the inputs that
// choose the file or the server are set — GRAFANAPI_CONFIG, XDG_CONFIG_HOME, XDG_CONFIG_DIRS and,
// when envOverridesApplied, GRAFANA_SERVER — and $HOME is the passwd home directory.
//
// adrg/xdg resolves the file as the first directory that exists or can be created among
// $XDG_CONFIG_HOME (absolute values only), ~/Library/Application Support and $XDG_CONFIG_DIRS, and
// on darwin the second always exists, so --config, GRAFANAPI_CONFIG, XDG_CONFIG_HOME and $HOME are
// the only inputs that move it; XDG_CONFIG_DIRS is checked as belt and braces. os/user.Current
// reads the passwd entry, not $HOME.
//
// GRAFANA_SERVER counts by presence, a deliberately conservative choice: caarlos0/env skips an
// empty value (env.go:499), so an empty one would not override the server. Callers that never
// apply environment overrides (session refresh, login update) pass envOverridesApplied=false.
func LegacyBindAllowed(explicitConfigFile string, envOverridesApplied bool) bool {
	return legacyBindAllowed(explicitConfigFile, envOverridesApplied, os.LookupEnv, passwdHome)
}

// EncodeSessionCredential binds cookie to ServerOrigin(server) and returns the Keychain payload
// to store for it. It is the one production helper that turns a cookie and a server into a bound
// item, so login, login update and the rotation re-persist all write the same origin format. The
// returned error wraps ServerOrigin's and never carries the raw server or the cookie.
func EncodeSessionCredential(cookie, server string) (string, error) {
	origin, err := ServerOrigin(server)
	if err != nil {
		return "", fmt.Errorf("bind session to server: %w", err)
	}

	secret, err := keychain.EncodeCredential(keychain.Credential{Cookie: cookie, Origin: origin})
	if err != nil {
		return "", fmt.Errorf("bind session to server: %w", err)
	}

	return secret, nil
}

func legacyBindAllowed(
	explicitConfigFile string,
	envOverridesApplied bool,
	lookupEnv func(string) (string, bool),
	passwdHomeDir func() (string, error),
) bool {
	if explicitConfigFile != "" {
		return false
	}

	nonEmpty := func(name string) bool {
		v, ok := lookupEnv(name)
		return ok && v != ""
	}

	if nonEmpty(ConfigFileEnvVar) || nonEmpty("XDG_CONFIG_HOME") || nonEmpty("XDG_CONFIG_DIRS") {
		return false
	}

	if envOverridesApplied {
		if _, set := lookupEnv("GRAFANA_SERVER"); set {
			return false
		}
	}

	home, ok := lookupEnv("HOME")
	if !ok || home == "" {
		return false
	}

	pwHome, err := passwdHomeDir()
	if err != nil || pwHome == "" {
		return false
	}

	return filepath.Clean(home) == filepath.Clean(pwHome)
}

func passwdHome() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}

	return u.HomeDir, nil
}
