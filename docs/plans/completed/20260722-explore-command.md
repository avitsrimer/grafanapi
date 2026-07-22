# `grafanapi explore` — ad-hoc datasource queries from the terminal

## Overview

**What:** A new top-level command that runs a single ad-hoc query against any configured Grafana
datasource and prints the result, mirroring Grafana's Explore UI:

```
grafanapi explore <datasource-uid-or-name> "<query>" [flags]
```

It resolves the datasource (by UID, then by name), maps the positional query string onto the
datasource-type-appropriate request field (`expr` for Prometheus/Loki, `rawSql` for SQL datasources,
`target` for Graphite, `query` for Elasticsearch/InfluxDB), POSTs a single-query `/api/ds/query`
request, decodes the response, and renders it as a human-friendly table (default) or as `json`/`yaml`
for piping to tools like `jq`.

**Why:** Operators already authenticate `grafanapi` to a Grafana instance (session cookie in the
Keychain, automatic rotation on `401`). Today there is no way to *read data* through the tool — only
to manage resources. `explore` closes that gap: a one-liner to sanity-check a PromQL/LogQL/SQL query
or a datasource's health from the terminal or a CI job, reusing the exact auth, org, and TLS plumbing
the rest of the CLI already has.

**Integration:** Builds directly on the completed session-cookie auth and automatic-rotation work
(`docs/plans/completed/20260721-session-cookie-auth-and-release.md`,
`docs/plans/completed/20260722-auto-rotate-session-on-401.md`). Datasource lookup goes through the
existing generated-client seam (`internal/grafana.ClientFromContext`), which already wires org-id,
TLS, the session cookie, and `401` rotation. The query request goes through the raw-HTTP seam
(`internal/httputils.NewTransport` wrapped with `GrafanaConfig.WrapWithSession`) so we control
response decoding end to end — this is a deliberate, load-bearing choice justified in the Solution
Overview. Note this is *not* identical to `internal/session.VerifyCookie`: that path uses the bare
transport without `WrapWithSession` (no rotation, by design); the query path must wrap it so a `401`
rotates-and-retries like every other authenticated command.

## Context (from discovery)

### OpenAPI client surface (vendored, `vendor/github.com/grafana/grafana-openapi-client-go`)

- **`/api/ds/query` is in the generated client:** `client.Datasources.QueryMetricsWithExpressions(body
  *models.MetricRequest) (*QueryMetricsWithExpressionsOK, *QueryMetricsWithExpressionsMultiStatus,
  error)` — `POST /ds/query`, `200`→OK, `207`→MultiStatus, typed errors for `400/401/403/500`.
- **Request model `models.MetricRequest`** (`models/metric_request.go`): `From *string` (required),
  `To *string` (required), `Queries []JSON` (required; `JSON = interface{}`), `Debug bool`. The query
  entries are fully untyped — we build `map[string]any`.
- **Response model is LOSSY — the reason we do NOT decode via the generated client for the query.**
  `QueryMetricsWithExpressionsOK.Payload` is `*models.QueryDataResponse` →
  `Results map[string]DataResponse`. `models.DataResponse` has `Error string \`json:"Error"\``,
  `Frames Frames`, `Status`. `models.Frame` (`models/frame.go`) has **only** `Fields []*Field
  \`json:"Fields"\``, `Meta \`json:"Meta"\``, `Name \`json:"Name"\``, `RefID \`json:"RefID"\`` — note
  the **capitalized** JSON tags and, critically, **no `data`/`schema` split and no column values.**
  `models.Field` (`models/field.go`) carries **only** `Config`, `Labels`, `Name` — **no values**.
  Grafana's real `/api/ds/query` wire format is the JSON-dataframe shape
  `results.<refId>.frames[i] = {schema:{name,refId,fields:[{name,type,typeInfo,labels}]},
  data:{values:[[...],[...]]}}` (lowercase, `schema`/`data` split, column-major `data.values`). The
  go-openapi consumer decodes the body into the lossy struct above and discards `data.values`
  **permanently** — so "call the generated client, then re-marshal its payload" cannot work: the
  values are gone before we ever hold the payload. Verified by reading `models/frame.go`,
  `models/field.go`, `models/data_response.go`.
- **Datasource lookup — all present** in `client/datasources`:
  - `GetDataSourceByUID(uid) (*GetDataSourceByUIDOK, error)` → `Payload *models.DataSource`.
    On `404` returns typed error `*datasources.GetDataSourceByUIDNotFound` (implements `Code() int`
    and `IsCode(int) bool`; `Code()==404`).
  - `GetDataSourceByName(name) (*GetDataSourceByNameOK, error)` → `Payload *models.DataSource`;
    `404` → `*datasources.GetDataSourceByNameNotFound`.
  - `GetDataSources() (*GetDataSourcesOK, error)` → `Payload models.DataSourceList`
    (`[]*models.DataSourceListItemDTO`, each with `Name`, `UID`, `Type`).
  - `models.DataSource` has `UID`, `Name`, `Type`, `ID`, `OrgID`.
- **Org header:** the generated client sets `X-Grafana-Org-Id` (const `OrgIDHeader`,
  `grafana_http_api_client.go:78`) automatically when `TransportConfig.OrgID != 0`. The raw-HTTP
  query path bypasses the generated client, so it must set this header itself (see Technical Details).

### Auth / transport seams

- **Generated-client seam — `internal/grafana/client.go` `ClientFromContext(ctx *config.Context)`:**
  builds `goapi.TransportConfig{Host, BasePath, Schemes, TLSConfig, OrgID}`, then wraps the runtime's
  `Transport` with `ctx.Grafana.WrapWithSession(...)` — session cookie + `401` rotation for free.
  `GetVersion` is the usage example. **Used here for datasource lookup.**
- **Raw-HTTP seam:** `httputils.NewTransport(gCtx)` → **`gCtx.Grafana.WrapWithSession(transport)`** →
  `&http.Client{Timeout, Transport: wrapped}`. **Used here for the query POST.**
  - **Caveat — `internal/session/session.go` `VerifyCookie` is NOT a full template for this:** it
    deliberately uses the **bare** `httputils.NewTransport(gCtx)` **without** `WrapWithSession`,
    because it is the login/verification path where rotation must not happen (it is validating a
    freshly-pasted cookie, not driving an established session). Copying `VerifyCookie` verbatim would
    **silently drop rotation** on the query path. `run.go` MUST wrap the transport with
    `gCtx.Grafana.WrapWithSession(...)`. Cite `VerifyCookie` only for its URL-building convention
    (parse `gCtx.Grafana.Server`, trim trailing `/`, append the API path, clear query/fragment) and
    its no-logging-round-tripper rule — not for its transport construction.
- **`WrapWithSession`** on `config.GrafanaConfig` returns the rotating round-tripper when a
  `SessionSource` is present, the static one when only a cookie is present, else the transport
  unchanged. `401` on either seam therefore triggers rotation-and-retry transparently.

### Command / options / output patterns

- **Registration:** `cmd/grafanapi/root/command.go` → `rootCmd.AddCommand(explore.Command())`
  alongside `config`, `login`, `resources`.
- **Options pattern:** a package `Options` struct with flag fields, `BindFlags(*pflag.FlagSet)`,
  `Validate() error`, and a `Command()`/`fooCmd(...)` builder that wires `RunE`. `login.Command()`
  shows a standalone top-level command; `resources` shows threading a shared `cmdconfig.Options`
  (which binds `--config`/`--context` and exposes `LoadConfig`/`LoadRESTConfig`).
- **`cmdconfig.Options.LoadConfig(ctx)`** (`cmd/grafanapi/config/command.go`) resolves the session
  cookie (+ `SessionSource`) from the Keychain and validates the current context; returns
  `config.Config`, and `cfg.GetCurrentContext()` yields the `*config.Context` that
  `grafana.ClientFromContext` and `httputils.NewTransport` both need.
- **Output:** `cmd/grafanapi/io/format.go` `Options` — builtin `json`/`yaml` codecs plus
  `RegisterCustomCodec(name, codec)` and `DefaultFormat(name)`; `BindFlags` adds `-o/--output`.
  `resources/get.go` registers a custom `tableCodec` (implements `internal/format.Codec` =
  `Encode`/`Decode`/`Format`). `config/command.go`'s `listContextsCmd` shows the plain
  `text/tabwriter` pattern we will reuse (no third-party table dependency exists in `go.mod`).
- **Error rendering:** `cmd/grafanapi/fail/convert.go` — `convertSessionErrors` maps a
  `runtime.APIError{Code:401}` (openapi) to `staleSessionError` (summary "Grafana session is stale or
  unauthorized", suggestion `Run: grafanapi login update`, exit code 2), and `convertAPIErrors` maps
  k8s `401`s the same way. **Note the asymmetry that dictates our 401 handling:** the bare sentinel
  `session.ErrUnauthorized` maps instead to `loginRejectedError` (summary "Grafana rejected the
  provided session cookie", suggestion "Verify the session cookie value and try again", same exit
  code) — that is the *login-prompt* rejection message, reserved for `login`/`login update`, **not**
  the expired-session message. So a dead-session `401` on the query path must be surfaced as a
  `*runtime.APIError{Code:401}`, never as `session.ErrUnauthorized`. Any other error renders
  generically with exit 1.

### Reference-docs generation (CI-enforced)

- `make cli-reference` runs `scripts/cmd-reference/main.go`, which calls `root.Command("version")`
  and `doc.GenMarkdownTree` into `docs/reference/cli`. A new top-level command adds new generated
  pages (`docs/reference/cli/grafanapi_explore.md` and an updated `grafanapi.md` index). `make
  reference-drift` (CI) fails if these are not regenerated and committed. Regeneration is MANDATORY.

## Development Approach

- **Testing approach: Regular** — write each task's implementation first, then its tests, matching
  repo conventions: `testify` `assert`/`require`, table-driven cases, `httptest.Server` fixtures for
  every HTTP interaction, realistic JSON payload fixtures under `testdata/`. **No live Grafana in
  unit tests.**
- Complete each task **fully** (including tests) before starting the next. Every task ends with a
  "run tests — must pass before next task" gate.
- **Scope discipline (minimal):** single query only (fixed `refId` `"A"`), no multi-query, no query
  history, no CSV output (note as future work — `json` covers piping), no streaming, no client-side
  time parsing (relative strings pass through to Grafana verbatim).
- Keep it elegant: domain logic (decoding, datasource resolution, query building, rendering) lives in
  a new `internal/explore` package that is fully unit-testable without Cobra; the `cmd/grafanapi/explore`
  package is a thin wiring layer (flags + a `tableCodec` adapter). Cross-cutting concerns (auth, org,
  TLS, rotation) are reused, never reimplemented.
- If scope changes mid-implementation, **update this plan file** (add ➕ tasks, mark ⚠️ blockers)
  before continuing.

## Testing Strategy

- **Decoding (`internal/explore`, package-level, table-driven, fixture-backed):** the single highest
  risk. Decode realistic JSON-dataframe fixtures — a Prometheus range vector with a labelled value
  field and epoch-ms time field; a SQL `format:"table"` frame with mixed string/number/null columns;
  a top-level error response; a `207` partial-success with a per-`refId` `error` — and assert the
  decoded structs and derived rows (including time-field detection and `null` handling).
- **Datasource resolution:** drive `grafana.ClientFromContext` against an `httptest.Server` whose
  handler scripts `GET /api/datasources/uid/{uid}`, `/name/{name}`, and `/api/datasources`; cover
  UID-hit, UID-miss→name-hit, both-miss→listing-error (message contains each datasource's
  name+uid+type), and a non-404 error (e.g. `401`) propagating unchanged.
- **Query building (pure, table-driven):** every supported type→field mapping; SQL types also set
  `format:"table"`; `--field` overrides the key; unknown type errors and names the type; `--param`
  values parsed as JSON when valid (number/bool/object) else string, and overriding generated keys;
  `--interval` → `intervalMs`; `--instant` → `instant:true,range:false`; `--max-data-points` →
  `maxDataPoints`; `--from`/`--to` passed verbatim.
- **Query execution:** `httptest.Server` scripting `POST /api/ds/query` — assert the request body
  (from/to/queries[0] shape), the wrapper-injected `Cookie` header, and `X-Grafana-Org-Id` when org
  set; decode a `200` success; a `401`→rotate→`200` script whose retried request replays the full
  JSON body (via `GetBody`); surface a `207`/per-`refId` error as a Go error; and confirm a
  dead-session `401` (rotation exhausted) surfaces as `*runtime.APIError{Code:401}` that
  `fail.ErrorToDetailedError` renders as **stale-session** (exit 2), not the login-rejected message.
- **Rendering:** golden-style assertions over decoded fixtures — sequential frames each with a
  name/refId header, RFC3339 time columns, series labels in the value-column header, wide-cell
  truncation; and that `json`/`yaml` codecs emit the full decoded structure untruncated.
- **Command wiring:** end-to-end through `httptest.Server` + a fake `keychain.Store` (via
  `cmdconfig.SetKeychainStore`) + a temp config file — `explore <uid> "<query>"` prints a table;
  `-o json` prints decodable JSON; a per-`refId` error exits non-zero.

## Progress Tracking

- Mark `- [x]` immediately upon completing each checkbox.
- Prefix discovered-mid-work tasks with ➕ and append them in-place.
- Prefix blockers with ⚠️ and stop to re-plan when one is hit.

## Solution Overview

### Response-decoding decision (RESOLVED): raw `POST /api/ds/query` + our own wire structs

**Decision:** send the query via the **raw-HTTP seam** — `httputils.NewTransport` **wrapped with**
`GrafanaConfig.WrapWithSession` — and decode the raw response body into our own structs that match
Grafana's real JSON-dataframe wire shape. Datasource **lookup** still uses the generated client
(`grafana.ClientFromContext`). (`session.VerifyCookie` uses the same `httputils.NewTransport` seam but
deliberately *without* `WrapWithSession`; the query path adds the wrap so `401`s rotate — see the
Context caveat.)

**Justification (why not the generated client for the query):**

1. **The generated response model is lossy and cannot round-trip the wire JSON — verified, not
   assumed.** `models.Frame` has no `schema`/`data` split and `models.Field` has no values field, so
   the go-openapi consumer decodes `/api/ds/query` responses into a struct that **drops every column
   value** (`data.values`). Option (a) — "call `QueryMetricsWithExpressions`, then re-marshal its
   payload" — is therefore impossible: the data is discarded during the client's own decode, before
   the payload is returned. Re-marshalling a value-less struct yields empty frames.
2. **Even the field metadata mis-maps.** `models.Frame`'s tags are capitalized (`Fields`, `Meta`,
   `Name`, `RefID`) and there is no `schema` object, whereas the wire format nests `schema.fields`
   with lowercase keys. The model was generated from a Go-struct swagger definition, not the actual
   HTTP JSON contract.
3. **The raw seam is already the established pattern for a bespoke authenticated request** — TLS
   comes from `httputils.NewTransport`, and the session cookie + `401` rotation come from wrapping it
   with `WrapWithSession` (the k8s and openapi paths do exactly this;  `session.VerifyCookie`
   demonstrates the `httputils.NewTransport` + URL-building half, minus the wrap it intentionally
   omits). The only thing the generated client adds that the raw path lacks is the `X-Grafana-Org-Id`
   header, which is one line to replicate.

The cost is owning ~4 small wire structs + one decode function, which is exactly what we need to test
against realistic fixtures anyway (the biggest risk item). This is strictly more robust than fighting
a code-generated model that structurally cannot represent the data.

### Package layout

- **`internal/explore/`** (domain; no Cobra):
  - `dataframe.go` — wire structs + `Decode` + row/typed-value derivation (Task 1).
  - `datasource.go` — `ResolveDataSource` via the generated client (Task 2).
  - `query.go` — type→field mapping, param parsing, `BuildQuery`/`BuildRequest` (Task 3).
  - `run.go` — raw `POST /api/ds/query`, org header, decode, per-`refId` error surfacing (Task 4).
  - `render.go` — `RenderTable(io.Writer, *QueryResponse, RenderOptions)` over `text/tabwriter` (Task 5).
- **`cmd/grafanapi/explore/`** (thin wiring):
  - `command.go` — `Options`, `BindFlags`, `Validate`, `Command()`; RunE orchestration (Task 6).
  - `table.go` — `tableCodec` implementing `format.Codec`, delegating to `explore.RenderTable` (Task 6).
- **Register** in `cmd/grafanapi/root/command.go` (Task 6).

## Technical Details

### Wire structs (`internal/explore/dataframe.go`)

```go
type QueryResponse struct {
    Results map[string]FrameResult `json:"results"`
}

type FrameResult struct {
    Status int     `json:"status,omitempty"`
    Frames []Frame `json:"frames,omitempty"`
    Error  string  `json:"error,omitempty"`
}

type Frame struct {
    Schema FrameSchema `json:"schema"`
    Data   FrameData   `json:"data"`
}

type FrameSchema struct {
    Name   string        `json:"name,omitempty"`
    RefID  string        `json:"refId,omitempty"`
    Fields []FieldSchema `json:"fields"`
}

type FieldSchema struct {
    Name   string            `json:"name"`
    Type   string            `json:"type"`   // "time","number","string","boolean", ...
    Labels map[string]string `json:"labels,omitempty"`
}

type FrameData struct {
    // Column-major: Values[col][row]. Cells are kept as json.RawMessage so the renderer can
    // format per-column by schema type (e.g. epoch-ms time -> RFC3339) and JSON/YAML output can
    // re-emit them untouched.
    Values [][]json.RawMessage `json:"values"`
}
```

- `Decode(r io.Reader) (*QueryResponse, error)` — `json.NewDecoder(...).Decode`. Missing `results` is
  a valid empty response, not an error.
- Row derivation helper on `Frame`: `RowCount()` = length of the longest column; `Cell(col,row)`
  returns the `json.RawMessage` or a JSON `null` when the column is shorter (defensive).
- `(*QueryResponse).FirstError()` returns the first non-empty per-`refId` `Error` (and its refId),
  used by both `run.go` and the command to fail on partial errors.

### Datasource resolution (`internal/explore/datasource.go`)

```go
func ResolveDataSource(client *goapi.GrafanaHTTPAPI, ref string) (*models.DataSource, error)
```

- Try `client.Datasources.GetDataSourceByUID(ref)`; on success return `.Payload`.
- On `*datasources.GetDataSourceByUIDNotFound` (detected via `errors.As`), try
  `GetDataSourceByName(ref)`; on success return `.Payload`.
- On `*datasources.GetDataSourceByNameNotFound`, call `GetDataSources()` and return an error listing
  each available datasource as `name (uid, type)`, sorted by name, prefixed with a clear
  "datasource %q not found" message. (If the listing call itself fails, wrap and return that error.)
- **Any non-404 error (e.g. `401`, network) propagates unchanged** so rotation/stale-session and
  network rendering fire as usual.

### Query building (`internal/explore/query.go`)

- Type→field map (compare `strings.ToLower(dsType)`):

  | datasource type | field key | extra defaults |
  |---|---|---|
  | `prometheus` | `expr` | — |
  | `loki` | `expr` | — |
  | `mysql`, `postgres`, `grafana-postgresql-datasource`, `mssql` | `rawSql` | `format: "table"` |
  | `elasticsearch` | `query` | — |
  | `influxdb` | `query` | — |
  | `graphite` | `target` | — |

- `QueryFieldForType(dsType, override string) (key string, extras map[string]any, err error)`:
  when `override != ""`, `key = override` and `extras` still includes the type's extra defaults
  (e.g. SQL `format:"table"`); unknown type with no override → error naming the type and suggesting
  `--field`.
- `BuildQuery(ds *models.DataSource, queryStr string, opts QueryOptions) (map[string]any, error)`
  assembles, in this precedence (lowest → highest):
  1. `refId: "A"`, `datasource: {uid: ds.UID, type: ds.Type}`, `maxDataPoints: <MaxDataPoints>`.
  2. type extras (`format:"table"` for SQL).
  3. the mapped field: `<key>: queryStr`.
  4. `intervalMs` when `--interval` set (`time.ParseDuration(interval).Milliseconds()`);
     `instant:true, range:false` when `--instant`.
  5. `--param key=value` entries — **highest precedence**, so they can override any generated key.
     Value parsing: `var v any; if json.Unmarshal([]byte(value), &v) == nil { use v } else { use the
     raw string }` (so `5`, `true`, `{"a":1}` become typed; bare words stay strings).
- `BuildRequest(query map[string]any, from, to string) models.MetricRequest` (or a plain
  `map[string]any` marshalled by `run.go`) with `From`/`To` set verbatim and `Queries: [query]`.

### Query execution (`internal/explore/run.go`)

```go
const queryTimeout = 30 * time.Second

func Run(ctx context.Context, gCtx *config.Context, body any) (*QueryResponse, error)
```

- Build the URL from `gCtx.Grafana.Server` (parse, trim trailing `/`, append `/api/ds/query`,
  clear query/fragment) — mirror `session.buildUserURL`'s URL convention.
- `http.NewRequestWithContext(ctx, POST, url, bytes.NewReader(jsonBody))`. **The body reader MUST be
  `GetBody`-capable** — `http.NewRequestWithContext` auto-populates `req.GetBody` when the body is a
  `*bytes.Reader`, `*bytes.Buffer`, or `*strings.Reader`, and the rotating round-tripper only retries
  after a `401`+rotation when `req.GetBody != nil`. Use `bytes.NewReader(jsonBody)` (a `*bytes.Reader`)
  so the retried request replays the full JSON body. Do **not** pass an arbitrary `io.Reader`.
- Set `Content-Type: application/json` and `X-Grafana-Org-Id: <OrgID>` **only when
  `gCtx.Grafana.OrgID != 0`** (replicating the generated client's org behaviour on the raw path).
  Do **NOT** manually set the `Cookie` header: `WrapWithSession`'s round-tripper clones each request
  and sets the cookie itself from the live (possibly just-rotated) value, so a manually-set `Cookie`
  is overwritten — setting it here would be a redundant no-op and risks using a stale value. Only
  `X-Grafana-Org-Id` genuinely needs to be set by hand.
- Transport: `httputils.NewTransport(gCtx)` → `gCtx.Grafana.WrapWithSession(transport)` →
  `&http.Client{Timeout: queryTimeout, Transport: wrapped}`. **Never** wrap a logging round-tripper
  (cookie leak).
- Status handling:
  - `200` or `207`: decode the body into `*QueryResponse`; then if `resp.FirstError()` is non-empty,
    return an error `fmt.Errorf("query %q failed: %s", refId, msg)` (exit 1). `207` without a
    per-refId error is treated as success (defensive).
  - `401`: the rotating transport already tried to rotate-and-retry; reaching here means the session
    is truly dead, so return `runtime.NewAPIError("explore query", <bounded body snippet or nil>,
    http.StatusUnauthorized)` — a `*runtime.APIError` with `Code == 401`. The **existing**
    `convertSessionErrors` branch (`errors.As(err, &*runtime.APIError{}) && Code == 401`) then renders
    `staleSessionError` — summary "Grafana session is stale or unauthorized", suggestion `Run:
    grafanapi login update`, exit code 2 — **unchanged**. (Returning `session.ErrUnauthorized` would
    be WRONG here: `convertSessionErrors` maps that sentinel to `loginRejectedError` — "Grafana
    rejected the provided session cookie" / "Verify the session cookie value" — which is the
    login-prompt message, not the expired-session one.) No new error type, no change to `fail/`.
  - any other status (e.g. `400`/`500` — a real non-2xx `/api/ds/query` failure returns a top-level
    `{"message": "...", "traceID": "..."}` envelope, not a per-refId `results` shape): read a bounded
    body snippet and return `fmt.Errorf("query: unexpected status %d: %s", code, snippet)`.

> Imports: `internal/explore` uses `github.com/go-openapi/runtime` (for `NewAPIError`; already
> vendored and a direct dependency, used by `cmd/grafanapi/fail`). It does **not** need
> `internal/session`. No import cycle either way.

### Rendering (`internal/explore/render.go`)

```go
type RenderOptions struct{ MaxCellWidth int } // default 60

func RenderTable(w io.Writer, resp *QueryResponse, opts RenderOptions) error
```

- Iterate `Results` in **sorted refId order** (deterministic). For each `FrameResult`, iterate its
  frames in order. For each frame, print a header line: `# <refId>` plus the frame `Name` when set.
- Columns = `schema.fields`. Header cell = field `Name`; if the field has `Labels`, append them
  compactly, e.g. `value {instance="…", job="…"}` (labels sorted by key).
- Cell formatting by field `Type`: `time` → parse the epoch-ms number and render
  `time.UnixMilli(v).UTC().Format(time.RFC3339)`; `null` → empty; strings → unquoted; everything else
  → the JSON scalar rendered plainly. Truncate any cell longer than `MaxCellWidth` with an ellipsis.
- Use `text/tabwriter` (like `listContextsCmd`) for column alignment. Frames render sequentially with
  a blank line between them.
- **Empty output:** if `Results` is empty, a `FrameResult` has no frames, or a frame has zero rows,
  emit a single graceful line (e.g. `No data.` / `<refId>: no data`) instead of nothing or a panic.
- `json`/`yaml` output does **not** go through `RenderTable`: the command hands the decoded
  `*QueryResponse` straight to the builtin codec, so those outputs are complete and untruncated.

### Command (`cmd/grafanapi/explore/command.go`, `table.go`)

- `Options` fields: `IO cmdio.Options`, `Field string`, `Params []string`, `From string`,
  `To string`, `MaxDataPoints int`, `Interval string`, `Instant bool`.
- `BindFlags`: register the `table` codec (`opts.IO.RegisterCustomCodec("table", &tableCodec{})`),
  `opts.IO.DefaultFormat("table")`, `opts.IO.BindFlags(flags)`; then
  `--field`, `--param` (`StringArrayVar`), `--from` (default `"now-1h"`), `--to` (default `"now"`),
  `--max-data-points` (default `1000`), `--interval` (default `""`), `--instant` (default `false`).
- `Validate`: `opts.IO.Validate()` + validate each `--param` matches `key=value` + validate
  `--interval` parses via `time.ParseDuration` when set.
- `Command()`: builds a `cmdconfig.Options{}`, binds `configOpts.BindFlags(cmd.Flags())` (adds
  `--config`/`--context`) and `opts.BindFlags(cmd.Flags())`; `Args: cobra.ExactArgs(2)`; a helpful
  `Example` block. RunE:
  1. `opts.Validate()`.
  2. `cfg, err := configOpts.LoadConfig(ctx)`; `gCtx := cfg.GetCurrentContext()`.
  3. `gClient, err := grafana.ClientFromContext(gCtx)`.
  4. `ds, err := explore.ResolveDataSource(gClient, args[0])`.
  5. `key, extras, err := explore.QueryFieldForType(ds.Type, opts.Field)`; `query, err :=
     explore.BuildQuery(ds, args[1], ...)`.
  6. `resp, err := explore.Run(ctx, gCtx, body)`.
  7. `codec, err := opts.IO.Codec(); return codec.Encode(cmd.OutOrStdout(), resp)`.
- `tableCodec` implements `format.Codec`: `Format()` → `"table"`; `Encode(w, input)` type-asserts
  `*explore.QueryResponse` and calls `explore.RenderTable`; `Decode` returns an
  "unsupported" error (mirrors `resources/get.go`'s `tableCodec`).

## Implementation Steps

### Task 1 — Response-decoding layer (wire structs + fixtures)

**Files:**
- Create: `internal/explore/dataframe.go`
- Create: `internal/explore/dataframe_test.go`
- Create: `internal/explore/testdata/{prometheus_range.json, sql_table.json,
  multistatus_partial.json, error_envelope.json}`

> Fixture shapes (important — two distinct error shapes):
> - **Per-query errors live inside a `200`/`207` `results` body** — `multistatus_partial.json` is a
>   `results.A` with a non-empty `error` string and a `status` (this is what `FirstError` reads).
> - **A non-2xx transport failure returns a top-level envelope**, not a `results` shape —
>   `error_envelope.json` is `{"message": "...", "traceID": "..."}` and is used by Task 4's
>   unexpected-status path, not by the `Decode`→`FirstError` path. (There is no `error_response.json`
>   with a synthetic `results.<refId>.error` on a non-2xx response — that shape does not occur on the
>   wire.)

- [x] Define `QueryResponse`, `FrameResult`, `Frame`, `FrameSchema`, `FieldSchema`, `FrameData` with
      JSON tags matching the **real** wire shape (lowercase `results`/`frames`/`schema`/`data`/
      `fields`, column-major `data.values` as `[][]json.RawMessage`). Follow the repo Go
      symbol-ordering convention.
- [x] Implement `Decode(io.Reader) (*QueryResponse, error)`, `Frame.RowCount()`, `Frame.Cell(col,row)`
      (defensive on ragged columns), and `(*QueryResponse).FirstError() (refID, msg string)`.
- [x] Add realistic fixtures: `prometheus_range.json` (a range vector — labelled `value` field +
      epoch-ms `time` field), `sql_table.json` (a `format:"table"` frame with string/number/`null`
      columns), `multistatus_partial.json` (a `results.A` carrying a non-empty `error` + `status`),
      and `error_envelope.json` (the top-level `{"message","traceID"}` non-2xx envelope — consumed by
      Task 4, not by `Decode`).
- [x] Write table-driven tests decoding the `results`-shaped fixtures and asserting struct contents,
      `RowCount`, `Cell` (including `null`), and `FirstError` (empty on the two success fixtures,
      populated on `multistatus_partial.json`).
- [x] Run tests — must pass before next task.

### Task 2 — Datasource resolution via the generated client

**Files:**
- Create: `internal/explore/datasource.go`
- Create: `internal/explore/datasource_test.go`

- [x] Implement `ResolveDataSource(client *goapi.GrafanaHTTPAPI, ref string) (*models.DataSource,
      error)`: UID → (404) name → (404) listing-error; detect `404` via `errors.As` on
      `*datasources.GetDataSourceByUIDNotFound` / `*datasources.GetDataSourceByNameNotFound`;
      propagate any non-404 error unchanged.
      ⚠️ **Deviation (verified while implementing):** there is no
      `*datasources.GetDataSourceByNameNotFound` type in the vendored client — reading
      `get_data_source_by_name_responses.go` shows its `ReadResponse` switch only special-cases
      `200/401/403/500`; a `404` falls through to the `default:` branch, which returns a bare
      `runtime.NewAPIError(...)`. So the name-lookup miss is detected instead via
      `errors.As(err, &apiErr)` on `*runtime.APIError` and checking `apiErr.Code ==
      http.StatusNotFound` (same field-access pattern already used in
      `cmd/grafanapi/fail/convert.go`). The UID lookup still uses the typed
      `*datasources.GetDataSourceByUIDNotFound` exactly as planned.
- [x] Build the not-found error message by listing `GetDataSources()` results as `name (uid, type)`
      sorted by name; wrap-and-return a failure of the listing call itself.
- [x] Add a test helper that constructs a `*goapi.GrafanaHTTPAPI` via `grafana.ClientFromContext`
      pointed at an `httptest.Server` (build a `*config.Context` whose `Grafana.Server` is the test
      server URL).
- [x] Write tests: UID-hit; UID-miss→name-hit; both-miss→listing-error (message contains each
      datasource's name+uid+type); a non-404 (`401`) propagating unchanged.
- [x] Run tests — must pass before next task.

### Task 3 — Query building (type mapping, params, flags)

**Files:**
- Create: `internal/explore/query.go`
- Create: `internal/explore/query_test.go`

- [x] Implement `QueryFieldForType(dsType, override)` with the type→field table and SQL
      `format:"table"` extras; unknown type (no override) errors and names the type + suggests
      `--field`; `--field` overrides the key while keeping type extras.
- [x] Implement `--param` parsing (`key=value`, value → JSON when `json.Unmarshal` succeeds else raw
      string) and `BuildQuery(ds, queryStr, QueryOptions)` assembling the precedence chain (base →
      extras → field → interval/instant → params-win), fixed `refId:"A"`, `datasource:{uid,type}`.
- [x] Implement `--interval` → `intervalMs` (`time.ParseDuration(...).Milliseconds()`), `--instant`
      → `instant:true,range:false`, `--max-data-points` → `maxDataPoints`, and the `From`/`To`
      passthrough into the request body.
- [x] Write table-driven tests for every mapping, the SQL `format:"table"` default, `--field`
      override, unknown-type error, param JSON-vs-string parsing and override precedence, interval
      conversion, instant, and verbatim from/to.
- [x] Run tests — must pass before next task.

### Task 4 — Query execution (raw `POST /api/ds/query`)

**Files:**
- Create: `internal/explore/run.go`
- Create: `internal/explore/run_test.go`

- [x] Implement `Run(ctx, gCtx, body)`: build the `/api/ds/query` URL from `gCtx.Grafana.Server`;
      POST JSON via `bytes.NewReader(jsonBody)` (so `req.GetBody` is auto-populated and the body can
      be replayed on retry); set `Content-Type` and `X-Grafana-Org-Id` (only when `OrgID != 0`);
      **do not** set `Cookie` manually (the round-tripper owns it); transport via
      `httputils.NewTransport` **wrapped with `gCtx.Grafana.WrapWithSession`**; bounded `queryTimeout`;
      no logging round-tripper.
- [x] Handle statuses: `200`/`207` → `Decode` then fail on `FirstError`; `401` → return
      `runtime.NewAPIError("explore query", <bounded snippet or nil>, http.StatusUnauthorized)`;
      other → `unexpected status` error with a bounded body snippet.
- [x] Write tests against an `httptest.Server`: assert the outbound body (from/to/`queries[0]`
      field+refId+datasource), the injected `Cookie` header (set by the wrapper, not by `Run`), and
      `X-Grafana-Org-Id` present iff `OrgID != 0`; decode a `200` success.
- [x] Write a **body-replay** test: a scripted `401` (first hit) → rotate → `200` (second hit) with a
      `SessionSource` present; assert the **retried** request still carries the full, byte-identical
      JSON body (proves `GetBody` replay works), the rotated cookie is used, and the decode succeeds.
- [x] Write tests for a `207`/per-`refId` error surfacing as a Go error, and a dead-session `401`
      (after rotation has given up) that, when passed through `fail.ErrorToDetailedError`, yields the
      **stale-session** `DetailedError` (summary "Grafana session is stale or unauthorized",
      suggestion `Run: grafanapi login update`, exit code 2) — asserting the `*runtime.APIError{401}`
      choice renders correctly and is *not* the "rejected"/"verify the session cookie value" message.
- [x] Run tests — must pass before next task.

### Task 5 — Table rendering

**Files:**
- Create: `internal/explore/render.go`
- Create: `internal/explore/render_test.go`

- [x] Implement `RenderTable(w, resp, RenderOptions)`: sorted-refId iteration, per-frame `# <refId>
      [name]` header, `tabwriter` columns, field-`Name` headers with sorted labels appended to
      labelled columns.
- [x] Implement per-type cell formatting (`time` epoch-ms → RFC3339 UTC; `null` → empty; strings
      unquoted; other scalars plain) and `MaxCellWidth` truncation (default 60) with an ellipsis.
- [x] Write golden-style tests over the Task 1 fixtures: a Prometheus frame (time column RFC3339,
      labels in the value header), a SQL table frame (mixed columns, a truncated wide cell), and a
      multi-frame response rendered sequentially.
- [x] Write an **empty-response** test: a `*QueryResponse` with no results, a `FrameResult` with no
      frames, and a frame whose columns are zero-length each render a graceful "no data" line (no
      panic, no ragged output) rather than nothing or an error.
- [x] Add a test asserting `json`/`yaml` codecs (from `internal/format`) emit the full decoded
      `*QueryResponse` untruncated (round-trip decode of the emitted JSON equals the input).
      ⚠️ **Note (verified while implementing):** exact deep-equality on the decoded Go struct is too
      strict for the `yaml` codec — `goccy/go-yaml`'s JSON-compatible decode turns an explicit JSON
      `null` cell into a `nil` `json.RawMessage` rather than a literal `RawMessage("null")`. Both
      marshal back to the same JSON `null` (`json.RawMessage.MarshalJSON` returns `"null"` for a nil
      receiver too), so the test asserts equality via re-marshaled JSON (`assert.JSONEq`) rather than
      `assert.Equal` on the structs — a representational difference in the codec, not a bug in
      `Decode`/`RenderTable`.
- [x] Run tests — must pass before next task.

### Task 6 — Command wiring, table codec, and root registration

**Files:**
- Create: `cmd/grafanapi/explore/command.go`
- Create: `cmd/grafanapi/explore/table.go`
- Create: `cmd/grafanapi/explore/command_test.go`
- Modify: `cmd/grafanapi/root/command.go` (register `explore.Command()`)

- [x] Implement `Options` + `BindFlags` (register/`DefaultFormat` the `table` codec, bind `-o`,
      `--field`, `--param`, `--from`, `--to`, `--max-data-points`, `--interval`, `--instant`) +
      `Validate` (IO + `key=value` params + parseable `--interval`).
- [x] Implement `Command()` (`ExactArgs(2)`, `Example`, `cmdconfig.Options` for `--config`/`--context`)
      and the RunE orchestration (LoadConfig → GetCurrentContext → ClientFromContext →
      ResolveDataSource → QueryFieldForType/BuildQuery → Run → Encode).
- [x] Implement `tableCodec` (`format.Codec`) delegating `Encode` to `explore.RenderTable` and
      returning an unsupported-operation error from `Decode`; register `explore.Command()` in
      `root/command.go` alongside `config`/`login`/`resources`.
- [x] Write command tests against an `httptest.Server` + fake `keychain.Store`
      (`cmdconfig.SetKeychainStore`) + temp config file: `explore <uid> "<query>"` prints a table;
      `-o json` prints JSON that decodes back to the response; a per-`refId` error exits non-zero.
      ⚠️ **Note (verified while implementing):** `cmdconfig` is the correct package alias for
      `cmd/grafanapi/config` (matches `cmd/grafanapi/resources`'s usage); `config.SetKeychainStore`
      (not a `cmdconfig`-prefixed name) is the actual exported test seam, and
      `testutils.NewFakeKeychainStore()` (already used by other packages) was reused instead of a
      new local fake. Test HTTP handlers use `assert` rather than `require` (`testifylint`'s
      go-require rule: `require` inside a handler goroutine only aborts the handler, not the test).
- [x] Run tests — must pass before next task.

### Task 7 — Verify acceptance criteria

**Files:**
- Modify: none (verification only; fix regressions in-place if found)

- [x] `make tests` (race) passes across all packages (fall back to `go test -race ./...` if `devbox`
      is unavailable, as prior plans did). `devbox` is not installed in this environment; ran `go test
      -race ./...` directly — all packages pass.
- [x] `make build` produces `./bin/grafanapi` on darwin/arm64 (`CGO_ENABLED=1`); `grafanapi explore
      --help` renders and shows all flags. Built directly via `go build -buildvcs=false -ldflags=...`
      (same flags as the Makefile) — produced a valid Mach-O arm64 binary; `--help` lists `explore`
      alongside `config`/`login`/`resources`; `explore --help` shows the full synopsis, examples, and
      all flags (`--config`, `--context`, `--field`, `--from`, `--to`, `--interval`, `--instant`,
      `--max-data-points`, `-o/--output`, `--param`).
- [x] `make lint` passes with **no new findings** versus the pre-existing 14-finding baseline (5
      gosec, 3 govet, 1 nolintlint, 5 staticcheck) recorded in the completed plans — diff
      `golangci-lint run -c .golangci.yaml ./...` output against that baseline; annotate any
      unavoidable new finding with a justified `//nolint` + comment and record it here. Ran
      `golangci-lint run -c .golangci.yaml ./...` directly (v2.12.2) — exactly 14 findings, same
      breakdown (5 gosec, 3 govet, 1 nolintlint, 5 staticcheck), all in pre-existing files
      (`internal/server/grafana`, `internal/server/handlers`, `scripts/*-reference`,
      `internal/httputils`, `cmd/grafanapi/fail`). Zero new findings in `internal/explore` or
      `cmd/grafanapi/explore`.
- [x] `goreleaser check` passes. Ran `goreleaser check` directly — "1 configuration file(s)
      validated".
- [x] **Reference docs (MANDATORY):** run `make cli-reference` (regenerates `docs/reference/cli`,
      adding `grafanapi_explore.md` and updating the index) plus `make env-var-reference` /
      `make config-reference`; then `make reference-drift` must pass with the regenerated files
      staged. If `devbox` is unavailable, run the underlying `go run scripts/cmd-reference/*.go
      ./docs/reference/cli` (and the env/config equivalents) directly. Ran all three generators
      directly (`go run scripts/cmd-reference/*.go`, `scripts/env-vars-reference/*.go`,
      `scripts/config-reference/*.go`). Resulting drift: new `docs/reference/cli/grafanapi_explore.md`
      page + a one-line addition to `docs/reference/cli/grafanapi.md`'s command index (the `explore`
      entry). No drift in `docs/reference/environment-variables` or `docs/reference/configuration`
      (the command adds no new env vars or config fields). These generated files are included in this
      task's commit, matching what `make reference-drift` expects staged.
- [x] Run the full suite once more (`go clean -testcache && go test -race ./...`) — must pass before
      the documentation task. Ran with a cleared test cache — all packages pass.

### Task 8 — Update documentation and complete the plan

**Files:**
- Modify: `README.md` (add an `explore` usage example)
- Create: `docs/guides/query-datasources.md` (a new, distinctly-named guide for the `explore`
  command) and update `docs/index.md`/`docs/guides/index.md` if they list guides/commands.
  **Do NOT reuse `docs/guides/explore-modify-resources.md`** — that existing guide is about
  *exploring/modifying Grafana resources* (dashboards/folders), a different concept; conflating the
  two would confuse readers.
- Modify: `AGENTS.md` (add `explore` to the command list and note the `internal/explore` package +
  the raw-query/lossy-model decision)
- Regenerate: `docs/reference/**` (already done in Task 7; re-confirm no drift)
- Move: this plan → `docs/plans/completed/20260722-explore-command.md` (via `git mv`)

- [x] Add a `README.md` example: `grafanapi explore <datasource> "<query>"` with a Prometheus and a
      SQL one-liner, and a `-o json | jq` piping example.
- [x] Add a new `docs/guides/query-datasources.md` guide (distinct from the existing
      `explore-modify-resources.md`): datasource resolution (uid-or-name), the type→field mapping
      table, `--field`/`--param`/`--from`/`--to`/`--interval`/`--instant`/`--max-data-points`, and
      output formats; note single-query scope and that CSV output is possible future work (json
      covers piping today).
- [x] Update `AGENTS.md`: add `explore` under the command groups; add the `internal/explore` package
      description; record that the query path decodes raw `/api/ds/query` JSON into local structs
      because the vendored `models.Frame`/`models.Field` are value-less and cannot round-trip the
      wire format, while datasource lookup uses the generated client.
- [x] Run `make reference` / `make reference-drift`; confirm zero drift (the new command's generated
      pages are committed).
- [x] Add a short Review section to this plan (what changed, deviations), then `git mv` it to
      `docs/plans/completed/`.
- [x] Run `make all` (lint, tests, build, docs) — must pass (fall back to the underlying commands if
      `devbox`/`mkdocs` are unavailable, matching prior plans).

## Review

All 8 tasks completed as planned; the design in Solution Overview and Technical Details matches
what shipped. Deviations, all discovered mid-implementation and recorded in-place at the time:

- **Task 2**: no `*datasources.GetDataSourceByNameNotFound` type exists in the vendored client — a
  `404` on name lookup falls through to a bare `runtime.NewAPIError`. `ResolveDataSource` detects it
  via `errors.As(err, &apiErr)` + `apiErr.Code == http.StatusNotFound` instead of a typed sentinel.
- **Task 5**: the `yaml` codec's JSON-compatible decode turns an explicit JSON `null` cell into a
  `nil` `json.RawMessage` rather than a literal `RawMessage("null")`; both marshal back to the same
  JSON `null`, so the round-trip test asserts via `assert.JSONEq` instead of `assert.Equal` on the
  decoded structs. Not a bug in `Decode`/`RenderTable`, just a representational difference in the
  codec.
- **Task 6**: `config.SetKeychainStore` (package `cmd/grafanapi/config`, imported as `cmdconfig`) is
  the real exported test seam, and the already-existing `testutils.NewFakeKeychainStore()` was
  reused rather than writing a new local fake. Handler-goroutine assertions use `assert` rather than
  `require` per `testifylint`'s go-require rule.

Task 8 (this task) added:

- `README.md` — a "query a datasource" example section (Prometheus, SQL, `-o json | jq`).
- `docs/guides/query-datasources.md` (new; kept distinct from `explore-modify-resources.md`, which
  covers a different concept — exploring/modifying Grafana *resources*, not datasource queries) plus
  a card in `docs/guides/index.md`.
- `AGENTS.md` — `explore` added as a fourth command group, an `internal/explore/` package
  description (all five files + the raw-HTTP/lossy-model decision), and a `cmd/grafanapi/explore/`
  line under `cmd/grafanapi/`.
- Reference docs re-regenerated (`cli`, `environment-variables`, `configuration`) to confirm zero
  drift from the Task 7 state — no command/flag/config changes happened in this docs-only task, so
  `git status` on `docs/reference` was empty after regenerating.
- Verification re-run for this task: `go build ./...` clean; `go test -race ./...` all packages pass
  (cached, since no Go source changed); `golangci-lint run -c .golangci.yaml ./...` still exactly 14
  findings (5 gosec, 3 govet, 1 nolintlint, 5 staticcheck), same breakdown as the Task 7 baseline,
  all in the same pre-existing files — zero new findings. `devbox`/`mkdocs` remain unavailable in
  this environment, so `make all`'s `docs` target (which needs `mkdocs build`) could not be run
  as-is; ran its constituent parts directly instead (`go build`, `go test -race`, `golangci-lint`,
  and the three reference-generation scripts), matching the fallback the prior plans used.

## Post-Completion

These require the user and cannot be safely automated by the implementation agents. **Never record a
real hostname, cookie value, or organization/company name** — use `$GRAFANA_TEST_SERVER` and "a test
datasource" throughout.

- **Live end-to-end verification (real Grafana, generic):**
  1. `grafanapi login --server "$GRAFANA_TEST_SERVER"` → paste the current browser `grafana_session`
     (ideally from a private/incognito window, per the auth docs) → expect success.
  2. Prometheus-style: `grafanapi explore <a test Prometheus datasource> "up"` → expect a table with
     a time column (RFC3339) and one row per series with labels.
  3. SQL-style: `grafanapi explore <a test SQL datasource> "select 1 as n"` → expect a one-row table;
     confirm `rawSql` + `format:"table"` were used (server returns a table frame).
  4. Piping: `grafanapi explore <a test datasource> "<query>" -o json | jq '.results.A.frames[0].schema'`
     → expect valid JSON with the schema fields intact (proves decoding is lossless).
  5. Flags: exercise `--from`/`--to` (e.g. `--from now-6h`), `--instant` against Prometheus,
     `--param` (e.g. a datasource-specific option), and `--field` on an otherwise-unknown type.
  6. Not-found: `grafanapi explore does-not-exist "up"` → expect the friendly "datasource not found"
     error listing available datasources (name/uid/type).
  7. Stale session: after the session truly dies (logout/revoke), rerun an `explore` command → expect
     the **"Grafana session is stale or unauthorized"** error with the `Run: grafanapi login update`
     suggestion (exit code 2) — confirming the dead-session `401` surfaces as a `*runtime.APIError`
     (rendered by `staleSessionError`), NOT the "Grafana rejected the provided session cookie" /
     "Verify the session cookie value" message (which is reserved for the login prompt).
- **Keychain "Allow" prompt:** the first Keychain access after each rebuild triggers the macOS
  "Allow / Always Allow" dialog — choose "Always Allow" for the installed binary.
