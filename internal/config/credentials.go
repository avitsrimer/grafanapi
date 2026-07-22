package config

import (
	"errors"

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
func ResolveContextSessionCookie(store keychain.Store, gCtx *Context) error {
	if gCtx == nil || gCtx.Grafana == nil {
		return nil
	}

	cookie, err := store.Get(keychain.Account(gCtx.Name))
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return nil
		}

		return err
	}

	gCtx.Grafana.SessionCookie = cookie
	gCtx.Grafana.Session = NewSessionSource(cookie, gCtx.Grafana.Server, gCtx.Grafana.TLS, store, keychain.Account(gCtx.Name))

	return nil
}
