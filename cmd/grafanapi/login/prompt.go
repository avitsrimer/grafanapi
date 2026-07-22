package login

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// prompter reads interactive input for the login command. Its methods are exported so that
// fakes defined in an external test package (cmd/grafanapi/login_test, following this repo's
// convention of testing through the public API) can satisfy the interface; the interface type
// itself stays unexported since it is only ever used as an injectable seam within this package.
//
// Production code uses ttyPrompter, which reads directly from /dev/tty so prompts work even when
// the command's stdin is piped or redirected; ttyPrompter is not unit-tested directly (doing so
// requires a real TTY) and is instead exercised end-to-end via the Post-Completion manual test
// plan. All login/login-update logic is tested through this interface using fakes.
type prompter interface {
	// PromptLine prints label and returns a single line of input, echoed as typed.
	PromptLine(label string) (string, error)
	// PromptSecret prints label and returns a single line of input without echoing it back to
	// the terminal (e.g. via golang.org/x/term.ReadPassword).
	PromptSecret(label string) (string, error)
}

// ttyPrompter is the production prompter. It opens /dev/tty for both prompt output and input so
// that login works correctly even when the command's own stdin/stdout are redirected.
type ttyPrompter struct{}

func (ttyPrompter) PromptLine(label string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("opening terminal: %w", err)
	}
	defer tty.Close()

	if _, err := fmt.Fprint(tty, label); err != nil {
		return "", fmt.Errorf("writing prompt: %w", err)
	}

	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading input: %w", err)
	}

	return strings.TrimRight(line, "\r\n"), nil
}

// PromptSecret is only ever used to prompt for the session cookie, so a missing terminal here
// (e.g. non-interactive shells, CI) points the caller at --cookie-stdin as the non-interactive
// alternative rather than just reporting the low-level open failure.
func (ttyPrompter) PromptSecret(label string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal available for the cookie prompt; use --cookie-stdin to pipe the value: %w", err)
	}
	defer tty.Close()

	if _, err := fmt.Fprint(tty, label); err != nil {
		return "", fmt.Errorf("writing prompt: %w", err)
	}

	secret, err := term.ReadPassword(int(tty.Fd()))
	// term.ReadPassword suppresses the newline the user's Enter key would normally echo; print
	// one ourselves so anything printed after the prompt starts on a fresh line.
	fmt.Fprintln(tty)

	if err != nil {
		return "", fmt.Errorf("reading secret input: %w", err)
	}

	return string(secret), nil
}
