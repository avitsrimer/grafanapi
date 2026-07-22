package explore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
)

// queryTimeout bounds the POST /api/ds/query request Run issues. Without it, an unreachable or
// firewalled Grafana host would hang the calling command indefinitely - matches
// internal/session.verifyTimeout and internal/config.rotateTimeout.
const queryTimeout = 30 * time.Second

// maxErrorBodySnippet bounds how much of a non-2xx response body is read into an error message,
// so a misbehaving server returning an oversized body cannot make Run buffer unbounded memory.
const maxErrorBodySnippet = 2048

// Run executes body (a /api/ds/query request payload, typically built via BuildRequest) against
// gCtx's configured Grafana instance and returns the decoded response.
//
// The request goes over the raw-HTTP seam - httputils.NewTransport wrapped with
// gCtx.Grafana.WrapWithSession - rather than the generated client, because the generated client's
// response model cannot represent the wire JSON-dataframe shape (see the package doc and the
// "Response-decoding decision" in docs/plans/20260722-explore-command.md). Wrapping with
// WrapWithSession is deliberate and load-bearing: it is what makes a 401 rotate-and-retry exactly
// like every other authenticated command, unlike internal/session.VerifyCookie, which uses the
// bare transport by design.
//
// The request body is passed as a *bytes.Reader so http.NewRequestWithContext auto-populates
// req.GetBody, letting the rotating round-tripper replay the full JSON body on a rotate-and-retry.
//
// Status handling:
//   - 200/207: the body is decoded into a *QueryResponse; if it carries a per-refId error (see
//     QueryResponse.FirstError), that is returned as a plain error.
//   - 401: rotation (if any) has already been attempted by the transport and failed or was
//     exhausted, so the session is truly dead. A *runtime.APIError{Code: 401} is returned (never
//     session.ErrUnauthorized) so cmd/grafanapi/fail's convertSessionErrors renders it as the
//     stale-session message, not the login-rejected one.
//   - anything else: a bounded snippet of the body is included in a plain error.
func Run(ctx context.Context, gCtx *config.Context, body any) (*QueryResponse, error) {
	if gCtx == nil || gCtx.Grafana == nil {
		return nil, errors.New("explore: no grafana context configured")
	}

	queryURL, err := buildQueryURL(gCtx.Grafana.Server)
	if err != nil {
		return nil, fmt.Errorf("explore: invalid server address: %w", err)
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("explore: encoding query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, queryURL.String(), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("explore: building request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if gCtx.Grafana.OrgID != 0 {
		req.Header.Set("X-Grafana-Org-Id", strconv.FormatInt(gCtx.Grafana.OrgID, 10))
	}

	transport, err := httputils.NewTransport(gCtx)
	if err != nil {
		return nil, fmt.Errorf("explore: %w", err)
	}

	client := &http.Client{Timeout: queryTimeout, Transport: gCtx.Grafana.WrapWithSession(transport)}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("explore: query request: %w", err)
	}
	defer resp.Body.Close()

	return handleQueryResponse(resp)
}

// handleQueryResponse implements Run's status-code decision tree; split out so Run itself stays
// focused on request construction.
func handleQueryResponse(resp *http.Response) (*QueryResponse, error) {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusMultiStatus:
		decoded, err := Decode(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("explore: decoding response: %w", err)
		}

		if refID, msg := decoded.FirstError(); msg != "" {
			return nil, fmt.Errorf("query %q failed: %s", refID, msg)
		}

		return decoded, nil
	case http.StatusUnauthorized:
		var payload any
		if snippet := readBodySnippet(resp.Body); snippet != "" {
			payload = snippet
		}

		return nil, runtime.NewAPIError("explore query", payload, http.StatusUnauthorized)
	default:
		snippet := readBodySnippet(resp.Body)
		if snippet == "" {
			return nil, fmt.Errorf("explore: unexpected status %d", resp.StatusCode)
		}

		return nil, fmt.Errorf("explore: unexpected status %d: %s", resp.StatusCode, snippet)
	}
}

// buildQueryURL appends /api/ds/query to server, preserving any existing path - mirrors
// buildUserURL (internal/session/session.go) and buildRotateURL (internal/config/session_source.go).
func buildQueryURL(server string) (*url.URL, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	trimmedPath := strings.TrimSuffix(parsed.Path, "/")
	parsed.Path = trimmedPath + "/api/ds/query"
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed, nil
}

// readBodySnippet reads up to maxErrorBodySnippet bytes of r for inclusion in an error message,
// trimming surrounding whitespace. Read errors are ignored: a best-effort, possibly-empty snippet
// is preferable to failing the already-failing request handling.
func readBodySnippet(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxErrorBodySnippet))

	return strings.TrimSpace(string(data))
}
