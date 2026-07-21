package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrg/xdg"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

func TestLoad_explicitFile(t *testing.T) {
	req := require.New(t)

	cfg, err := config.Load(t.Context(), config.ExplicitConfigFile("./testdata/config.yaml"))
	req.NoError(err)

	req.Equal("local", cfg.CurrentContext)
	req.Len(cfg.Contexts, 1)
	req.Equal("local", cfg.Contexts["local"].Name)
	req.Equal("http://localhost:3000/", cfg.Contexts["local"].Grafana.Server)
}

func TestLoad_explicitFile_notFound(t *testing.T) {
	req := require.New(t)

	_, err := config.Load(t.Context(), config.ExplicitConfigFile("./testdata/does-not-exist.yaml"))
	req.Error(err)
	req.ErrorIs(err, os.ErrNotExist)
}

func TestLoad_standardLocation_noExistingConfig(t *testing.T) {
	req := require.New(t)

	fakeConfigDir := t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", fakeConfigDir)

	// make sure the xdg library uses the new-fake env var we just set
	xdg.Reload()

	cfg, err := config.Load(t.Context(), config.StandardLocation())
	req.NoError(err)

	// An empty configuration is returned
	req.Equal("default", cfg.CurrentContext)
	req.Len(cfg.Contexts, 1)
}

func TestLoad_standardLocation_withExistingConfig(t *testing.T) {
	req := require.New(t)

	fakeConfigDir := t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", fakeConfigDir)

	// create a barebones config file at the standard location
	err := os.MkdirAll(filepath.Join(fakeConfigDir, config.StandardConfigFolder), 0777)
	req.NoError(err)

	err = os.WriteFile(
		filepath.Join(fakeConfigDir, config.StandardConfigFolder, config.StandardConfigFileName),
		[]byte(`current-context: local`),
		0600,
	)
	req.NoError(err)

	// make sure the xdg library uses the new-fake env var we just set
	xdg.Reload()

	cfg, err := config.Load(t.Context(), config.StandardLocation())
	req.NoError(err)

	req.Equal("local", cfg.CurrentContext)
	req.Empty(cfg.Contexts)
}

func TestLoad_standardLocation_withEnvVar(t *testing.T) {
	req := require.New(t)

	// Set the environment variable to point to a test config
	t.Setenv(config.ConfigFileEnvVar, "./testdata/config.yaml")

	cfg, err := config.Load(t.Context(), config.StandardLocation())
	req.NoError(err)

	req.Equal("local", cfg.CurrentContext)
	req.Len(cfg.Contexts, 1)
	req.Equal("local", cfg.Contexts["local"].Name)
	req.Equal("http://localhost:3000/", cfg.Contexts["local"].Grafana.Server)
}

func TestLoad_standardLocation_envVarTakesPrecedence(t *testing.T) {
	req := require.New(t)

	fakeConfigDir := t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", fakeConfigDir)

	// create a config file at the standard location with different content
	err := os.MkdirAll(filepath.Join(fakeConfigDir, config.StandardConfigFolder), 0777)
	req.NoError(err)

	err = os.WriteFile(
		filepath.Join(fakeConfigDir, config.StandardConfigFolder, config.StandardConfigFileName),
		[]byte(`current-context: standard-location`),
		0600,
	)
	req.NoError(err)

	// Set the environment variable to point to a different config
	t.Setenv(config.ConfigFileEnvVar, "./testdata/config.yaml")

	// make sure the xdg library uses the new-fake env var we just set
	xdg.Reload()

	cfg, err := config.Load(t.Context(), config.StandardLocation())
	req.NoError(err)

	// Should load from env var, not standard location
	req.Equal("local", cfg.CurrentContext)
	req.Len(cfg.Contexts, 1)
	req.Equal("http://localhost:3000/", cfg.Contexts["local"].Grafana.Server)
}

func TestLoad_withOverride(t *testing.T) {
	req := require.New(t)

	cfg, err := config.Load(t.Context(), config.ExplicitConfigFile("./testdata/config.yaml"), func(cfg *config.Config) error {
		cfg.CurrentContext = "overridden"
		return nil
	})
	req.NoError(err)

	req.Equal("overridden", cfg.CurrentContext)
	req.Len(cfg.Contexts, 1)
	req.Equal("http://localhost:3000/", cfg.Contexts["local"].Grafana.Server)
}

func TestLoad_withInvalidYaml(t *testing.T) {
	req := require.New(t)

	cfg := `current-context: local
this-field-is-invalid: []`

	configFile := testutils.CreateTempFile(t, cfg)

	_, err := config.Load(t.Context(), config.ExplicitConfigFile(configFile))
	req.Error(err)
	req.ErrorAs(err, &config.UnmarshalError{})
	req.ErrorContains(err, "unknown field \"this-field-is-invalid\"")
}

// TestLoad_DoesNotLeakSecretsOnError is a regression test ensuring that
// sensitive values (fields tagged datapolicy:"secret" such as token, password,
// and tls.key-data) do not appear in error output produced by
// config.Load + fail.ErrorToDetailedError.
//
// See docs/specs/bugfix-prevent-token-leak for the full specification.
func TestLoad_DoesNotLeakSecretsOnError(t *testing.T) {
	// validationOverride mimics the validator used by the real CLI (LoadConfig).
	validationOverride := func(cfg *config.Config) error {
		if !cfg.HasContext(cfg.CurrentContext) {
			return config.ContextNotFound(cfg.CurrentContext)
		}
		return cfg.GetCurrentContext().Validate()
	}

	tests := []struct {
		name    string
		fixture string
		// overrides are optional; pass validationOverride to trigger validation.
		overrides []config.Override
		wantErr   bool
		// checkRendered asserts properties of the full DetailedError.Error() output.
		// Used for UnmarshalError (parse-error) cases.
		checkRendered func(t *testing.T, rendered string)
		// checkValidation asserts properties of the ValidationError directly.
		// Used when a ValidationError is expected.
		checkValidation func(t *testing.T, err error)
		// checkSuccess asserts properties of the successfully-loaded Config.
		checkSuccess func(t *testing.T, cfg config.Config)
	}{
		// NOTE(Task 3): the "bad-token-separator" (AC-1/AC-13) and "bad-password-indent"
		// (AC-2) cases that used to live here tested redaction of the `token`/`password`
		// keys. Those fields (and User) were removed from GrafanaConfig in favor of the
		// session-cookie auth mechanism, so they are no longer part of the
		// datapolicy:"secret" denylist and can no longer be meaningfully tested for
		// redaction. Task 9 (config/test fixture & redaction cleanup) deletes
		// testdata/bad-token-separator.yaml and testdata/bad-password-indent.yaml and
		// repurposes them into a non-secret parse-error fixture.
		{
			// AC-3: parse error near a tls.key-data block scalar containing a PEM body.
			// The key-data field is tagged datapolicy:"secret", so T1 redacts the block.
			name:    "bad-tls-key-data-block",
			fixture: "./testdata/bad-tls-key-data-block.yaml",
			wantErr: true,
			checkRendered: func(t *testing.T, rendered string) {
				t.Helper()
				// AC-3: no PEM body line must appear in rendered error
				require.NotContains(t, rendered, "-----BEGIN EC PRIVATE KEY-----",
					"PEM body line must not leak in rendered error (AC-3)")
				// AC-3: key name "key-data" must be present in surrounding context
				require.Contains(t, rendered, "key-data",
					"key name must remain visible in rendered error (AC-3)")
			},
		},
		{
			// AC-4: config that parses but fails validation (missing server); the
			// annotated source must show the path context.
			// NOTE(Task 3): this fixture used to also carry a `token:` secret to prove
			// AnnotatedSource redacts it; token/password/user were removed from
			// GrafanaConfig, so the only remaining datapolicy:"secret" field is TLS
			// key-data, which is covered separately by "valid-config" (AC-7/AC-12) and
			// "bad-tls-key-data-block" (AC-3).
			name:      "validation-error",
			fixture:   "./testdata/validation-error.yaml",
			overrides: []config.Override{validationOverride},
			wantErr:   true,
			checkValidation: func(t *testing.T, err error) {
				t.Helper()
				req := require.New(t)
				var validationErr config.ValidationError
				req.ErrorAs(err, &validationErr,
					"error must be a ValidationError")
				// AC-4: AnnotatedSource must be non-empty (annotation was produced)
				req.NotEmpty(validationErr.AnnotatedSource,
					"AnnotatedSource must contain some context (AC-4)")
			},
		},
		{
			// AC-7, AC-12: a well-formed config file must load cleanly and the real
			// secret value must be available byte-for-byte in the returned Config
			// (the redacted copy never reaches the Config struct).
			name:    "valid-config",
			fixture: "./testdata/valid-config.yaml",
			wantErr: false,
			checkSuccess: func(t *testing.T, cfg config.Config) {
				t.Helper()
				req := require.New(t)
				ctx, ok := cfg.Contexts["test"]
				req.True(ok, "context 'test' must exist")
				req.NotNil(ctx, "context must not be nil")
				req.NotNil(ctx.Grafana, "grafana config must not be nil")
				req.Equal("https://grafana.example.com", ctx.Grafana.Server)
				// Real secret must survive Load unmodified (AC-7, AC-12).
				// NOTE(Task 3): APIToken/User/Password were removed from GrafanaConfig in
				// favor of the session-cookie auth mechanism; this fixture now exercises
				// the one remaining datapolicy:"secret" field (TLS key-data) instead of
				// the removed token field.
				req.NotNil(ctx.Grafana.TLS, "tls config must not be nil")
				req.Equal([]byte("glc_real_runtime_secret1"), ctx.Grafana.TLS.KeyData,
					"KeyData must equal the literal value from the fixture (AC-7, AC-12)")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(t.Context(), config.ExplicitConfigFile(tc.fixture), tc.overrides...)

			if !tc.wantErr {
				require.NoError(t, err)
				if tc.checkSuccess != nil {
					tc.checkSuccess(t, cfg)
				}
				return
			}

			require.Error(t, err)

			if tc.checkRendered != nil {
				// Render through the same pipeline that the CLI uses so that the
				// test covers the full error-rendering path including yaml.FormatError.
				rendered := fail.ErrorToDetailedError(err).Error()
				tc.checkRendered(t, rendered)
			}

			if tc.checkValidation != nil {
				tc.checkValidation(t, err)
			}
		})
	}
}

func TestWrite(t *testing.T) {
	req := require.New(t)

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	cfg := config.Config{
		CurrentContext: "local",
	}

	err := config.Write(t.Context(), config.ExplicitConfigFile(configFile), cfg)
	req.NoError(err)

	req.FileExists(configFile)
}
