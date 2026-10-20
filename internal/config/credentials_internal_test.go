package config

import (
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLegacyBindAllowed(t *testing.T) {
	const home = "/Users/someone"

	clean := map[string]string{"HOME": home}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		maps.Copy(env, clean)
		maps.Copy(env, extra)

		return env
	}

	okHome := func() (string, error) { return home, nil }

	tests := []struct {
		name         string
		explicitFile string
		envApplied   bool
		env          map[string]string
		passwd       func() (string, error)
		want         bool
	}{
		{name: "all clear", envApplied: true, env: clean, passwd: okHome, want: true},
		{name: "explicit --config", explicitFile: "/tmp/c.yaml", envApplied: true, env: clean, passwd: okHome},
		{name: "GRAFANAPI_CONFIG set", envApplied: true, env: with(map[string]string{ConfigFileEnvVar: "/tmp/c.yaml"}), passwd: okHome},
		{name: "XDG_CONFIG_HOME set", envApplied: true, env: with(map[string]string{"XDG_CONFIG_HOME": "/tmp/x"}), passwd: okHome},
		{name: "XDG_CONFIG_DIRS set", envApplied: true, env: with(map[string]string{"XDG_CONFIG_DIRS": "/tmp/x"}), passwd: okHome},
		{name: "GRAFANA_SERVER set", envApplied: true, env: with(map[string]string{"GRAFANA_SERVER": "https://evil.example"}), passwd: okHome},
		{name: "GRAFANA_SERVER empty still counts by presence", envApplied: true, env: with(map[string]string{"GRAFANA_SERVER": ""}), passwd: okHome},
		{name: "GRAFANA_SERVER ignored when env overrides are not applied", envApplied: false, env: with(map[string]string{"GRAFANA_SERVER": "https://evil.example"}), passwd: okHome, want: true},
		{name: "GRAFANAPI_CONFIG empty is ignored", envApplied: true, env: with(map[string]string{ConfigFileEnvVar: ""}), passwd: okHome, want: true},
		{name: "HOME differs from passwd home", envApplied: true, env: with(map[string]string{"HOME": "/tmp/fake"}), passwd: okHome},
		{name: "HOME unset", envApplied: true, env: map[string]string{}, passwd: okHome},
		{name: "HOME with trailing slash equals passwd home", envApplied: true, env: with(map[string]string{"HOME": home + "/"}), passwd: okHome, want: true},
		{name: "passwd lookup error", envApplied: true, env: clean, passwd: func() (string, error) { return "", errors.New("no passwd") }},
		{name: "passwd home empty", envApplied: true, env: clean, passwd: func() (string, error) { return "", nil }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				v, ok := tc.env[name]
				return v, ok
			}

			got := legacyBindAllowed(tc.explicitFile, tc.envApplied, lookup, tc.passwd)
			assert.Equal(t, tc.want, got)
		})
	}
}
