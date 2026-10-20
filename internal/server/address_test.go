package server_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/server"
	"github.com/stretchr/testify/assert"
)

func TestIsLoopbackAddress(t *testing.T) {
	tests := []struct {
		address string
		want    bool
	}{
		{address: "127.0.0.1", want: true},
		{address: "127.0.0.2", want: true},
		{address: "::1", want: true},
		{address: "localhost", want: true},
		{address: "LOCALHOST", want: true},
		{address: "", want: false},
		{address: "0.0.0.0", want: false},
		{address: "::", want: false},
		{address: "192.168.1.10", want: false},
		{address: "grafana.example.com", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.address, func(t *testing.T) {
			assert.Equal(t, tc.want, server.IsLoopbackAddress(tc.address))
		})
	}
}
