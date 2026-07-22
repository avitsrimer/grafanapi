package explore

import (
	"errors"
	"fmt"
	"io"

	"github.com/grafana/grafanapi/internal/explore"
	"github.com/grafana/grafanapi/internal/format"
)

// tableCodec implements format.Codec for the "table" output format: it type-asserts the decoded
// *explore.QueryResponse and delegates rendering to explore.RenderTable. Mirrors
// cmd/grafanapi/resources/get.go's tableCodec.
type tableCodec struct{}

func (c *tableCodec) Format() format.Format {
	return "table"
}

func (c *tableCodec) Encode(output io.Writer, input any) error {
	resp, ok := input.(*explore.QueryResponse)
	if !ok {
		return fmt.Errorf("table codec: unsupported input type %T", input)
	}

	return explore.RenderTable(output, resp, explore.RenderOptions{})
}

func (c *tableCodec) Decode(io.Reader, any) error {
	return errors.New("table codec does not support decoding")
}
