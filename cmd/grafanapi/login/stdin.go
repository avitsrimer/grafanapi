package login

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// readCookieFromStdin reads the full contents of cmd's stdin, trims trailing whitespace and
// newlines, and returns an error if the result is empty. It backs --cookie-stdin on both `login`
// and `login update`, letting the session cookie be piped in non-interactively instead of typed
// at a TTY prompt, e.g.:
//
//	pbpaste | grafanapi login update --cookie-stdin
//
// It reads via cmd.InOrStdin() (rather than os.Stdin directly) so tests can inject a fake reader.
func readCookieFromStdin(cmd *cobra.Command) (string, error) {
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("reading cookie from stdin: %w", err)
	}

	cookie := strings.TrimRight(string(data), " \t\r\n")
	if cookie == "" {
		return "", errors.New("--cookie-stdin: stdin was empty; pipe the session cookie value, e.g. `pbpaste | grafanapi login update --cookie-stdin`")
	}

	return cookie, nil
}
