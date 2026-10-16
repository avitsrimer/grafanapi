package onerror_test

import (
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/onerror"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestMode_StopOnError(t *testing.T) {
	tests := []struct {
		mode onerror.Mode
		want bool
	}{
		{onerror.Ignore, false},
		{onerror.Fail, false},
		{onerror.Abort, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			require.Equal(t, tc.want, tc.mode.StopOnError())
		})
	}
}

func TestMode_FailOnErrors(t *testing.T) {
	tests := []struct {
		mode onerror.Mode
		want bool
	}{
		{onerror.Ignore, false},
		{onerror.Fail, true},
		{onerror.Abort, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			require.Equal(t, tc.want, tc.mode.FailOnErrors())
		})
	}
}

func TestMode_Validate(t *testing.T) {
	tests := []struct {
		mode    onerror.Mode
		wantErr bool
	}{
		{onerror.Ignore, false},
		{onerror.Fail, false},
		{onerror.Abort, false},
		{"bad-value", true},
		{"", true},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			err := tc.mode.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestBindFlag(t *testing.T) {
	t.Run("defaults to fail", func(t *testing.T) {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

		var mode onerror.Mode
		onerror.BindFlag(flags, &mode)

		require.Equal(t, onerror.Fail, mode)

		flag := flags.Lookup("on-error")
		require.NotNil(t, flag)
		require.Equal(t, string(onerror.Fail), flag.DefValue)
		require.NoError(t, mode.Validate())
	})

	t.Run("parses a provided value", func(t *testing.T) {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

		var mode onerror.Mode
		onerror.BindFlag(flags, &mode)

		require.NoError(t, flags.Parse([]string{"--on-error", "abort"}))
		require.Equal(t, onerror.Abort, mode)
	})

	t.Run("keeps an invalid value for Validate to reject", func(t *testing.T) {
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

		var mode onerror.Mode
		onerror.BindFlag(flags, &mode)

		require.NoError(t, flags.Parse([]string{"--on-error", "nope"}))
		require.Equal(t, onerror.Mode("nope"), mode)
		require.Error(t, mode.Validate())
	})
}
