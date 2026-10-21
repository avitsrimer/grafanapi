package config_test

import (
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/alerts"
	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/datasources"
	"github.com/grafana/grafanapi/cmd/grafanapi/explore"
	internalconfig "github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_AuthenticatedCommands_NeverSendCookieToAnotherServer drives the real cobra commands that
// load a stored session (explore, datasources, alerts) through every way an attacker can change
// the server the cookie would be sent to: GRAFANA_SERVER, a --config file, and a
// GRAFANAPI_CONFIG file exported by an .envrc. Each is tried against a session bound to the
// legitimate server and against a legacy (origin-less) item, and the attacker's server must
// receive no request at all, nor may the cookie show up in any output.
func Test_AuthenticatedCommands_NeverSendCookieToAnotherServer(t *testing.T) {
	commands := []struct {
		name string
		cmd  func() *cobra.Command
		args []string
	}{
		{"explore", explore.Command, []string{"prom-uid", "up"}},
		{"datasources", datasources.Command, nil},
		{"alerts rules list", alerts.Command, []string{"rules", "list"}},
	}

	vectors := []struct {
		name string
		// legacy seeds a bare-cookie item instead of one bound to the legitimate server.
		legacy bool
		// setup returns the command-line flags and environment for the vector.
		setup   func(t *testing.T, homeConfig, evilConfig, evilURL string) ([]string, map[string]string)
		wantErr error
	}{
		{
			name: "GRAFANA_SERVER override",
			setup: func(_ *testing.T, homeConfig, _, evilURL string) ([]string, map[string]string) {
				return []string{"--config", homeConfig}, map[string]string{"GRAFANA_SERVER": evilURL}
			},
			wantErr: internalconfig.ErrSessionOriginMismatch,
		},
		{
			name: "--config pointing at another server",
			setup: func(_ *testing.T, _, evilConfig, _ string) ([]string, map[string]string) {
				return []string{"--config", evilConfig}, nil
			},
			wantErr: internalconfig.ErrSessionOriginMismatch,
		},
		{
			name: "GRAFANAPI_CONFIG exported by an .envrc",
			setup: func(_ *testing.T, _, evilConfig, _ string) ([]string, map[string]string) {
				return nil, map[string]string{internalconfig.ConfigFileEnvVar: evilConfig}
			},
			wantErr: internalconfig.ErrSessionOriginMismatch,
		},
		{
			name:   "legacy item with --config",
			legacy: true,
			setup: func(_ *testing.T, _, evilConfig, _ string) ([]string, map[string]string) {
				return []string{"--config", evilConfig}, nil
			},
			wantErr: internalconfig.ErrSessionUnbound,
		},
		{
			name:   "legacy item with GRAFANA_SERVER",
			legacy: true,
			setup: func(_ *testing.T, homeConfig, _, evilURL string) ([]string, map[string]string) {
				return []string{"--config", homeConfig}, map[string]string{"GRAFANA_SERVER": evilURL}
			},
			wantErr: internalconfig.ErrSessionUnbound,
		},
		{
			name:   "legacy item with GRAFANAPI_CONFIG",
			legacy: true,
			setup: func(_ *testing.T, _, evilConfig, _ string) ([]string, map[string]string) {
				return nil, map[string]string{internalconfig.ConfigFileEnvVar: evilConfig}
			},
			wantErr: internalconfig.ErrSessionUnbound,
		},
	}

	for _, command := range commands {
		for _, vector := range vectors {
			t.Run(command.name+"/"+vector.name, func(t *testing.T) {
				home, _, _ := newRecordingServer(t)
				evil, requests, cookied := newRecordingServer(t)

				secret := testutils.BoundSecret(t, attackCookie, home.URL)
				if vector.legacy {
					secret = attackCookie
				}

				store := testutils.NewFakeKeychainStore()
				require.NoError(t, store.Set(keychain.Account("dev"), secret))
				t.Cleanup(config.SetKeychainStore(store))

				flags, env := vector.setup(t, serverConfig(t, home.URL), serverConfig(t, evil.URL), evil.URL)

				var result testutils.CommandResult

				testCase := testutils.CommandTestCase{
					Cmd:     command.cmd(),
					Command: append(append([]string{}, command.args...), flags...),
					Env:     env,
					Assertions: []testutils.CommandAssertion{
						func(_ *testing.T, res testutils.CommandResult) { result = res },
					},
				}
				testCase.Run(t)

				require.ErrorIs(t, result.Err, vector.wantErr)
				assert.NotContains(t, result.Err.Error(), attackCookie)
				assert.NotContains(t, result.Stdout, attackCookie)

				assert.Zero(t, requests.Load(), "the attacker's server must receive no request")
				assert.Zero(t, cookied.Load(), "no request may carry a Cookie header")

				raw, ok := store.RawValue(keychain.Account("dev"))
				require.True(t, ok)
				assert.Equal(t, secret, raw, "a refused session must be left as stored")
			})
		}
	}
}
