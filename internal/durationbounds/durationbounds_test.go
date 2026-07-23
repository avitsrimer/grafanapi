package durationbounds_test

import (
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/durationbounds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWithDays(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{name: "plain Go duration", input: "12h", expected: 12 * time.Hour},
		{name: "day suffix", input: "6d", expected: 6 * 24 * time.Hour},
		{name: "fractional day suffix", input: "1.5d", expected: 36 * time.Hour},
		{name: "unparseable", input: "not-a-duration", wantErr: true},
		{name: "invalid day count", input: "xd", wantErr: true},
		{name: "positive infinity day count", input: "Infd", wantErr: true},
		{name: "negative infinity day count", input: "-Infd", wantErr: true},
		{name: "NaN day count", input: "NaNd", wantErr: true},
		{name: "day count overflowing int64 nanoseconds", input: "1e300d", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := durationbounds.ParseWithDays(testCase.input)
			if testCase.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, testCase.expected, got)
		})
	}
}
