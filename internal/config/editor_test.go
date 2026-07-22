package config_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/require"
)

func Test_SetValue(t *testing.T) {
	testCases := []struct {
		name           string
		input          config.Config
		path           string
		value          string
		expectedOutput config.Config
	}{
		{
			name:           "string",
			input:          config.Config{},
			path:           "current-context",
			value:          "ctx-name",
			expectedOutput: config.Config{CurrentContext: "ctx-name"},
		},
		{
			name:  "string in new context",
			input: config.Config{},
			path:  "contexts.new.grafana.server",
			value: "url",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"new": {
						Grafana: &config.GrafanaConfig{Server: "url"},
					},
				},
			},
		},
		{
			name: "string in existing context",
			input: config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{Server: "url"},
					},
				},
			},
			path:  "contexts.existing.grafana.server",
			value: "new-url",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{Server: "new-url"},
					},
				},
			},
		},
		{
			name:  "boolean in new context",
			input: config.Config{},
			path:  "contexts.new.grafana.tls.insecure-skip-verify",
			value: "true",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"new": {
						Grafana: &config.GrafanaConfig{TLS: &config.TLS{Insecure: true}},
					},
				},
			},
		},
		{
			name:  "bytes in new context",
			input: config.Config{},
			path:  "contexts.new.grafana.tls.cert-data",
			value: "foo bar baz",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"new": {
						Grafana: &config.GrafanaConfig{TLS: &config.TLS{CertData: []byte("foo bar baz")}},
					},
				},
			},
		},
		{
			name:  "int64 in new context",
			input: config.Config{},
			path:  "contexts.new.grafana.org-id",
			value: "1",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"new": {
						Grafana: &config.GrafanaConfig{OrgID: 1},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			req := require.New(t)

			err := config.SetValue(&testCase.input, testCase.path, testCase.value)
			req.NoError(err)

			req.Equal(testCase.expectedOutput, testCase.input)
		})
	}
}

func Test_SetValue_withUnknownField(t *testing.T) {
	req := require.New(t)
	input := config.Config{}

	err := config.SetValue(&input, "unknown", "value")
	req.Error(err)
}

// Test_SetValue_rejectsRemovedSecretFields ensures the decommissioned auth
// fields (User, Password, APIToken) can no longer be set through
// `grafanapi config set`: since editor.go is reflection/yaml-tag driven and
// GrafanaConfig no longer declares those fields, the lookup fails with the
// same "unable to locate path" error as any other unknown field.
func Test_SetValue_rejectsRemovedSecretFields(t *testing.T) {
	for _, path := range []string{
		"contexts.existing.grafana.user",
		"contexts.existing.grafana.password",
		"contexts.existing.grafana.token",
	} {
		t.Run(path, func(t *testing.T) {
			req := require.New(t)
			input := config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{Server: "url"},
					},
				},
			}

			err := config.SetValue(&input, path, "value")
			req.Error(err)
			req.ErrorContains(err, "unable to locate path")
		})
	}
}

func Test_UnsetValue(t *testing.T) {
	testCases := []struct {
		name           string
		input          config.Config
		path           string
		expectedOutput config.Config
	}{
		{
			name:           "string",
			input:          config.Config{CurrentContext: "ctx-name"},
			path:           "current-context",
			expectedOutput: config.Config{},
		},
		{
			name: "map entry",
			input: config.Config{
				Contexts: map[string]*config.Context{
					"dev": {
						Grafana: &config.GrafanaConfig{Server: "dev-url"},
					},
					"prod": {
						Grafana: &config.GrafanaConfig{Server: "prod-url"},
					},
				},
			},
			path: "contexts.prod",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"dev": {
						Grafana: &config.GrafanaConfig{Server: "dev-url"},
					},
				},
			},
		},
		{
			name: "string in context",
			input: config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{Server: "url"},
					},
				},
			},
			path: "contexts.existing.grafana.server",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{Server: ""},
					},
				},
			},
		},
		{
			name: "boolean in new context",
			input: config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{TLS: &config.TLS{Insecure: true}},
					},
				},
			},
			path: "contexts.existing.grafana.tls.insecure-skip-verify",
			expectedOutput: config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{TLS: &config.TLS{Insecure: false}},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			req := require.New(t)

			err := config.UnsetValue(&testCase.input, testCase.path)
			req.NoError(err)

			req.Equal(testCase.expectedOutput, testCase.input)
		})
	}
}

func Test_UnsetValue_withUnknownField(t *testing.T) {
	req := require.New(t)
	input := config.Config{}

	err := config.UnsetValue(&input, "unknown")
	req.Error(err)
}

// Test_UnsetValue_rejectsRemovedSecretFields mirrors
// Test_SetValue_rejectsRemovedSecretFields for `grafanapi config unset`.
func Test_UnsetValue_rejectsRemovedSecretFields(t *testing.T) {
	for _, path := range []string{
		"contexts.existing.grafana.user",
		"contexts.existing.grafana.password",
		"contexts.existing.grafana.token",
	} {
		t.Run(path, func(t *testing.T) {
			req := require.New(t)
			input := config.Config{
				Contexts: map[string]*config.Context{
					"existing": {
						Grafana: &config.GrafanaConfig{Server: "url"},
					},
				},
			}

			err := config.UnsetValue(&input, path)
			req.Error(err)
			req.ErrorContains(err, "unable to locate path")
		})
	}
}
