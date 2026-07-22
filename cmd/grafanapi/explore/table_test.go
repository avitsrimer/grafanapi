// Package explore (whitebox, not explore_test) is intentional here: these tests drive the
// unexported tableCodec type directly to cover its Format/Decode/Encode branches.
//
//nolint:testpackage // whitebox test: needs access to the unexported tableCodec type
package explore

import (
	"bytes"
	"testing"

	"github.com/grafana/grafanapi/internal/explore"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTableCodec_Format(t *testing.T) {
	assert.Equal(t, format.Format("table"), (&tableCodec{}).Format())
}

func TestTableCodec_Decode_ReturnsUnsupportedError(t *testing.T) {
	err := (&tableCodec{}).Decode(bytes.NewReader(nil), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support decoding")
}

func TestTableCodec_Encode_TypeAssertionFailure(t *testing.T) {
	var buf bytes.Buffer

	err := (&tableCodec{}).Encode(&buf, "not-a-query-response")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported input type")
	assert.Empty(t, buf.String())
}

func TestTableCodec_Encode_DelegatesToRenderTable(t *testing.T) {
	resp := &explore.QueryResponse{
		Results: map[string]explore.FrameResult{"A": {}},
	}

	var buf bytes.Buffer
	require.NoError(t, (&tableCodec{}).Encode(&buf, resp))
	assert.Equal(t, "A: no data\n", buf.String())
}
