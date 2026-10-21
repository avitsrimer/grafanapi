package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"

	"github.com/grafana/grafanapi/internal/keychain"
)

// Binding states reported by ResolveContextSessionCookie (see BindingState).
const (
	// BindingNone means nothing was loaded: no stored item, or the context has no server.
	BindingNone BindingState = iota
	// BindingBound means the stored item was bound to the context's server and loaded.
	BindingBound
	// BindingMigrated means a legacy item was bound to the context's server just now and loaded.
	BindingMigrated
)

// ResolveSessionCookie returns an Override that populates the current context's session cookie
// from store (see ResolveContextSessionCookie). It is a no-op if there is no current context, or
// if the current context has no Grafana config.
//
// Callers that load a single (current) context — Options.LoadConfig/LoadRESTConfig — use this
// Override. Callers that iterate every context — "config check" — call
// ResolveContextSessionCookie directly for each one instead, since an Override only ever sees the
// current context.
//
// The Override runs before the validator, so it must not hide the server-URL diagnostics Validate
// would give: an empty server loads nothing and returns no error (Validate reports "server is
// required"), and a server ServerOrigin rejects loads nothing and returns an error wrapping that
// *ServerURLError (errors.Is(err, ErrInvalidServerURL)), which the CLI renders with its
// suggestions. A cleartext remote server passes ServerOrigin and is rejected by Validate.
func ResolveSessionCookie(store keychain.Store, opts ResolveOptions) Override {
	return func(cfg *Config) error {
		if cfg.CurrentContext == "" || !cfg.HasContext(cfg.CurrentContext) {
			return nil
		}

		_, err := ResolveContextSessionCookie(store, cfg.Contexts[cfg.CurrentContext], opts)

		return err
	}
}

// ResolveContextSessionCookie populates gCtx.Grafana.SessionCookie and gCtx.Grafana.Session from
// store, keyed by keychain.Account(gCtx.Name), but only for the server the stored session is bound
// to. A keychain.ErrNotFound result leaves both nil/empty rather than failing: the context may
// simply not have completed `grafanapi login` yet. Any other store error is returned as-is. The
// SessionSource is only constructed when a cookie was actually loaded, so unauthenticated contexts
// and login flows never get one (see session_source.go).
//
// This is the single choke point every transport relies on: a transport only sends a cookie when
// SessionCookie or Session is set, so refusing to set them here closes every path at once. The
// stored item is a keychain credential payload (see keychain.DecodeCredential). In order:
//
//   - a malformed payload is an error that never carries the raw item;
//   - an empty server loads nothing and is not an error (Validate reports it);
//   - a server ServerOrigin rejects loads nothing and returns an error wrapping ServerOrigin's;
//   - a bound item whose origin is not exactly ServerOrigin(server) is a mismatch
//     (*SessionBindingError, ErrSessionOriginMismatch). The comparison is plain string equality: a
//     stored origin that is not in ServerOrigin's canonical form never matches and fails closed;
//   - a legacy (origin-less) item is refused as unbound (ErrSessionUnbound) unless
//     opts.AllowLegacyBind, in which case it is bound to the server on the spot (best-effort write
//     back, notice on opts.Notice) and loaded, reported as BindingMigrated.
//
// On every error nothing is populated, so no cookie can be sent.
func ResolveContextSessionCookie(store keychain.Store, gCtx *Context, opts ResolveOptions) (SessionBinding, error) {
	if gCtx == nil || gCtx.Grafana == nil {
		return SessionBinding{State: BindingNone}, nil
	}

	account := keychain.Account(gCtx.Name)

	raw, err := store.Get(account)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return SessionBinding{State: BindingNone}, nil
		}

		return SessionBinding{}, err
	}

	cred, err := keychain.DecodeCredential(raw)
	if err != nil {
		return SessionBinding{}, fmt.Errorf("session for context %q: %w", gCtx.Name, err)
	}

	server := gCtx.Grafana.Server
	if server == "" {
		return SessionBinding{State: BindingNone}, nil
	}

	serverOrigin, err := ServerOrigin(server)
	if err != nil {
		return SessionBinding{}, fmt.Errorf("session for context %q: %w; the session cookie was not sent", gCtx.Name, err)
	}

	state := BindingBound

	switch {
	case !cred.Legacy():
		if cred.Origin != serverOrigin {
			return SessionBinding{}, &SessionBindingError{
				Context:      gCtx.Name,
				BoundOrigin:  cred.Origin,
				ServerOrigin: serverOrigin,
			}
		}
	case !opts.AllowLegacyBind:
		return SessionBinding{}, &SessionBindingError{Context: gCtx.Name, ServerOrigin: serverOrigin}
	default:
		bindLegacySession(store, account, gCtx.Name, cred.Cookie, server, serverOrigin, opts.notice())

		state = BindingMigrated
	}

	gCtx.Grafana.SessionCookie = cred.Cookie
	gCtx.Grafana.Session = NewSessionSource(cred.Cookie, server, gCtx.Grafana.TLS, store, account)

	return SessionBinding{State: state, Origin: serverOrigin}, nil
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

// ResolveOptions tunes ResolveContextSessionCookie.
type ResolveOptions struct {
	// AllowLegacyBind permits binding a legacy (origin-less) item to the effective server on first
	// use. Callers set it from LegacyBindAllowed: it is only safe when the server provably comes
	// from the user's own configuration file.
	AllowLegacyBind bool
	// Notice receives the one-line notice of a legacy migration and the warning of a failed
	// write-back. Nil discards them. Neither ever carries the cookie.
	Notice io.Writer
}

// BindingState classifies what ResolveContextSessionCookie found.
type BindingState int

// SessionBinding is the outcome of a successful ResolveContextSessionCookie. Origin is the origin
// the loaded session is bound to, in ServerOrigin's format; empty for BindingNone.
type SessionBinding struct {
	State  BindingState
	Origin string
}

func (o ResolveOptions) notice() io.Writer {
	if o.Notice == nil {
		return io.Discard
	}

	return o.Notice
}

// bindLegacySession binds a legacy item's cookie to serverOrigin and writes it back. The write is
// best-effort: the cookie is loaded either way, and a failure only costs the next run the same
// migration. The messages name the context and the origin, never the cookie.
func bindLegacySession(store keychain.Store, account, name, cookie, server, serverOrigin string, notice io.Writer) {
	secret, err := EncodeSessionCredential(cookie, server)
	if err == nil {
		err = store.Set(account, secret)
	}

	if err != nil {
		fmt.Fprintf(notice, "warning: could not bind the stored session for context %q to %s: %v\n", name, serverOrigin, err)

		return
	}

	fmt.Fprintf(notice, "notice: bound the stored session for context %q to %s (first use since upgrading)\n", name, serverOrigin)
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
