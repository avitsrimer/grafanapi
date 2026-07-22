package datasources

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/format"
)

// tableCodec implements format.Codec for the "table" output format: it type-asserts the decoded
// models.DataSourceList and renders it as NAME/UID/TYPE/DEFAULT columns. Mirrors
// cmd/grafanapi/explore/table.go's tableCodec.
type tableCodec struct{}

func (c *tableCodec) Format() format.Format {
	return "table"
}

func (c *tableCodec) Encode(output io.Writer, input any) error {
	items, ok := input.(models.DataSourceList)
	if !ok {
		return fmt.Errorf("table codec: unsupported input type %T", input)
	}

	out := tabwriter.NewWriter(output, 0, 4, 2, ' ', tabwriter.TabIndent|tabwriter.DiscardEmptyColumns)

	fmt.Fprintf(out, "NAME\tUID\tTYPE\tDEFAULT\n")
	for _, ds := range items {
		fmt.Fprintf(out, "%s\t%s\t%s\t%t\n", ds.Name, ds.UID, ds.Type, ds.IsDefault)
	}

	return out.Flush()
}

func (c *tableCodec) Decode(io.Reader, any) error {
	return errors.New("table codec does not support decoding")
}
