package logs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/fatih/color"
	"github.com/grafana/grafanapi/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLogger(t *testing.T, opts *logs.Options) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	previous := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = previous })

	buf := &bytes.Buffer{}

	return slog.New(logs.NewHandler(buf, opts)), buf
}

func handlerVariants() map[string]*logs.Options {
	return map[string]*logs.Options{
		"default": nil,
		"replace-attr": {
			ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr { return attr },
		},
	}
}

func TestHandler_LiteralEscapeTextIsNotTurnedIntoRealEscape(t *testing.T) {
	for name, opts := range handlerVariants() {
		t.Run(name, func(t *testing.T) {
			logger, buf := newLogger(t, opts)

			// Backslash, 'x', '1', 'b': no real ESC byte in the input.
			logger.Warn("state fetch failed", "error", `server said \x1b[2J`)

			assert.NotContains(t, buf.String(), "\x1b")
			assert.Contains(t, buf.String(), `server said`)
		})
	}
}

func TestHandler_RealEscapesNeverReachOutput(t *testing.T) {
	hostile := "a\x1b]52;c;aGk=\x07b\x1b[31mred\x1b[0m"

	for name, opts := range handlerVariants() {
		t.Run(name, func(t *testing.T) {
			logger, buf := newLogger(t, opts)

			logger.Warn("msg", "value", hostile)
			logger.Warn("msg", logs.Err(errors.New(hostile)))

			assert.NotContains(t, buf.String(), "\x1b")
			assert.NotContains(t, buf.String(), "\x07")
		})
	}
}

func TestHandler_MessageIsSanitizedAndSingleLine(t *testing.T) {
	for name, opts := range handlerVariants() {
		t.Run(name, func(t *testing.T) {
			logger, buf := newLogger(t, opts)

			logger.Log(context.Background(), slog.LevelWarn, "before\x1b]52;c;aGk=\x07after\nforged line\r")

			out := buf.String()
			assert.NotContains(t, out, "\x1b")
			assert.NotContains(t, out, "\x07")
			assert.NotContains(t, out, "\r")
			assert.NotContains(t, out, "aGk=")
			assert.Contains(t, out, "beforeafter forged line")
			assert.Equal(t, 1, strings.Count(out, "\n"))
		})
	}
}

func TestHandler_BenignRecordIsUnchanged(t *testing.T) {
	logger, buf := newLogger(t, &logs.Options{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return attr
		},
	})

	logger.Info("hello world", "key", "value", "quoted", "a b")

	require.Equal(t, "INFO hello world key=value quoted=\"a b\" \n", buf.String())
}
