# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

**grafanapi** is a command-line tool for managing Grafana resources through the REST API. It supports Grafana 12 and above, enabling users to authenticate, manage multiple environments, and perform administrative tasks from the terminal. The tool is particularly useful for dashboards-as-code workflows and CI/CD automation.

## Development Environment

This project uses **devbox** for consistent development environments:

```bash
# Enter devbox shell (includes all required tools)
devbox shell

# Run one-off commands within devbox
devbox run go version

# Add packages
devbox add go@1.24
```

## Common Commands

### Build and Test

```bash
# Build the binary to bin/grafanapi
make build

# Run all tests
make tests

# Run tests for a specific package
go test -v ./internal/config

# Install to $GOPATH/bin
make install

# Run linter
make lint

# Run all checks (lint, tests, build, docs)
make all
```

### Documentation

```bash
# Generate and build all documentation
make docs

# Generate CLI reference documentation
make cli-reference

# Generate environment variable reference
make env-var-reference

# Generate configuration file reference
make config-reference

# Serve documentation locally with live reload
make serve-docs

# Check for drift in generated documentation
make reference-drift
```

### Dependency Management

```bash
# Vendor dependencies
make deps

# Clean build artifacts and dependencies
make clean
```

## Architecture

### Command Structure

grafanapi follows the Cobra command pattern with eight main command groups:

1. **login**: Authenticate to a Grafana instance using a session cookie
   - `login`: Prompt for server + session cookie, validate, persist context + Keychain entry
   - `login update`: Refresh a stale cookie for an existing context (server not re-prompted)

2. **config**: Manage configuration contexts for connecting to Grafana instances
   - `config set`: Set configuration values
   - `config unset`: Unset configuration values
   - `config use-context`: Switch between configured contexts
   - `config list-contexts`: List all configured contexts
   - `config current-context`: Show the current context
   - `config view`: View the current configuration
   - `config check`: Validate the configuration

3. **resources**: Manipulate Grafana resources (dashboards, folders, etc.)
   - `resources get`: Get resources from Grafana
   - `resources list`: List resources
   - `resources pull`: Pull resources from Grafana to local files
   - `resources push`: Push local resources to Grafana
   - `resources delete`: Delete resources from Grafana
   - `resources edit`: Edit resources interactively
   - `resources validate`: Validate resource manifests
   - `resources serve`: Serve resources locally with live reload (binds 127.0.0.1 by default; the
     printed URL carries a per-run access token)

4. **explore**: Run a single ad-hoc query against a Grafana datasource (mirrors Grafana's Explore UI)
   - `explore DATASOURCE QUERY`: resolve a datasource (UID, then name), map the query string onto
     the type-appropriate request field (`expr`/`rawSql`/`target`/`query`, override with `--field`),
     POST it to `/api/ds/query`, and render the result as a table (default) or `json`/`yaml`

5. **datasources**: List the datasources configured on the current context
   - `datasources`: GET `/api/datasources`, sort by name, and render as a table (default,
     NAME/UID/TYPE/DEFAULT columns) or `json`/`yaml`

6. **install-skill**: Install the bundled Claude Code skill
   - `install-skill`: Write the embedded `skill/grafanapi` tree to `<--to>/skills/grafanapi`
     (default `--to ~/.claude`), replacing any existing installation. No config/auth dependency.

7. **session**: Keep a context's Grafana session alive proactively, on a schedule, opt-in per
   context via a `live-window` configuration field
   - `session refresh`: force a rotation now, reusing the exact `SessionSource` rotate +
     Keychain-persist path as automatic 401-triggered rotation; unconditional by default
     (`--context`/current context, or `--all` for every context with a stored cookie); `--due`
     restricts the target set to contexts whose `live-window` has elapsed since their last
     rotation (the scheduler entry point) — mutually exclusive with `--all`/`--context`
   - `session keepalive install`: write and load a macOS launchd LaunchAgent that periodically runs
     `session refresh --due`; errors if no context has `live-window` set; `--interval` overrides the
     derived wake interval (`min(live-window)/2`, clamped `[15m, 12h]`), itself bounded `[1m, 6d]`
   - `session keepalive status`: report installed/loaded, interval, target binary, and a tail of the
     keep-alive log (never contains a cookie)
   - `session keepalive uninstall`: boot the LaunchAgent out and remove its plist, idempotently

8. **alerts**: Inspect **and manage** Grafana Unified Alerting. Reading needs the Viewer role,
   writing needs Editor; no command in the group performs an org-admin operation (no
   `ResetPolicyTree`, no `decrypt=true`, no contact-point delete, no pruning). Every parent in the
   group (`alerts`, `alerts rules`, `alerts contact-points`) has no `RunE`, so a bare invocation
   prints help and exits 0; `alerts` carries `--config`/`--context` as persistent flags. The
   `contact-points` subgroup is still read-only (`list` only).
   - `alerts rules list`: join rule **definitions** (`GET /api/v1/provisioning/alert-rules`, via the
     generated client) with live **evaluation state**
     (`GET /api/prometheus/grafana/api/v1/rules`, raw HTTP) on rule `uid`, so one command answers
     both "what is defined" and "what is firing". Renders
     FOLDER/GROUP/TITLE/UID/TYPE/STATE/HEALTH/FOR/PAUSED/ALERTS as a table (default) or
     `json`/`yaml`. FOLDER is the folder's **full path** (`Parent/Child`), which is what the state
     endpoint's `file` field carries — per rule for a rule it lists, and per *folder*
     (`folderUid` → `file` on each group) for one it does not, so an unevaluated rule still shows a
     FOLDER and is still reachable by `--folder <path>`; the residual gap is a folder with **no**
     group in the state response at all (every rule in it unevaluated), which has no path anywhere
     in the payload and stays UID-only. Client-side filters `--folder` (folder UID, full path, **or**
     the path's last segment — exact and case-sensitive; a parent never matches its children, while
     the last-segment form matches the folder *title* and therefore spans every folder with that
     title, `Reports` selecting both `A/Reports` and `B/Reports`) and
     `--state` (single value, trimmed/lowercased, `normal` aliased to `inactive`, matched
     case-insensitively against the endpoint's verbatim state, validated by the
     one `alerts.ValidateState` both layers call, against `alerts.KnownStates()` =
     `firing|pending|recovering|inactive|unknown|recording|unavailable`). Both filters depend on the
     state call: if it fails (non-401) they **fail the command** instead of degrading — the two
     exceptions are `--state recording` and `--state unavailable`, the only values decided without
     the state view. See `stateDependentFilter`. A degraded run stamps STATE `unavailable` (never
     `unknown`) on every alerting rule, which is the **machine-readable** signal that the state half
     was never fetched — the warning goes to stderr, invisible to `-o json | jq`
   - `alerts contact-points list`: `GET /api/v1/provisioning/contact-points` via the generated
     client, sorted by name (UID as tie-break — Grafana allows duplicate names), rendered as
     NAME/TYPE/UID/PROVENANCE or `json`/`yaml`. Contact points are **org-scoped**, not
     folder-scoped. Grafana redacts each type's **schema-declared secure fields** as
     `"[REDACTED]"` server-side — a credential embedded in a webhook URL or a custom header is not
     a secure field and comes back verbatim, so no doc may call `settings` secret-free; it is a
     json/yaml-only field
   - `alerts pull`: Grafana → disk. Writes a **bundle** (`rules/<folder UID>/<group title>.<ext>`,
     `contact-points.<ext>`, `policies.<ext>`) via `internal/alerts.Pull` + `WriteBundle`
     (`WriteOptions{Format, KeepFolders}`, whose `KeepFolders` distinguishes nil from empty exactly
     as `PullOptions.Groups` does). Flags
     `-p/--path` (default `./alerting`), `--folder-uid` (repeatable `StringArrayVar`, UID only,
     unknown UID → error), `-o/--output yaml|json` (the **file** format; the table codec is
     deliberately not registered, so `-o table` is rejected), `--max-concurrent` (default 10).
     Stale files under `rules/` that this pull did not produce are removed (restricted to the
     `--folder-uid` set when given); `contact-points`/`policies` are rewritten, and a nil one
     writes **no** file. Output: one `cmdio.Success` summary
     (`%d rule group(s), %d contact point(s) and %d policy tree(s) written to %s`)
   - `alerts push`: disk → Grafana, via `internal/alerts.ReadBundle` → `Push` (which itself calls
     `Pull` for the local groups only, then `Plan`). Flags `-p/--path` (a **single** `StringVarP` —
     a bundle is one tree, unlike `resources push`'s repeatable `-p`), `--dry-run`,
     `--ui-editable` (**creates only**; an update always mirrors the stored provenance),
     `--on-error ignore|fail|abort` (via `onerror.BindFlag`), `--max-concurrent`. `--dry-run`
     renders the plan table (`ACTION KIND ID DETAIL` plus `N to create, M to update, K unchanged`)
     and prints the refusals an apply would hit as `✘ would fail: …` lines below it (see
     `internal/alerts/push.go`); an apply prints **one `cmdio` line per change**
     (`✔ Update RuleGroup folder-uid-1/probe-group`, `✘ … : <err>`) plus
     `%d change(s) applied, %d error(s)` — a deliberate divergence from `resources push`'s single
     summary line (few objects, large blast radius each). `runPush` **reports before it returns**
     `Push`'s error whenever the push reached an object, exactly as `runRuleOp` does: under
     `--on-error abort`, or a 401 found halfway through, the contact points and rule groups already
     written must still be on screen. The push's own error then wins over the report's aggregate.
     The report is **conditional** (`pushErr == nil || len(Applied)+len(Failed) != 0`) because a
     push that failed *before* applying anything — a 401 on the fetch `Plan` needs, an ambiguous
     contact-point name — has a zero `Result`, which rendered as `✔ 0 change(s) applied, 0 error(s)`
     (or a bare plan header under `--dry-run`) above the error that actually happened. The exit code comes from
     `onerror.Mode.FailOnErrors()` (a **predicate**, no argument), and the error **wraps the first
     failure** so a 403 still renders as permission-denied
   - `alerts rules pause|resume|delete UID...`: `cobra.MinimumNArgs(1)`, `--dry-run`, **no
     `--force`** (every rule is named on the command line, matching `resources delete`'s
     named-selector precedent). `pause`/`resume` are one constructor
     (`rulesPauseCmd(configOpts, paused bool)`) over one write — Grafana has no pause endpoint, so
     `isPaused` is flipped by rewriting the whole rule group (`PutAlertRuleGroup`), which is the
     only form that persists for an Editor on 12.3.x. Output is one line per uid,
     `<uid> (<title> in <group>) <outcome>` where the outcome is `paused`/`resumed`/`deleted`,
     `already paused`/`not paused`, or the same words prefixed `would be` under `--dry-run`, closed
     by `%d rule(s) %s, %d unchanged, %d error(s)`

### Core Packages

**cmd/grafanapi/** - CLI command implementations
- `root/`: Root command setup with logging and flags
- `login/`: `login` / `login update` — interactive session-cookie authentication
- `config/`: Configuration management commands
- `resources/`: Resource manipulation commands
- `onerror/`: the shared `--on-error` flag — `Mode` with the constants `Ignore`/`Fail`/`Abort`,
  `Validate`, `BindFlag(flags, *Mode)`, `StopOnError()` and `FailOnErrors()`. Extracted from
  `cmd/grafanapi/resources/onerror.go` (deleted, no alias) so `resources` and `alerts` share one
  implementation. `FailOnErrors()` is a **bool predicate**, not a `FailOnErrors(n) error`
  constructor — every caller counts its own failures and builds its own error. The flag help text
  and the doc comments both say **"items"**, not "resources": the flag is on `alerts push` too,
  where the items are contact points, a policy tree and rule groups. The generated CLI reference is
  regenerated from source, so the wording change is a diff on the `resources` pages, not drift
- `explore/`: `explore` — thin Cobra wiring (flags, a `table` `format.Codec` adapter) over
  `internal/explore`; no domain logic lives here
- `datasources/`: `datasources` — thin Cobra wiring (flags, a `table` `format.Codec` adapter)
  calling the generated Grafana client directly; no domain logic to extract for a single list call.
  Its table cells go through `termsafe.Cell` (name/UID/type are server-controlled); json/yaml stay raw
- `installskill/`: `install-skill` — writes the `skill.Files` embedded FS to `<--to>/skills/grafanapi`
  (`os.RemoveAll` then `fs.WalkDir`, directories `0o750`/files `0o600`); no config/auth dependency
- `session/`: `session refresh` / `session keepalive install|status|uninstall` — a **standalone**
  top-level command (like `login`), owning its own `--config`/`--context` persistent flags plus
  package-level `keychainStore`/`controller` test seams (`SetKeychainStore`/`SetController`), since
  `refresh --all`/`--due` resolve *every* context themselves rather than just the current one.
  `refresh.go` holds the pure `dueContexts(cfg, now, modAt)` selector (unset `live-window` → skip;
  set-but-invalid → warn+skip, never a hard error; `keychain.ErrNotFound` → skip; otherwise select
  iff `now.Sub(lastRotation) >= window`) and maps a rejected rotation to
  `runtime.NewAPIError(..., 401)` for the standard exit-2 stale-session rendering, exactly as the
  completed `explore` plan did for its own dead-session path. `keepalive.go` derives the LaunchAgent
  spec from `internal/launchd` and shells out only through the `Controller` seam — never real
  `launchctl` in tests.
- `alerts/`: `alerts rules list|pause|resume|delete`, `alerts contact-points list`, `alerts pull`,
  `alerts push` — thin Cobra wiring over `internal/alerts`, mirroring `datasources`/`explore`:
  flag binding, a fast pre-network `--state`
  check that calls the same `internal/alerts.ValidateState` `ListRules` uses (so the message and
  the accepted set have one source, while the domain function stays safe for non-Cobra callers),
  and two `table` `format.Codec` adapters. `table.go` holds the shared seam both codecs use — a
  single `tableFormat` const (the one source for both `RegisterCustomCodec` and `DefaultFormat`),
  `newTableWriter` (the one tabwriter configuration), the `cell` sanitizer and the `errTableDecode`
  sentinel — which is why `dupl` does not fire on the second codec. Every string cell goes through
  `cell`, which replaces control **and** Unicode format (`Cf`) characters with a space: a folder
  title or rule name is arbitrary user input, one embedded tab or newline is read by the tabwriter
  as a column or row terminator (silently shifting or splitting the row), and a `Cf` bidi override
  makes a terminal re-order the rest of the line without changing a byte of the layout (the
  json/yaml codecs keep the raw value). `cell` delegates to `termsafe.Runes` (byte-identical to
  the predicate it used before the extraction).
  `newTableWriter` deliberately drops the
  `tabwriter.TabIndent` the repo's other table renderers pass: that flag pads a *leading* empty
  cell with raw tabs, and FOLDER is empty for every row in degraded mode, which would shift whole
  rows to the terminal's next tab stop. No domain logic lives here.
  The write half adds `pull.go`, `push.go`, `ruleops.go`, `plan_table.go` and `report.go`.
  `report.go` holds `summaryPrinter(succeeded, failed)`, the one ladder that picks
  `cmdio.Success`/`Warning`/`Error` for a closing summary line (clean → success, nothing achieved →
  error, partial → warning); `reportPush` and `reportRuleOp` share it and keep only their own
  wording and their own exit-code rule (`push` gates the returned error on `--on-error`, the rule
  ops always return one). `plan_table.go` is a
  **plain renderer, not a registered codec** (`push` has no `-o`, so there is no format to select
  and nothing to decode back); it reuses `table.go`'s `newTableWriter`/`cell` seam and puts a
  change's first detail line on the change's own row, the rest on indented follow-on rows.
  The `✔`/`✘` **result** lines of `reportPush`, `reportRefusals` and `reportRuleOp` are scrubbed as
  **whole lines** — built with `fmt.Sprintf`, passed through `cell`, emitted as
  `cmdio.X(out, "%s", scrubbed)` — never field by field. `cmdio.Success/Warning/Error` scrub
  nothing, and the same untrusted group title, folder UID or contact-point name that is the plan ID
  is quoted back inside the error rendered beside it, so scrubbing only the ID would still let a
  newline in one of them open a forged `✔ …` line in the output a human reads before approving a
  destructive push. `ruleOpLabel` therefore returns its parts **unscrubbed**: the line it ends up
  in is the unit that has to be safe.
  `ruleops.go` holds one `ruleOperation{verb, done, skipped, short, action, gerund, paused, apply}`
  value per operation — **one** struct, carrying the help wording too — so `runRuleOp`/`reportRuleOp`
  are written once for all three and `pause`/`resume` are one constructor switching on a bool (the
  three wording fields are empty for `delete`, whose help is written out in full at its own
  command). Its two constructors are `newDeleteOperation()`/`newPauseOperation(paused)` — `new`-
  prefixed so they cannot be mistaken for `internal/alerts`' `deleteOperation`/`pauseOperation`
  **constants**, which are the words those operations name themselves with in an error. `apply` is
  the internal function itself in all three (`alerts.DeleteRules`, `alerts.SetRulesPaused`), with
  `paused` carried on the struct and `runRuleOp` building the one `alerts.RuleOpOptions` they share;
  there is no adapter closure. The write commands have **no `Validate()`** on
  the rule ops (positional uids Cobra already counted plus a bool flag); `pull`/`push` keep
  `resources`' own flag validation and byte-identical messages (`--path is required`,
  `max-concurrent must be greater than zero`)
- `fail/`: Error handling and detailed error messages. `DetailedError.Error()` scrubs `Summary`,
  `Suggestions` and `DocsLink` with `termsafe.Cell` and a non-yaml `Parent` with `termsafe.Text`; a
  `yaml.Error` parent is scrubbed only when colour is off (with colour on, stripping would remove the
  legitimate highlighting). `Details` is **never** scrubbed: every producer is local and
  `ValidationError.AnnotatedSource` is intentionally coloured. Only errors *returned* by a command pass
  through here; `config check` and `session refresh` print some error text directly
- `io/`: Output formatting and user messages

**skill/** - Embedded Claude Code skill (repo root, not under `internal/` or `cmd/`)
- `//go:embed grafanapi` embeds `skill/grafanapi/SKILL.md` into `skill.Files` (an `embed.FS`) at
  build time, so the installed binary can write it from any directory regardless of source
  checkout — consumed only by `cmd/grafanapi/installskill`
- `grafanapi/SKILL.md`: the Claude Code skill content itself (frontmatter `name`/`description`
  plus usage docs); edit this file directly to change what `install-skill` writes

**internal/config/** - Configuration management
- Context-based configuration (similar to kubectl contexts)
- Support for multiple Grafana instances (contexts)
- Authentication: Grafana session cookie (`grafana_session`), resolved from the macOS Keychain at load time — never a config-file field or env var
- Environment variable overrides for non-secret fields (GRAFANA_SERVER, GRAFANA_ORG_ID, GRAFANA_STACK_ID)
- Automatic Stack ID discovery for Grafana Cloud
- TLS configuration support
- `session_source.go`: `SessionSource` — a shared, mutable session cookie holder (mutex +
  generation counter) built once per context at credential-resolution time when a cookie is
  loaded from the Keychain. `Rotate()` POSTs `/api/user/auth-tokens/rotate` with the current
  cookie, extracts the new `grafana_session` from `Set-Cookie`, and re-persists it to the
  Keychain; the generation counter deduplicates concurrent *and* sequential rotate attempts so
  only one network rotation happens per staleness event. `(GrafanaConfig).WrapWithSession` is the
  single helper every transport (k8s, openapi, serve) calls to get a `rotatingRoundTripper` (when
  a `Session` is present), the older static `sessionCookieRoundTripper` (cookie without a source,
  e.g. tests), or the underlying transport unchanged (no cookie). On a truly-dead session (rotate
  itself gets `401`/`403`), the original `401` is returned unchanged so the existing
  `cmd/grafanapi/fail` stale-session rendering fires — rotation is purely a transparent retry
  layer in front of that path. Bootdata/stack-ID discovery (`stack_id.go`) is intentionally
  **not** wired into rotation (pre-auth, single non-retryable probe, fails soft already).
  `Refresh(ctx) (string, error)` (added for `session refresh`) is a 3-line force-now wrapper —
  `Current()` → `Rotate(ctx, gen)` — that reuses `Rotate`'s single-flight join instead of
  duplicating `doRotate`/`persist`; it lives in `internal/config` (not `internal/session`) to avoid
  an import cycle, so only the `session` **command** package needs both packages.
- `types.go`: `GrafanaConfig.LiveWindow` (`yaml:"live-window,omitempty"`, no env tag) is an optional,
  persisted `string` opting a context into scheduled keep-alive — presence, not a boolean, signals
  opt-in; a `ParsedLiveWindow()` helper centralizes `time.ParseDuration` + bounds-check `[1m, 6d]`
  (`Validate` rejects a set-but-invalid value; unset always passes; `IsEmpty()` treats it like any
  other persisted field, i.e. it does **not** get zeroed the way `SessionCookie`/`Session` do).

**internal/explore/** - Ad-hoc datasource query domain logic (no Cobra)
- `dataframe.go`: wire structs (`QueryResponse`/`FrameResult`/`Frame`/`FrameSchema`/`FieldSchema`/
  `FrameData`) matching Grafana's real `/api/ds/query` JSON-dataframe shape, plus `Decode`,
  `Frame.RowCount`/`Cell`, and `(*QueryResponse).FirstError`
- `datasource.go`: `ResolveDataSource` — UID lookup, then name lookup, then a "not found" error
  listing every configured datasource — via the generated openapi client
  (`grafana.ClientFromContext`)
- `query.go`: `QueryFieldForType`/`BuildQuery` — datasource-type → request-field mapping
  (`expr`/`rawSql`/`target`/`query`), `--param`/`--interval`/`--instant` handling
- `run.go`: `Run` — raw `POST /api/ds/query` over `httputils.NewTransport` wrapped with
  `GrafanaConfig.WrapWithSession` (so a `401` rotates-and-retries like every other authenticated
  path), decode, and per-`refId` error surfacing
- `render.go`: `RenderTable` — `text/tabwriter` rendering for the default `table` output format.
  Cells, headers, frame names and refIDs go through `termsafe.Cell` **before** `truncate`, which
  measures runes (so a dropped sequence cannot make a cell look longer than it renders)
- **Deliberate raw-HTTP decision**: the query path decodes the raw `/api/ds/query` response body
  into these local structs instead of calling the generated client's
  `Datasources.QueryMetricsWithExpressions`, because the vendored `models.Frame`/`models.Field`
  have no `schema`/`data` split and no values field at all — the go-openapi consumer discards every
  column value (`data.values`) while decoding, so its payload cannot be re-marshalled into the real
  wire shape. Datasource **lookup** still goes through the generated client, since its response
  models (`models.DataSource`) round-trip correctly; only the query response is lossy.

**internal/alerts/** - Unified Alerting domain logic (no Cobra) — reading (`rules.go`, `state.go`,
`contactpoints.go`) and writing (`files.go`, `provenance.go`, `pull.go`, `diff.go`, `push.go`,
`ruleops.go`), over two files both write paths share: `body.go` (the write-body projections) and
`errors.go` (the error vocabulary). The package **never imports a `cmd/` package**: like
`remote.PushRequest`, `Push` takes a `StopOnError bool` and the cmd layer owns the `--on-error`
mode.
- `rules.go`: carries the package doc. `Rule` — the single flat output shape for all three formats
  (explicit json **and** yaml tags), `ruleFromProvisioned` (every `models.ProvisionedAlertRule`
  deref is nil-guarded), `formatDuration` (trims only *whole* trailing components, so `1m0s`→`1m`
  and `1h0m0s`→`1h` but `10s` stays `10s`),
  `KnownStates()`/`NormalizeState`/`ValidateState` (the single `--state` validation path),
  `ListRulesOptions`, `stateDependentFilter` (the degrade-or-fail decision: `--folder` in any form,
  or a `--state` value other than `recording`/`unavailable`, cannot be answered without the state
  view, so a non-401 state failure is fatal for it — `unknown` is deliberately **not** exempt, see
  the two-value rule below), `folderLeaf` (last unescaped-`/` segment of a folder
  path — a *title* match, so it spans folders sharing a leaf title; undoes Grafana's
  `\/` escaping — Grafana does **not** escape a literal backslash, so `bs\/Child` is irreducibly
  ambiguous between the folder `bs\` and a root folder `bs/Child`; it is read as the latter, which
  the doc comment and the `TestListRules_FolderFilterNestedPath` cases pin),
  `applyState(states, unjoined)` (merges the rule's `RuleStates.ByUID` entry, then falls back to
  `RuleStates.FolderPaths[r.FolderUID]` when that entry carried no folder path — the join-miss case,
  which is what keeps FOLDER filled and `--folder <path>`/`<leaf>` working for a rule the state view
  never listed; `TestListRules_FolderFilterMatchesUnjoinedRule` pins it and the residual UID-only
  case — `unjoined` is the state an unlisted alerting rule gets, `unknown` or `unavailable`, passed
  in by `ListRules` rather than hardcoded, which is what makes a degraded run visible in the
  output), and
  `ListRules` — provisioning defs → `FetchRuleStates` → join on uid → join folder paths on
  folderUID → filter →
  sort (folder/group/title, UID as final tie-break) → **never-nil** slice, so `-o json` emits `[]`
  rather than `null`. Optional output fields (`updated`, `lastEvaluation`) are `*time.Time` with
  `omitempty`, so a rule the server reported no timestamp for has no key rather than
  `0001-01-01T00:00:00Z`
- `state.go`: local wire structs for the `{status,data}` envelope of
  `GET /api/prometheus/grafana/api/v1/rules` plus `RuleState`, `RuleStates` and `FetchRuleStates` —
  raw HTTP over
  `httputils.NewTransport` wrapped with `GrafanaConfig.WrapWithSession` (rotate-on-401), bounded
  `stateTimeout`/`maxErrorBodySnippet`, `401` returned as an **unwrapped**
  `runtime.NewAPIError(..., 401)`. `FetchRuleStates` returns `RuleStates` — **two** maps, not just
  the uid-keyed one: `ByUID` (per-rule state) and `FolderPaths` (`folderUid` → `file`, one entry per
  group that carries both, so it resolves a folder path for a rule the response never listed). They
  are separate joins on purpose; a rule can need the second without appearing in the first.
  `ActiveAlerts` is an **exact scan of `alerts[]`**, counting
  instances whose state is not Normal after trimming Grafana's `" (reason)"` suffix (`"Normal
  (Error)"` is a Normal instance; an instance with no state at all counts as Normal too, since a
  malformed payload must not inflate a count the output cannot explain). The per-rule `totals` map is deliberately **not** decoded and
  the request deliberately sends **no** `limit_alerts`, even though `limit_alerts=0` would keep the
  response proportional to the rule count instead of the org's alert-instance count: `totals` is
  not a partition of the instance set, so no arithmetic over it is exact. Verified on Grafana
  12.3.2, one instance per rule — `execErrState: Error` → `alerts[].state "Alerting"`,
  `totals {alerting:1}`; `Error` → `"Error"`, `{error:1}`; `OK` → `"Normal (Error)"`,
  `{normal:1}`; `Alerting` → `"Alerting (Error)"`, `{alerting:1, error:1}`. `sum(totals) -
  totals[normal]` reads 2 for that last single active instance, while skipping the `error` key
  reads 0 for the second row's single active instance, and nothing in the payload distinguishes
  the two cases (`execErrState` is not reported). A non-`success` envelope surfaces the server's
  own `errorType`/`error` — either one on its own, not just both together — which is the only
  diagnostic degraded mode can show
- `contactpoints.go`: `ListContactPoints` via the generated client
  (`provisioning.NewGetContactpointsParams().WithContext(ctx)`), sorted by name then UID, non-nil
  and **nil-element-free** (the payload is a `[]*EmbeddedContactPoint`, so a `null` element would
  both panic the sort comparator and render as `null` in `-o json`/`-o yaml`)
- `files.go`: the on-disk bundle. `Bundle{Groups, ContactPoints, Policies}`, the layout constants,
  `sanitizeFileName`, `WriteBundle(dir, bundle, WriteOptions{Format, KeepFolders})` and
  `ReadBundle(dir)` (whose three parts are one helper each — `readGroupFiles`,
  `readContactPointsFile`, `readPolicyFile` — so the top level reads as the three files it is).
  Grafana's `…/export` format is **not importable** (the write endpoints reject it with a 400), so
  the on-disk shape mirrors the *write* models instead. Encoding goes through a **generic map**:
  `json.Marshal` the model, decode into `map[string]any`, delete the read-only keys
  `id`/`updated`/`provenance`, encode with the chosen `internal/format` codec — which is what
  makes the key set identical in YAML and JSON and keeps `updated` from leaking (`strfmt.DateTime`
  is a struct, so `omitempty` does nothing in `encoding/json` and a zero value would serialise as
  `"0001-01-01T00:00:00.000Z"`). Two details that are easy to undo by accident: the stripping is
  **targeted, not blanket-recursive** (a blanket walk would also delete a legitimate `id` inside a
  query's free-form `data[].model`), and `normalizeNumbers` re-types every decoded literal as
  `int64` when integral (plain `any` decoding makes everything a `float64`, which the YAML encoder
  writes as `interval: 600.0`; `json.Number` is worse — goccy quotes it). `orgID` is **kept** as
  fetched, since a nil would serialise as `orgID: null`; push overwrites it. A nil
  `ContactPoints`/`Policies` writes **no** file (a `null` file would read back as an empty object
  and push as a wipe), and the top-level files are removed in *every* recognised extension
  (`.yaml`, `.yml`, `.json`) before the current one is written, so `pull -o json` after
  `pull -o yaml` cannot leave two files disagreeing. `ReadBundle` decodes JSON with
  `DisallowUnknownFields` and YAML with the strict codec (so a typo'd key fails the same way in
  both), validates **structurally** (`validateGroup`/`validateRule`/`validateContactPoint`), and
  returns groups sorted by `folderUid` then `title`. A sanitised-name **collision** is an error
  naming both groups — never a silent overwrite
- **Bundle file names are compared folded, never byte-exact.** `foldFileName` =
  `strings.ToLower(norm.NFC.String(name))` (normalise *then* lowercase — lowercasing can itself
  change composition), and both `writeGroupFiles`' collision guard and `pruneGroupFiles`' staleness
  test key on it. grafanapi is macOS-only and APFS is case- and normalisation-insensitive, so
  `rules/F/Alpha.yaml` and `rules/F/alpha.yaml` are **one** directory entry: keying on the exact
  path silently dropped one of two groups titled `Alpha`/`alpha` (and of two folder UIDs differing
  only in case), and — worse — made a re-pull of a group re-titled `Alpha` → `alpha` write *into
  the existing entry* while `os.ReadDir` still reported `Alpha.yaml`, so prune deleted the file it
  had just written and left the folder empty with `alerts pull` reporting success. The collision
  error names both groups *and* both folders and says names are compared case-insensitively.
  `golang.org/x/text/unicode/norm` was already vendored (and already `## explicit` in
  `vendor/modules.txt`), so this added no dependency
- **Rule uids are unique across the whole bundle.** `validateGroup` refuses a uid repeated inside
  one group (the body would carry the rule twice, and `Plan` — which pairs by uid — would match
  both entries to the one remote rule and report a re-order that is really a duplicate);
  `declareRuleUIDs` threads a uid→file map through `readGroupFiles` and refuses a uid claimed by
  two files, naming both (a uid addresses one rule in the whole instance, so whichever group is
  written second takes the rule out of the first). Uid-less rules are skipped — Grafana assigns the
  uid on create, so a bundle may hold any number of them
- **`folderUID`, `ruleGroup` and `orgID` are optional on read**, even though the provisioning API
  requires all three: `ruleGroupBody` overwrites a rule's `folderUID`/`ruleGroup` from the group
  file that addresses it, and `resolveOrgID` supplies the organization. Requiring them at
  `ReadBundle` refused the hand-written rule the guide shows and made `orgID` — a field the guide
  tells users *not* to edit — mandatory in every file. `validateRule` therefore requires only
  `title`, `condition`, `data`, `execErrState`, `noDataState` and `for`. The one case push cannot
  fill in is a **new** group on a context whose `org-id` is 0 (Grafana Cloud, which addresses a
  stack) whose rules carry no `orgID` of their own: `requireOrgID` refuses it, failing that group
  by name before the `--dry-run` return, rather than sending a body Grafana answers with an opaque
  400. **`Plan` has to agree with that**, which is why `ruleDetails` compares each local rule
  projected through the same `pinRule` (see the `diff.go` bullet) — an optional field the diff
  compared as written would be a change no push could close
- **Rule groups, rules and contact points are deliberately *not* checked with their generated
  `Validate(strfmt.Default)`** (only `models.Route` still is — it declares no enum, so nothing
  there can go stale). The generated validation pins enums to the swagger the vendored client was
  generated from: `noDataState`/`execErrState` predate Grafana 11's `KeepLast`, and the
  contact-point `type` enum predates webex/sns/mqtt/oncall. Since `alerts pull` writes those
  values out verbatim, validating against the enums made a bundle that could be pulled and never
  pushed back — and one such rule anywhere failed the *whole* bundle at `ReadBundle`. The
  replacements check only what a write cannot be built without (required pointers non-nil,
  `folderUid`/`title` non-empty, `interval > 0`, non-empty `data`, a condition), which is the
  generated required-field set minus the enums; which values exist is the server's business, and a
  rejection from Grafana names the field. Do not "fix" this by re-adding `Validate`
- `provenance.go`: the three-way classifier behind every write. Stored `""` → send
  `X-Disable-Provenance: true`; stored `"api"` → omit the header; **anything else** (`file`,
  `converted_prometheus`, `disabled`, unknown) → refuse the write. Exported as
  `ProvenanceHeader(stored)` (update/delete), `CreateProvenanceHeader(uiEditable)` (creates only)
  and `GroupProvenanceHeader(rules)`, over three unexported helpers:
  `disableProvenanceHeader(disable)` (the `*string` the generated client takes — `&"true"` or nil),
  `foreignProvenanceError(stored)` (the single wording for "belongs to another provisioner") and
  `groupProvenance(rules)` (a group's per-rule values reduced to the one its write can be sent
  under). `CreateProvenanceHeader` is `disableProvenanceHeader`'s body under a name: it exists so
  the create-time choice reads as one at its call sites, since the same bool means something else
  on the update path. The refusal errors are **unprefixed**
  (`provenance "file" is managed outside the API`, `rules carry mixed provenance ("api" and "")`)
  so callers wrap them as `alerts: <op> <kind> <id>: %w`. An empty rule group classifies as
  unclaimed, and a disagreeing one returns `("", err)` — every caller goes through
  `GroupProvenanceHeader`, which propagates the error rather than heading a write with the zero
  value
- `pull.go`: `Pull(ctx, gClient, PullOptions) (Bundle, error)`. `PullOptions.Groups` distinguishes
  **nil from empty**: nil discovers every group (list → distinct `(folderUID, ruleGroup)` →
  bounded errgroup of group fetches), a non-nil empty selection fetches none — which is what keeps
  a bundle holding only `contact-points.yaml` from pulling the whole instance. `FolderUIDs` are
  validated with `Folders.GetFolderByUID` (404 → `alerts: pull: folder <uid> not found`), since
  `GetAlertRulesParams` has no folder filter and an empty folder is otherwise indistinguishable
  from a missing one. `groupRefID(ref)` — `<folderUID>/<title>`, the one way a group is named in a
  plan ID and in every message about it — lives here, beside the `GroupRef` it renders, not in the
  writers that print it. **`asAPIError(operation, err)` is load-bearing**: the *folders* client's
  generated reader declares typed 401/403/404/500 responses, so a 401 from the folder check is a
  `*folders.GetFolderByUIDUnauthorized` and `cmd/grafanapi/fail`'s `errors.As` for
  `*runtime.APIError` would miss it — a stale session would exit 1 with an opaque message instead
  of exiting 2. Both the pull check and the push pre-check go through it. `MaxConcurrent <= 0`
  falls back to 10 (`errgroup.SetLimit(0)` deadlocks)
- `diff.go`: `Plan(local, remote Bundle) ([]Change, error)` — pure, deterministic, unit-tested.
  Actions are `Create|Update|Unchanged` **only** (no `Delete` until `--prune` exists), and the
  `Kind` strings sort into the apply order (`ContactPoint` < `PolicyTree` < `RuleGroup`), so one
  sort gives both a readable plan and the order push walks. Rules are matched by **uid**, so moving
  a rule within a group file is not reported against that rule; details read `+ <title> (<uid>)`,
  `- …`, `~ …` and `~ interval 60s→120s`. The order itself **is** a change, though: Grafana
  evaluates a group's rules in body order, so `sameRuleOrder` compares the uid sequences the two
  sides share (uids present on both, in order) and a pure re-order emits a single `~ rule order`
  line — without it the plan said `Unchanged`, no PUT was made, and the next pull reverted the
  file. Uid-less or dropped rules are excluded from the comparison: they change the sequence by
  definition and already have a `+`/`-` line. The premise the detail rests on — that a GET returns
  the rules in the order the last PUT sent — is **verified against Grafana 12.3**, not assumed:
  `GET /api/v1/provisioning/folder/{f}/rule-groups/{g}` and the flat
  `GET /api/v1/provisioning/alert-rules` both replay the PUT's sequence (Grafana persists it as
  `rule_group_idx`), stably across repeated GETs. A **uid-less** local rule whose title matches a remote rule the write would
  delete additionally emits `! <title> (<uid>) would be replaced, not updated; run alerts pull
  first` — the re-push-before-re-pull shape. It is a **guard, not a match**: `findRule` still keys on
  uid alone (matching on title would silently update the wrong rule after a rename), and `!` sorts
  before `+`/`-`/`~` so the warning heads the block it explains. `equalRedacted` treats a local
  `"[REDACTED]"` leaf as equal to whatever stands remotely (recursively, over the whole encoded
  contact point, arrays included) without ever letting the marker hide a **sibling** change; the
  policy tree is deep-compared ignoring `Provenance` recursively over `routes[]`. Contact-point
  **uniqueness is enforced on the resolved remote object**, not on the local IDs `ReadBundle`
  already checks: `{uid: cp-1, name: team}` and a uid-less `{name: team}` are two different plan
  IDs that match one remote point, and applying both would PUT that uid twice with no ordering
  between them — so `planContactPoints` errors naming both local entries. `equalContactPoints`
  compares **the body the push would send** (`contactPointBody(local, remote.UID)`), not the local
  file as written: a hand-written point may carry no `uid` and be matched by name, and comparing
  that file directly finds one key fewer than the remote object has, so an identical point planned
  as `Update` and was re-PUT on every single push. **Rules follow the same rule**, for the same
  reason: `ruleDetails` takes the two whole groups and compares `pinRule(local, group.FolderUID,
  group.Title, match.OrgID)` — the body `ruleGroupBody` would send — against the matched remote
  rule. `folderUID`, `ruleGroup` and `orgID` are pointers *without* `omitempty` in the vendored
  model, so a file omitting them (which `validateRule` allows, and the guide shows) marshals three
  `null`s, `dropEmptyValues` deletes the keys, and the local map came out three keys short of the
  remote — `~ <title> (<uid>)` on every plan and a group re-PUT on every push, clobbering
  concurrent edits and re-stamping provenance, with no edit that could ever close it. `pinRule` is
  the **single source** of that pinning, shared by the write and the plan; keep it that way. The
  four detail markers are `const`s (`replacedMarker`/`addedMarker`/`removedMarker`/`changedMarker`,
  with `ruleOrderDetail` built from the last), and the byte-order note that makes `!` head the block
  sits on that block — the lines are sorted, so the markers are load-bearing, not decoration
- `push.go`: `Push(ctx, gClient, local, PushOptions) (Result, error)` — `Pull` the local groups'
  remote counterparts → `Plan` → apply in order contact points → policy tree → rule groups. It
  holds `pushRun` and its phases **only**: what is shared with the other write paths lives in
  `body.go` and `errors.go` (below), so this file is not the package's helper drawer.
  Bodies are **copies** (`ruleGroupBody`/`contactPointBody`/`routeBody`), so `Push` never mutates
  the bundle `Plan` was computed from; each rule's `folderUID`/`ruleGroup` is pinned and `orgID`
  resolved remote → context (when non-zero; Cloud contexts have `org-id` 0) → file. The pinning
  itself is `pinRule`, which `Plan` projects local rules through as well so the two cannot drift. Folders are
  pre-checked per distinct UID and are **never created** (`folder <uid> does not exist; push
  folders first (grafanapi resources push)`). A 404 **and a 403** (`folder <uid>: permission
  denied`, a `*messageError` keeping the `*runtime.APIError` reachable) fail only that folder's
  groups; anything else there — a 401, a transport failure — is fatal. The 403 is per folder
  because Grafana's alerting permissions are: `folders:read` is granted per folder, so a bundle
  spanning ten folders routinely gets a 403 for the eight this session does not own, and ending the
  push over it would refuse the other two *and* suppress the `--dry-run` plan for them, which is
  precisely when a user is trying to find out which folders they may write.
  `StopOnError` runs the group PUTs **one at a time** (stopping at the first
  failure is only meaningful when the writes are ordered); the concurrent path uses a plain
  `errgroup.Group`, **not** `errgroup.WithContext`, because cancelling the siblings of a failed
  write would report as failures writes that never ran. Only a 401 propagates as the error that
  stops a push, which is also how it escapes `StopOnError=false` and keeps the exit-2 contract.
  Note that the policy tree is **never a real create** (`GetPolicyTreeOK` always allocates a
  `models.Route`), so `--ui-editable` never applies to it.
  **`--dry-run` runs the whole apply pipeline with every client call skipped**, rather than
  returning straight after `Plan`: the checks that decide *whether* a write is allowed are read-only
  (the folder pre-check, the provenance classification, `redactedFields`), so running them is what
  keeps a dry run from promising a `Create RuleGroup` the next apply deterministically refuses. The
  refusals land in `Result.Failed` and the cmd layer prints them as `✘ would fail: …` below the plan
  table, still exiting 0. `Push` also **clears `StopOnError` under `DryRun`** — stopping at the first
  refusal would hide the rest of the plan.
  **A create carrying `"[REDACTED]"` is refused** (`alerts: push contact point <id>: secret field(s)
  <paths> are redacted; set the real value(s) in the file before creating`): the marker means "keep
  the stored secret", and nothing is stored for an object that does not exist yet, so a POST would
  store the literal string. `redactedFields` walks the *encoded* point recursively and reports dotted
  paths (`settings.http_config.password`, `settings.headers[0].value`). The PUT-preserve behaviour is
  unchanged — this is the create-only exception, and it is what pull→push into a **different**
  instance hits.
  **`operationError` (`errors.go`) returns a `*messageError` wrapper on the 400 path, not a flattened
  `fmt.Errorf` string.** Substituting Grafana's `ValidationError` message for the generated
  client's own rendering is the point, but the response still has to be reachable through
  `errors.As`: that is how `cmd/grafanapi/fail` picks between the 401 and 403 renderings and how
  `record` recognises a dead session. Only the 400 responder carries a `*models.ValidationError`
  today, so nothing breaks in practice — which is exactly why a `%w` drop here would go unnoticed
  until a future responder does. `checkFolders` uses the same type for its 403 refusal, which is
  why it is `messageError` rather than `validationError`: the message replaces the rendering, the
  wrapped error keeps the status code reachable
- `body.go`: the write-body projections, shared by `push.go`'s phases, `ruleops.go`'s group PUT and
  `diff.go`'s plan — `pinRule`, `ruleGroupBody`, `contactPointBody`, `routeBody`, plus
  `resolveOrgID`/`requireOrgID`. Every one of them returns a **copy**, which is what lets `Plan`
  compare the body a push would send without the push mutating the bundle the plan was computed
  from. They are here rather than in `push.go` because none of them is push-run specific
- `errors.go`: the package's error vocabulary — the `ErrRuleNotFound` sentinel, the `messageError`
  wrapper (`Error`/`Unwrap`) and `operationError`/`validationMessage`. `operationError(operation,
  target, err)` is the **single renderer** of `alerts: <op> <target>: <err>`: every write path calls
  it rather than hand-building the same `fmt.Errorf`, which is what keeps the `ValidationError`
  substitution and the `errors.As`-reachable response on all of them at once (the message-only
  failures — a group that is gone, a rule its group no longer holds — stay plain `fmt.Errorf`, since
  there is no response to keep reachable)
- `ruleops.go`: `SetRulesPaused(ctx, gClient, uids, RuleOpOptions)` and
  `DeleteRules(ctx, gClient, uids, RuleOpOptions)` — **one** options type for all three operations
  (delete ignores `Paused`), so the cmd layer hands every one of them the same value instead of
  adapting a `dryRun bool` signature through a closure. Both sit on an unexported `ruleOpRun`
  mirroring `push.go`'s `pushRun`. Pause/resume fetch each rule, group by `(folderUID, ruleGroup)`,
  fetch the group, flip `isPaused` on the **copies** and `PutAlertRuleGroup` — one write per group, the
  group's unnamed rules sent back exactly as Grafana reported them. Uids are **deduped** (two
  mentions would be two full replacements racing) and outcomes are emitted **in the order the user
  typed them** rather than in completion order. Delete does `GetAlertRule` first, so an unknown uid
  is a clean `alerts: rule <uid> not found` rather than the **500** Grafana answers a DELETE for an
  unknown UID with; an empty uid is absence without a request (`GET /alert-rules/` is the *list*
  endpoint). That message wraps the exported **`alerts.ErrRuleNotFound`** sentinel (declared in
  `errors.go` with the rest of the package's error vocabulary), which
  `cmd/grafanapi/fail.convertAlertsErrors` matches to render "Alert rule not found" (exit 1) instead
  of the generic "Unexpected error" heading — the cmd layer keeps returning plain errors, never a
  `*fail.DetailedError`, so 401/403 precedence is untouched
- **`unknown` vs `unavailable` — two values, never one**: `unknown` means "the state view was read
  and had no entry for this rule" (not evaluated yet); `unavailable` means "the state view could not
  be read at all", so it is stamped on **every** alerting rule of a degraded run. Collapsing them
  breaks two things at once: `-o json` would carry a plausible `"unknown"` for every rule with no
  machine-readable trace of the outage (the warning is on stderr, which a jq pipe never sees), and
  `--state unknown` — exempt from `stateDependentFilter` because the join assigns it — would
  silently change meaning from "not yet evaluated" to "every alerting rule" and answer with exit 0.
  So `unavailable` is in `KnownStates()`, `stateDependentFilter` exempts `recording` and
  `unavailable` **only**, and `--state unknown` fails in degraded mode exactly like `--state
  firing`. `--state unavailable` is the filter that works during an outage. Pinned at both layers
  (`TestListRules_DegradedStateEndpoint`, `TestListRules_UnavailableStateOnlyOnStateFailure`,
  `Test_RulesListCommand_degradedStateEndpoint{,Filtered}`)
- **Definitions are primary, state is enrichment**: a rule with no state entry gets
  `State: "unknown"` rather than being dropped — and keeps its FOLDER, resolved by folder UID from
  the state response's group objects, so "still listed" also means "still filterable by
  `--folder <path>`" — and a recording rule (`Record != nil`) gets
  `Type`/`State` `"recording"` so it never looks like a broken join. A non-401 state-call failure
  degrades to a `logging.FromContext(ctx).Warn` and continues **only when the request does not
  depend on state**; a `401` from **either** call is fatal, so the stale-session exit-2 contract
  holds. Degrading a *filtered* request would apply the filter to a fabricated state
  (or to folder paths that were never fetched) and return an empty list with a nil error — `--state
  firing -o json` printing `[]` and exiting 0 during an outage is indistinguishable from "nothing is
  firing" and invisible to a jq pipe — so `stateDependentFilter` makes that case a plain wrapped
  error (exit 1, never the 401 exit 2): `alerts: cannot apply --folder|--state <v> without the
  alert rule state: %w`. The degraded warning goes to stderr so `-o json` stays pipeable; both the
  warning and the five fail-instead-of-degrade cases are asserted at both layers. The `401` check
  comes **first** in that branch, and the precedence is pinned at both layers
  (`TestListRules_StateEndpoint401BeatsStateDependentFilter`,
  `Test_RulesListCommand_stateEndpoint401WithFilter`) by asserting the message is *not* the
  "cannot apply" one — `errors.As` alone cannot catch a swap, since wrapping the `401` in
  `cannot apply ...: %w` still satisfies it (and still exits 2) while telling the user to retry with
  fewer flags instead of to re-authenticate
- `state.go` as a whole is ~90 lines cloned from `internal/explore/run.go` — not only
  `buildStateURL` (a fourth copy of the trim-slash-and-append helper, alongside
  `internal/explore`'s `buildQueryURL`, `internal/session`'s `buildUserURL` and
  `internal/config`'s `buildRotateURL`) but also `readBodySnippet`/`maxErrorBodySnippet` verbatim,
  the request-construction block and the 200/401/default status tree. Accepted deliberately;
  extraction into one `internal/httputils` raw-GET-JSON seam is earmarked for the planned `api`
  passthrough group, which adds the fifth call site (see Technical Debt Inventory item 13)

**internal/resources/** - Resource abstraction layer
- `Resource`: Wraps Kubernetes-style unstructured objects for Grafana resources
- `Resources`: Collection with filtering, grouping, and concurrent operations
- Uses `k8s.io/apimachinery` for resource representation
- Supports multiple source formats (JSON, YAML)

**internal/resources/local/** - Local file operations
- `reader.go`: Load resources from disk (supports directories and single files)
- `writer.go`: Save resources to disk with proper formatting

**internal/resources/remote/** - Remote Grafana operations
- `puller.go`: Fetch resources from Grafana API
- `pusher.go`: Upload resources to Grafana API (create/update)
- `deleter.go`: Delete resources from Grafana

**internal/resources/dynamic/** - Dynamic Kubernetes client wrapper
- `namespaced_client.go`: Per-namespace resource operations
- `versioned_client.go`: Version-aware resource client
- Wraps k8s.io/client-go dynamic client

**internal/resources/discovery/** - API discovery
- `registry.go`: Discover available resource types from Grafana API
- `registry_index.go`: Index and cache discovered resources

**internal/resources/process/** - Resource processing
- `managerfields.go`: Handle manager metadata (similar to kubectl's server-side apply)
- `serverfields.go`: Process server-managed fields

**internal/server/** - Local development server (for `resources serve`)
- Chi-based HTTP server with reverse proxy to Grafana
- Live reload via WebSocket
- File watching with fsnotify
- Dashboard and folder preview handlers
- Script execution for generated resources
- Cookie injection and rotate-on-401 are transport-level here too: `server.go`'s
  `httputil.ReverseProxy` and `server/grafana/requests.go`'s dashboard-proxy client both wrap their
  `Transport` (built from `httputils.NewTransport`) with `(GrafanaConfig).WrapWithSession`, so a
  `401` from Grafana triggers the same rotate-and-retry as the k8s/openapi paths
- `guard.go`: the **outermost** middleware (registered before `middleware.RequestLogger`, so a
  `?token=` query is never logged; `reject` logs method and path only). Order: Host allowlist
  (`421`) -> a present `?token` is redeemed or rejected **even when a valid cookie is already
  held** (GET/HEAD with the right token -> cookie + `303` to a fixed `/`, never derived from
  `r.URL`; anything else `401`; `next` is never called, so the token cannot reach a handler or the
  proxy) -> cookie (`401`) -> Origin and `Sec-Fetch-Site` (`403`). `allowedHosts(listenHost, port, ifaceAddrs)` builds
  the Host set (a wildcard bind allows the non-link-local interface IPs present **at startup**
  only, so a changed IP gives `421` until restart)
- Token/cookie design: `rand.Text()` per run (128-bit, `Server.token`), compared in constant time.
  Cookie `grafanapi_serve_<port>`, `HttpOnly`, `SameSite=Strict`, `Path=/`, no `Max-Age` (a
  browser-session cookie), no `Secure` (plain http on loopback would drop it), plus
  `Referrer-Policy: no-referrer` on the redirect. Cookies are scoped by host, not port, so other
  servers on the same host receive it; two checks keep their *pages* out (a request with an `Origin`
  must carry this server's own; a `Sec-Fetch-Site` that is present must be `same-origin` or
  `none`, which also covers the no-`Origin` subresource GETs a same-site page on another local
  port can send), but the cookie is not protection against a hostile local server. A request with
  neither header is a non-browser client and passes
- Proxy Origin rule: the guard has already vetted any `Origin` as this server's own, so the
  `ReverseProxy.Rewrite` replaces it with **Grafana's own origin** (no Origin in -> none out)
  instead of deleting it, because `SetURL` sends Grafana's Host and the browser's Origin could never
  match it. Rewrite also drops the browser `Cookie` (it holds the serve token; session cookie
  injection is the transport's job) and `ModifyResponse` strips `Set-Cookie`. Grafana's CSRF
  behaviour on the rewritten Origin is reasoned, not verified against a live instance
- Lifecycle: `Listen(ctx)` (a `net.ListenConfig`, resolves the real port, e.g. for `:0`) ->
  `URL()` (tokenized, uses the bound port) -> `Serve(ctx, ln)` (shuts down on ctx cancel,
  returns nil); `handler(ctx)` is unexported. The `http.Server` carries `ReadHeaderTimeout`,
  `ReadTimeout`, `WriteTimeout` and `IdleTimeout`. The websocket survives them because
  `net/http`'s `Hijack` clears the connection deadlines (gorilla clears them again when
  `HandshakeTimeout` is 0); `TestServe_WebsocketSurvivesServerTimeouts` pins the behaviour.
  The upgrader's `CheckOrigin` is `guard.originAllowed`
- `livereload.Initialize` is guarded by a `sync.Once`: it used to start a new global hub per call,
  and `connections` is mutated by that hub's `run()` loop

**internal/grafana/** - Grafana API client
- Wraps grafana-openapi-client-go
- Client construction from context configuration
- Cookie injection is transport-level, not a static header map: `ClientFromContext` wraps the
  vendored client's `*httptransport.Runtime.Transport` with `(GrafanaConfig).WrapWithSession`, so
  the openapi path sees `401`s and rotates identically to the k8s path (no more static
  `HTTPHeaders` cookie map)

**internal/format/** - Format detection and conversion
- JSON, YAML format support
- Auto-detection from file extensions

**internal/httputils/** - HTTP utilities
- REST client helpers
- Request/response handling

**internal/keychain/** - macOS Keychain credential store
- `Store` interface (`Set`/`Get`/`Delete`/`ModifiedAt`) keyed by `"grafanapi:<context-name>"`
- Darwin-only cgo implementation against `Security.framework` (package build tag `darwin`);
  grafanapi does not build on any other platform
- `ModifiedAt(account) (time.Time, error)` is the **last-rotation-time source** for keep-alive: the
  Keychain item's `kSecAttrModificationDate` is updated by `securityd` on every `Set` (login, login
  update, and every rotation), so it is exactly "the last time this context's cookie was
  (re)written" with zero new persisted state. The darwin implementation queries
  `SecItemCopyMatching` with `kSecReturnAttributes` and **no** `kSecReturnData`, so — unlike a real
  `Get` — it never decrypts the secret and therefore does not trigger the Keychain ACL "Allow"
  prompt; `config check` and the `--due` scheduler read it silently. Returns `ErrNotFound` when no
  item exists.

**internal/launchd/** - macOS launchd LaunchAgent management (no third-party plist library; plist
generation is stdlib `text/template`, inspection is stdlib `encoding/xml` token scanning)
- `spec.go`: `Label` (locked `io.github.avitsrimer.grafanapi.keepalive`), `AgentSpec`,
  `DefaultAgentSpec` (`Args = session refresh --due` — **not** `--all`, so the scheduler honors each
  context's own `live-window`), interval bounds `[1m, 6d]`
- `paths.go`: home-based path helpers — plist at
  `~/Library/LaunchAgents/io.github.avitsrimer.grafanapi.keepalive.plist`, log at
  `~/Library/Logs/grafanapi/keepalive.log`
- `plist.go`: `Generate` (XML-escaped binary/log paths, `RunAtLoad` false) and `Inspect` (Label,
  StartInterval, ProgramArguments) round-trip a plist without any third-party dependency
- `path.go`: `ResolveBinaryPath` — `os.Executable()` → `EvalSymlinks`, then prefers a stable
  Homebrew symlink (`/opt/homebrew/bin/grafanapi` or `/usr/local/bin/grafanapi`) over a versioned
  `Cellar`/`Caskroom` path, so the plist's absolute program path survives a `brew upgrade`
- `controller.go`: `Controller` interface (`Bootstrap`/`Bootout`/`Print`) over `launchctl`'s modern
  `gui/<uid>`/`gui/<uid>/<label>` targets; the real implementation shells out through an injectable
  `commandFunc` seam (never a literal `exec.Command` selector, so no gosec G204 finding); tests use
  `testutils.FakeController` and never invoke real `launchctl`
- `session keepalive install` derives `StartInterval` as `min(live-window)/2` clamped to
  `[15m, 12h]` unless `--interval` overrides it (bounded `[1m, 6d]`, never re-clamped to the
  narrower derived range)

**internal/session/** - Session cookie verification and stale-session errors
- `VerifyCookie`: validates a cookie via `GET /api/user`
- `ErrUnauthorized`: sentinel returned by `VerifyCookie` on a 401; recognized centrally by
  `cmd/grafanapi/fail.convertAuthErrors`
- Only used by `login`/`login update` (which always paste a fresh cookie and validate it
  directly) — unrelated to `internal/config`'s `SessionSource` rotation, which only ever kicks in
  for *already-authenticated* commands hitting a stale cookie mid-session

**internal/secrets/** - Secret management
- Secure handling of sensitive configuration data (currently just TLS key data; the session cookie is never serialized to disk)

**internal/termsafe/** - Terminal-safe rendering of server-controlled text (leaf package, stdlib only)
- `Cell` drops every escape sequence whole (CSI, OSC, DCS, SOS, PM, APC and other ESC-introduced
  sequences, 7-bit and 8-bit C1 forms) and then replaces each remaining control or Unicode format rune
  with a space; an unterminated string sequence (OSC/DCS/SOS/PM/APC) is dropped to the end of the value.
  `Text` is `Cell` for multi-line messages (keeps `\n` and `\t`, still replaces `\r`)
- `Runes` is the per-rune rule alone, with no sequence parsing; it exists only so alerts' `cell` stays
  byte-identical. New callers use `Cell`
- Tag characters (U+E0000 block, `Cf`) are removed. Deliberate non-goal: U+2028/U+2029 and variation
  selectors pass through (not terminal control)
- The json/yaml codecs must **not** use it: they carry raw values and callers are told to treat them as
  untrusted. Callers: `explore`, `datasources`, `alerts` tables, `fail.DetailedError`, `internal/logs`

**internal/logs/** - Structured logging
- slog-based logging with verbosity levels
- The record message goes through `termsafe.Cell`, and `prepareString` no longer rewrites a literal
  `\x1b` in a quoted value into a real ESC (it used to re-create the byte the earlier cut had removed)
- Integration with k8s klog

### Key Design Patterns

1. **Context-Based Configuration**: Like kubectl, grafanapi uses named contexts to manage multiple Grafana instances. Each context contains server URL, authentication, and namespace (org-id or stack-id).

2. **Kubernetes-Style Resources**: Resources are represented as unstructured objects following Kubernetes conventions (apiVersion, kind, metadata, spec). This enables compatibility with k8s tooling and patterns.

3. **Resource Manager Metadata**: Tracks which tool manages each resource (grafanapi, UI, etc.) to prevent accidental overwrites. Only resources managed by grafanapi can be modified via grafanapi.

4. **Three-Way Merge**: When pushing resources, grafanapi performs server-side apply semantics similar to kubectl, allowing multiple managers to coexist.

5. **Dashboards as Code**: The `serve` command supports script-based dashboard generation with live preview, enabling code-first workflows with SDKs like grafana-foundation-sdk.

6. **Raw-HTTP Wire Structs Where Generated Models Are Lossy**: `internal/explore` decodes
   `/api/ds/query` responses into its own local structs (`dataframe.go`) instead of the generated
   client's response model, because `models.Frame`/`models.Field` cannot round-trip the real
   JSON-dataframe wire shape (no `schema`/`data` split, no values field at all). This is a
   deliberate escape hatch from the "always use the generated client" pattern, applied only where
   the generated model is verifiably lossy — datasource lookup in the same package still uses the
   generated client, since `models.DataSource` round-trips correctly.

   `internal/alerts/state.go` also uses local wire structs, but **for a different reason, and the
   distinction matters**: the generated `models.RuleDiscovery`/`RuleGroup`/`AlertingRule` are
   **not** lossy — they would round-trip every field the prometheus rules view needs. Nothing calls
   them because (a) there is no generated *service* for
   `/api/prometheus/grafana/api/v1/rules` at all (there is no `client/prometheus`), (b) they do not
   model the `{status, data}` envelope the endpoint actually returns, and (c) every field arrives as
   a pointer or `strfmt` type needing a nil-safe deref for the ~8 values the package reads. So: the
   explore case is "the generated model is broken for this payload", the alerts case is "the
   envelope and the deref ergonomics"; do not conclude from `internal/alerts` that the alerting
   models are lossy. Rule and contact-point **definitions** in the same package go through the
   generated client, as `models.ProvisionedAlertRule`/`EmbeddedContactPoint` round-trip correctly.

7. **Provenance-Aware Alerting Writes (refuse, never guess)**: every write in `internal/alerts`
   classifies the object's **stored** `provenance` first (`provenance.go`) and behaves three ways:
   `""` (UI-editable) → send `X-Disable-Provenance: true`; `"api"` (this tool's own claim) → omit
   the header; **anything else** — `file`, `converted_prometheus`, `disabled`, or a value Grafana
   adds later — → **refuse the write** with
   `alerts: <op> <kind> <id>: provenance "<v>" is managed outside the API`, as a per-item failure
   that honours `--on-error`. A rule group whose rules disagree is refused the same way. This is
   not defensiveness for its own sake: Grafana enforces the pairing, and getting it wrong is a
   **500** (rules) or a 403 `alerting.notifications.invalidProvenance` (policies) on an update, and
   a 409 `alerting.provenanceMismatch` on a delete. An object another provisioner owns would also
   simply be re-created by its owner on the next run, so overwriting it is never the user's intent.
   `--ui-editable` picks the mode for a **new** object only (`CreateProvenanceHeader`); an update
   always mirrors what is stored. This is the same instinct as resource manager metadata (pattern
   3) applied to alerting: know who owns the object before writing it.

## Configuration

Configuration is stored in `$XDG_CONFIG_HOME/grafanapi/config.yaml` (typically `~/.config/grafanapi/config.yaml`).

### Environment Variables

Environment variables can override non-secret configuration values:
- `GRAFANA_SERVER`: Grafana server URL
- `GRAFANA_ORG_ID`: Organization ID (on-prem)
- `GRAFANA_STACK_ID`: Stack ID (Grafana Cloud)

There is no environment variable for the session cookie — it is never
accepted via flag or env var, only through the interactive `grafanapi login`
/ `grafanapi login update` prompts, and is resolved from the macOS Keychain at
config-load time.

## Testing Patterns

- Unit tests use Go standard testing with testify/assert
- Configuration tests in `internal/config/*_test.go`
- Resource filtering/selector tests in `internal/resources/*_test.go`
- Test data in `testdata/` directories
- Use `make tests` to run all tests with race detection
- **Build-tag convention** (`internal/keychain/`): the whole package carries `//go:build darwin`
  (it is cgo against `Security.framework`), so its tests do too — they exercise the real cgo
  Keychain against throwaway accounts, cleaned up via `t.Cleanup`. CI runs the whole module,
  including this package, on a `macos-latest` runner.
- **Package-level test-seam pattern** (`cmd/grafanapi/login/`, `cmd/grafanapi/config/`): production
  code depends on `keychain.Store` (and, for `login`, a `prompter` interface) via a package-level
  `var` defaulting to the real implementation. Tests swap in a fake via an exported
  `SetKeychainStore(store)` / `SetPrompter(p)` function, each returning a restore closure. Pass that
  closure straight to **`t.Cleanup`**, never to a `defer` the caller has to remember: a forgotten
  `defer` leaves the real macOS Keychain wired up for every test that runs afterwards, which fails
  somewhere else entirely or blocks on an ACL prompt. The `cmd/grafanapi/alerts` fixtures
  (`newTestConfig`, `setupPush`, `setupRuleOp`, `newPullFixtureAndConfig`) therefore return no
  closure at all — they install the fake store and register its cleanup themselves.
- **Write-test fixture convention** (`internal/alerts`, `cmd/grafanapi/alerts`): a write test drives
  a scripted `httptest` server through a route table keyed by **method + path** (`pushRoute`), not
  by path alone — a push GETs and writes the same addresses. The fixture records every request's
  method, path, `X-Disable-Provenance` header (a `*string`: absent and `"true"` are different
  instructions to Grafana), cookie and body, and `requests`/`requestsFor`/`writes`/`saw` are how a
  test asserts what was, and was not, written. A request to a path the table does not script fails
  the test rather than 404ing quietly. Concurrency is pinned with a **barrier**, never a sleep: the
  server holds each request until the bound is saturated, so a serial implementation fails on the
  barrier's timeout and an unbounded one fails on the recorded peak. `/rotate` is scripted `401`
  (a truly dead session) everywhere except the replay tests, which wire a real
  `config.NewSessionSource` at a rotate server that succeeds, to prove the generated client's write
  bodies are rewindable (`req.GetBody`).

## Code Generation

- CLI reference docs auto-generated from Cobra commands via `scripts/cmd-reference/`
- Environment variable docs generated via `scripts/env-vars-reference/`
- Config reference docs generated via `scripts/config-reference/`
- All generated docs must be committed (checked by CI via `make reference-drift`)

## Build Process

Build uses Make with devbox. Version information injected at build time:
- `version`: Git tag (exact match) or "SNAPSHOT"
- `commit`: Git commit SHA
- `date`: Build timestamp

Flags set in Makefile via `-ldflags` on `main.version`, `main.commit`, `main.date`.

## Important Notes

- Only supports Grafana 12+
- Project is in "public preview" maturity
- Uses vendored dependencies (committed to repo)
- Documentation built with mkdocs (Python-based)
- Feature toggle `kubernetesDashboards` must be enabled in Grafana for `resources serve`
- **macOS/arm64 only, explicitly**: the Keychain credential store is cgo against
  `Security.framework`, `internal/keychain` carries `//go:build darwin`, and
  `cmd/grafanapi/unsupported.go` deliberately fails the build with a clear compile error on any
  other `GOOS`. There is no linux/cross-compile support and none is planned — CI (lint, tests,
  docs) all run on `macos-latest`. Released via GoReleaser as a Homebrew cask
  (`avitsrimer/homebrew-apps`).

## Codebase Architecture Insights

### System Overview

**grafanapi** is a ~16,000 line (non-test; ~32,000 with tests, excluding `vendor/`) Go codebase
that provides a kubectl-like interface for managing Grafana resources. It bridges Grafana's REST API with Kubernetes-style resource management patterns, enabling Infrastructure-as-Code workflows for Grafana dashboards, folders, and other resources.

The tool's architecture is heavily influenced by Kubernetes client patterns, using k8s.io/client-go and k8s.io/apimachinery libraries to provide a consistent, familiar interface for developers already comfortable with kubectl.

### Architectural Layers

The codebase follows a clean layered architecture:

```
CLI Layer (cmd/grafanapi/)
    ↓
Business Logic Layer (internal/resources/, internal/config/)
    ↓
Client Abstraction Layer (internal/resources/dynamic/)
    ↓
Transport Layer (internal/httputils/, internal/grafana/)
    ↓
Grafana REST API
```

### Core Abstractions

#### 1. Resource Abstraction (`internal/resources/resources.go`)

The `Resource` type is the fundamental building block, wrapping `k8s.io/apimachinery/unstructured.Unstructured`:

- **GrafanaMetaAccessor**: Provides typed access to Grafana-specific metadata (manager, source info)
- **Source Tracking**: Each resource tracks its origin (file path, format) for round-tripping
- **Manager Metadata**: Resources carry information about which tool manages them (grafanapi vs UI)
- **Collection Operations**: The `Resources` type provides filtering, grouping, and concurrent operations

#### 2. Discovery System (`internal/resources/discovery/`)

API discovery is critical for dynamic resource type resolution:

- **RegistryIndex**: Caches discovered resource types from Grafana's API server
- **Preferred Versions**: Like Kubernetes, tracks which version is preferred for each resource type
- **Selector Resolution**: Converts partial user input (e.g., "dashboards") to fully-qualified GVK
- **Filtering**: Excludes read-only or internal resource groups (featuretoggle, iam, etc.)

#### 3. Selector → Filter Pipeline

User input flows through a two-stage resolution process:

1. **Selectors** (`internal/resources/selector.go`): Partial specifications from user
   - Supports short forms: "dashboards", "dashboards/foo"
   - Supports long forms: "dashboards.v1alpha1.dashboard.grafana.app/foo"
   - Parsed into `PartialGVK` + resource UIDs

2. **Filters** (`internal/resources/filter.go`): Fully-resolved specifications
   - Contains complete `Descriptor` with GVK and plural/singular forms
   - Three filter types: All (list all), Multiple (get specific), Single (get one)
   - Used by readers/pullers to fetch the right resources

#### 4. Dynamic Client (`internal/resources/dynamic/`)

Wraps k8s.io/client-go's dynamic client for Grafana:

- **Namespaced Operations**: All operations scoped to a namespace (org-id or stack-id)
- **Pagination**: Automatic handling via k8s.io/client-go pager
- **Concurrent Gets**: When fetching multiple named resources, uses errgroup for parallelism
- **Error Translation**: Converts k8s StatusError to friendly error messages

### Execution Flows

#### Push Flow (Local → Grafana)

```
1. Read files from disk (FSReader)
   - Concurrent file reads with errgroup
   - Format detection (JSON/YAML)
   - Decode into unstructured.Unstructured

2. Apply filters (from selectors)
   - Skip resources not matching user criteria
   - Deduplicate by GVK+name

3. Process resources
   - ManagerFieldsAppender: Add grafanapi manager metadata
   - ServerFieldsStripper: Remove read-only fields (for dry-run)

4. Push to Grafana (Pusher)
   - Two-phase: folders first (dependency ordering)
   - Per-resource: Check if exists → Create or Update
   - Concurrent pushes (configurable concurrency)
   - Dry-run support via Kubernetes dry-run semantics
```

#### Pull Flow (Grafana → Local)

```
1. Discover API resources (Registry)
   - Call Grafana's ServerGroupsAndResources endpoint
   - Build index of supported resource types

2. Resolve selectors to filters
   - Convert user input to fully-qualified descriptors
   - Support version wildcards (all versions vs preferred)

3. Fetch from Grafana (Puller)
   - List or Get operations via dynamic client
   - Concurrent fetches with errgroup
   - Process: Strip server fields, check manager

4. Write to disk (FSWriter)
   - Group by kind (one file per kind or per resource)
   - Format as JSON/YAML
   - Create directory structure
```

#### Serve Flow (Local Development Server)

```
1. Start HTTP server (Chi router)
   - Listen on --address (default 127.0.0.1), print the tokenized URL
   - Guard middleware first: Host allowlist (421) -> ?token redeem/reject -> cookie (401) ->
     Origin / Sec-Fetch-Site (403); only then the request logger and the routes
   - Reverse proxy to Grafana (vetted Origin rewritten to Grafana's, no browser Cookie)
   - Custom handlers for dashboards/folders
   - WebSocket for live reload

2. Watch files for changes (fsnotify)
   - Detect file modifications
   - Reload resources into memory
   - Trigger WebSocket reload event

3. Serve dashboard previews
   - Intercept Grafana dashboard API calls
   - Inject local resource data
   - Mock out unrelated API endpoints

4. Live reload in browser
   - WebSocket connection to browser
   - Automatic page refresh on file change
```

#### Explore Flow (Ad-hoc datasource query)

```
1. Resolve the datasource (internal/explore.ResolveDataSource)
   - Generated client: GetDataSourceByUID, then GetDataSourceByName on 404
   - Both-miss: list all datasources, return a "not found" error naming them

2. Build the query (internal/explore.QueryFieldForType / BuildQuery)
   - Map datasource type -> request field (expr/rawSql/target/query)
   - Apply type extras (e.g. SQL's format:"table"), --interval/--instant,
     --param overrides (highest precedence)

3. Execute the query (internal/explore.Run)
   - Raw POST /api/ds/query over httputils.NewTransport wrapped with
     GrafanaConfig.WrapWithSession (so a 401 rotates-and-retries)
   - Decode the raw response body into local wire structs (not the generated
     client's response model - see Key Design Patterns)

4. Render the result (internal/explore.RenderTable)
   - Table (default) via text/tabwriter, or json/yaml for piping
```

#### Alerts Flow (`alerts rules list`)

```
1. Validate the flags (cmd/grafanapi/alerts.rulesListOptions.Validate)
   - Output format + --state (NormalizeState -> KnownStates), before any
     config load or network call

2. Fetch the definitions (internal/alerts.ListRules)
   - Generated client: Provisioning.GetAlertRulesWithParams(...WithContext(ctx))
   - Any error is fatal, wrapped "alerts: list alert rules: %w" so a 401 stays
     reachable via errors.As

3. Fetch the live state (internal/alerts.FetchRuleStates)
   - Raw GET /api/prometheus/grafana/api/v1/rules over httputils.NewTransport
     wrapped with GrafanaConfig.WrapWithSession (so a 401 rotates-and-retries);
     ActiveAlerts is an exact scan of each rule's `alerts[]` (`totals` is not a
     partition of the instance set - see internal/alerts/state.go)
   - Decode the {status,data} envelope into local wire structs; flatten the
     group tree into RuleStates{ByUID, FolderPaths} - per-rule state keyed by
     uid (entries with no uid are skipped) plus folderUid -> `file` path per
     group (groups missing either half are skipped)
   - 401 -> fatal, unwrapped *runtime.APIError{401} - checked BEFORE
     stateDependentFilter, so a dead session never renders as "cannot apply"
   - any other failure -> fatal too if the request depends on state
     (stateDependentFilter: any --folder, or a --state other than
     recording/unavailable) -> plain wrapped error, exit 1
   - any other failure on an unfiltered request -> warn and continue with a
     zero RuleStates and unjoined = "unavailable" (degraded mode), so the
     degraded run is detectable in -o json and not on stderr alone

4. Join, filter, sort (internal/alerts.ListRules)
   - Join on uid: recording rules -> "recording", unjoined alerting rules ->
     "unknown" (healthy state view) or "unavailable" (degraded); state entries
     with no definition are ignored
   - --state is matched case-insensitively against the endpoint's verbatim
     state, since the filter has been lowercased by NormalizeState
   - Second join, on folderUID: a rule with no state entry still gets its
     folder path from RuleStates.FolderPaths, so FOLDER is populated and
     --folder by path/leaf reaches it (see applyState)
   - Filter --folder (UID, full folder path, or the path's last segment - a
     title match, so it spans folders sharing that leaf) and --state, then sort
     folder/group/title/uid
   - Always a non-nil slice, so -o json emits [] not null

5. Render the result (cmd/grafanapi/alerts.rulesTableCodec)
   - Table (default) via text/tabwriter, or json/yaml encoding []alerts.Rule

`alerts contact-points list` is steps 1, 2 (GetContactpoints), 4 (sort only)
and 5 (contactPointsTableCodec over models.ContactPoints) - no state join.
```

#### Alerts Push Flow (`alerts push`)

```
1. Validate the flags, load the context, read the bundle
   - cmd/grafanapi/alerts.pushOptions.Validate -> LoadConfig ->
     grafana.ClientFromContext -> internal/alerts.ReadBundle(path)
   - ReadBundle is strict in both formats (DisallowUnknownFields / yaml.Strict)
     and checks each model structurally - required fields, never the generated
     enums - so a typo'd key or a missing required field costs no network call
     and no write, while a value the vendored swagger predates (KeepLast, a
     webex contact point) still round-trips
   - It also refuses a bundle that addresses one object twice: two files
     declaring the same (folderUid, title) - the path is not authoritative, so
     nothing else stops it - and two contact-point entries sharing a uid (or,
     uid-less, a name). Both would otherwise become two racing writes to one
     object, last writer winning, silently

2. Fetch only what the plan needs (internal/alerts.Pull, from Push)
   - PullOptions.Groups = the local groups' refs (non-nil, possibly empty), so
     one GetAlertRuleGroup per local group - never GetAlertRules over the whole
     instance - plus one GetContactpoints and one GetPolicyTree
   - A group absent remotely (404) is "not there yet", not an error: that is a
     Create

3. Plan (internal/alerts.Plan) - pure, no requests
   - Create | Update | Unchanged only; remote-only objects are omitted (no
     prune). Rule groups diff by uid with "+/-/~ <title> (<uid>)" details plus
     "~ interval Ns→Ms" and "~ rule order" when the shared uids stand in a
     different sequence (Grafana evaluates in body order); a local "[REDACTED]"
     leaf compares equal to whatever stands remotely; the policy tree is
     compared ignoring provenance recursively
   - It also refuses what ReadBundle cannot see: two local contact points whose
     plan IDs differ (one carrying a uid, one uid-less matched by name) but
     which resolve to the same remote object
   - --dry-run does NOT stop here: it runs steps 4-5 with every write skipped,
     so the read-only refusals (missing folder, foreign provenance, a create
     still carrying "[REDACTED]") are reported as "would fail" lines below the
     plan table. Still zero writes, still exit 0, and --on-error is ignored

4. Pre-check folders (internal/alerts.folderExists, via asAPIError)
   - Once per distinct folderUid of a group that will actually be written
   - 404 -> that folder's groups fail with "folder <uid> does not exist; push
     folders first (grafanapi resources push)"; folders are NEVER auto-created
   - Any other failure is fatal: same session, same permissions for every write
     that would follow

5. Apply, in dependency order: contact points -> policy tree -> rule groups
   - Provenance decides the X-Disable-Provenance header per object (stored ""
     -> "true", "api" -> no header, anything else -> per-item refusal); a new
     object uses CreateProvenanceHeader(--ui-editable)
   - Bodies are copies: folderUID/ruleGroup pinned per rule, orgID resolved
     remote -> context (non-zero) -> file, id/updated/provenance cleared
   - A rule group PUT REPLACES the group, so a rule missing from the file is
     deleted from it (the "-" lines the dry run printed)
   - StopOnError (abort) writes the groups one at a time and stops at the first
     failure; otherwise every write is attempted (plain errgroup, bounded by
     --max-concurrent - cancelling siblings would report writes that never ran
     as failures). Only a 401 propagates as the error that ends the push

6. Report (cmd/grafanapi/alerts.reportPushResult -> reportPush)
   - One cmdio line per change, applied first and failures last, then
     "N change(s) applied, M error(s)"
   - Printed even when the push was cut short (--on-error abort, or a 401
     halfway through): what was already written has to stay visible. Push's own
     error then wins over the report's aggregate
   - Skipped when a failed push applied and failed nothing (a 401 on the fetch,
     an ambiguous contact point): a zero Result would print "0 change(s)
     applied, 0 error(s)" above the real error
   - onerror.Mode.FailOnErrors() decides the exit code; the error wraps the
     FIRST failure, so a 403 still renders as permission-denied (exit 1) and a
     401 as stale-session (exit 2)
```

`alerts rules pause|resume|delete` share steps 5-6's shape without a bundle
(and without step 4: a rule's folder is wherever Grafana already holds it):
GetAlertRule per uid (unknown -> "not found", before any write), group by
(folderUID, ruleGroup), then one PutAlertRuleGroup per group with isPaused
flipped on the named rules - or one DeleteAlertRule per uid - with the same
provenance header rule and the same first-failure-wrapping report.

### Key Technology Choices

#### Kubernetes Client Libraries

Using k8s.io/client-go provides:
- Battle-tested dynamic client implementation
- Standard patterns for discovery, pagination, error handling
- Unstructured object representation (map[string]interface{})
- Compatibility with Grafana's Kubernetes-inspired API

Trade-off: Adds dependency weight (~many vendor packages) but saves enormous implementation effort.

#### Cobra for CLI

Standard Go CLI framework providing:
- Subcommand structure
- Flag binding and validation
- Help generation
- Consistent with other Cloud Native tools

#### Context-Based Configuration

Similar to kubectl config, enables:
- Multiple environment management (dev, staging, prod)
- Easy context switching
- Environment variable overrides
- Secure credential storage

#### Concurrent Operations

Heavy use of `golang.org/x/sync/errgroup`:
- File reads/writes parallelized
- API calls (Get, Create, Update) concurrent
- Configurable concurrency limits
- Proper error propagation and cancellation

### Code Organization Patterns

#### 1. Options Pattern

Commands use options structs with setup/validate methods:
```go
type pushOpts struct {
    Paths []string
    MaxConcurrent int
    // ...
}

func (opts *pushOpts) setup(flags *pflag.FlagSet) { ... }
func (opts *pushOpts) Validate() error { ... }
```

#### 2. Processor Pattern

Resource transformations via processor interface:
```go
type Processor interface {
    Process(*Resource) error
}
```

Used for:
- Adding manager fields (`ManagerFieldsAppender`)
- Stripping server fields (`ServerFieldsStripper`)
- Composable pipeline in push/pull operations

#### 3. Registry Pattern

Discovery registry caches API metadata:
- Lazy initialization on first use
- Index by GVK for fast lookups
- Supports partial GVK matching
- Preferred version tracking

#### 4. Source-Tracking Pattern

Resources carry their source information:
- File path where they were read
- Format (JSON/YAML)
- Enables round-tripping preserving format
- Used in error messages for context

### Testing Strategy

- **Unit Tests**: Focus on parsing, filtering, selector logic
- **Table-Driven Tests**: Common in selector/filter tests
- **Mock Filesystems**: Use `testdata/` directories for test fixtures
- **No Integration Tests**: All tests are unit tests (no live Grafana required)

Current test coverage focuses on:
- Configuration loading and validation
- Selector parsing and resolution
- Filter matching logic
- Resource grouping and sorting
- Manager field processing

### Performance Characteristics

**Concurrency Model**:
- Default 10 concurrent operations (configurable)
- errgroup with SetLimit for bounded concurrency
- File I/O parallelized (reading multiple files)
- Network I/O parallelized (multiple API calls)

**Memory Profile**:
- Resources loaded into memory during operations
- No streaming (entire resource collections in RAM)
- Reasonable for typical use cases (hundreds of dashboards)
- Could be problematic for very large deployments (thousands of resources)

**Network Efficiency**:
- Pagination handled automatically by k8s client
- List operations prefer List over Get-per-resource
- Get operations parallelized when fetching multiple named resources

### Extension Points

The codebase is designed for extension:

1. **New Resource Types**: Automatically discovered via Grafana API
2. **New Formats**: Add codec to `internal/format/codec.go`
3. **New Processors**: Implement Processor interface
4. **Custom Handlers**: Add to `internal/server/handlers/`
5. **Authentication Methods**: Extend `internal/config/rest.go`
6. **New Explore Datasource Types**: Add one entry to `fieldByType` in `internal/explore/query.go`
   (query field key + any type extras, e.g. SQL's `format:"table"`); no other change needed unless
   the type needs bespoke request shaping beyond the field-key mapping.

### Dependencies of Note

**Core Libraries**:
- `k8s.io/client-go` v0.34.1: Dynamic client, discovery
- `k8s.io/apimachinery` v0.34.1: Unstructured, GVK, schemas
- `github.com/spf13/cobra` v1.10.1: CLI framework
- `github.com/go-chi/chi/v5` v5.2.3: HTTP router for serve
- `github.com/gorilla/websocket` v1.5.4: Live reload

**Grafana Libraries**:
- `github.com/grafana/grafana-openapi-client-go`: Generated API client
- `github.com/grafana/grafana/pkg/apimachinery`: Grafana's K8s extensions
- `github.com/grafana/grafana-app-sdk/logging`: Structured logging

**Utilities**:
- `golang.org/x/sync` v0.17.0: errgroup for concurrency
- `github.com/goccy/go-yaml` v1.18.0: YAML codec
- `github.com/fsnotify/fsnotify` v1.9.0: File watching

### Security Considerations

**Credential Management**:
- Grafana session cookie stored in the macOS Keychain (generic-password item,
  ACL-only, one per context) — never written to the plaintext config file
- Config file (0600 permissions) holds only non-secret context data (server,
  org-id/stack-id, TLS); the in-memory `SessionCookie` field is `json:"-" yaml:"-"`
- TLS key data remains the only `datapolicy:"secret"`-tagged, redacted field
- Legacy `token`/`user`/`password` config keys are rejected by strict YAML
  decoding, with a migration message pointing to `grafanapi login`
- Automatic session rotation (`internal/config.SessionSource`) re-persists a fresh cookie to the
  Keychain on every `401`, using its own dedicated, bounded-timeout HTTP client that is never
  wrapped in a logging/debug transport — the cookie value is never written to logs at any point
  in the rotation path

**On-disk artifacts (`alerts pull`)**:
- An alerting bundle is the one artifact grafanapi writes that can contain third-party credentials.
  Files are created `0600` (directories `0750`), which is the only protection they have.
- Redaction is **conditional, never blanket**: Grafana replaces each contact-point type's
  *schema-declared* secure fields with `"[REDACTED]"` server-side, so those never reach disk. A
  credential Grafana does not classify that way — a token in a webhook URL path/query, or in a
  custom header value — is written to `contact-points.<ext>` **verbatim**. Docs must say
  "declared secure fields", never an unconditional "secrets stay redacted", and must tell users to
  review the bundle before committing or promoting it.
- Pushing the marker back keeps the stored secret on an **update** only; a create carrying it is
  refused (see `internal/alerts/push.go`), so the literal string is never stored as a secret.
- Nothing in this package decrypts, prints or logs a secret: there is no `decrypt=true` call site
  and no contact-point delete.

**Terminal Output**:
- Server-controlled text (datasource names, label values, error messages, log lines) is scrubbed by
  `internal/termsafe` before it reaches a terminal: escape sequences are removed, other control and
  format characters (bidi overrides, tag characters) become spaces
- `-o json` / `-o yaml` are raw by design and must be treated as untrusted data; `jq -r` prints
  control characters verbatim
- Scrubbing removes terminal control only. It cannot stop plain-text prompt injection in output an
  agent reads

**Network Security**:
- TLS verification enabled by default
- Option to disable (insecure-skip-verify) for dev/test
- Custom CA certificates supported
- Client certificate authentication supported

**Local serve server**:
- `resources serve` binds `127.0.0.1` by default; a non-loopback `--address` prints a warning,
  because the server acts with the user's Grafana session
- Every request passes `internal/server/guard.go`: Host allowlist (DNS rebinding, `421`), a per-run
  access token redeemed once into an `HttpOnly` `SameSite=Strict` cookie (`401`), and a same-origin
  check on `Origin` and `Sec-Fetch-Site` (`403`; also applied to the live-reload websocket upgrade)
- The proxy never forwards the browser's `Cookie` and strips Grafana's `Set-Cookie`
- The token cookie is host-scoped, not port-scoped: it reaches other servers on the same host

**Resource Management**:
- Manager metadata prevents accidental overwrites
- Dry-run mode for safe testing
- Include-managed flag required to modify non-grafanapi resources

### Common Gotchas

1. **Namespace Confusion**: org-id (on-prem) vs stack-id (cloud) are both called "namespace"
2. **Version Handling**: Preferred version vs all versions affects discovery
3. **Folder Ordering**: Folders must be pushed before dashboards (dependency)
4. **Manager Metadata**: Resources created by UI can't be pushed unless --include-managed
5. **Format Preservation**: Pull/push roundtrip preserves original format (JSON/YAML)
6. **Explore's Dead-Session 401 Must Stay a `*runtime.APIError`**: `internal/explore.Run` must
   surface a dead-session `401` (rotation exhausted) as `runtime.NewAPIError(..., 401)`, never as
   `session.ErrUnauthorized`. `cmd/grafanapi/fail.convertAuthErrors` maps the two differently:
   the bare sentinel renders as the login-rejected message ("Grafana rejected the provided session
   cookie" — meant for `login`/`login update`), while `*runtime.APIError{Code:401}` renders as the
   stale-session message ("Grafana session is stale or unauthorized" / `login update` suggestion).
   Returning the wrong one shows the wrong message to the user.

7. **403 Is a Separate Rendering, in the Same Converter**: `convertAuthErrors` (renamed from
   `convertSessionErrors` when alerting writes landed) switches on `apiErr.Code`, so a
   `*runtime.APIError{Code:403}` renders as `permissionDeniedError` — "your user lacks the required
   permission", exit **1**, never the 401's exit 2. Its suggestion must stay **operation-neutral**
   (check the user's permissions, and that `grafanapi config current-context` targets the intended
   instance — a 403 is as often the right role on the wrong instance): this converter matches *every*
   openapi 403, so `explore`, `datasources`, `config check` and `login` get the same text, and naming
   one operation's role there is wrong advice for all the others. Which role an operation needs
   belongs in its own `Long` help and its guide (alerting writes need Editor). Before that,
   a 403 rendered as "Unexpected error … (status 403): {}" — `runtime.APIError.Response` marshals
   to `{}`, so Grafana's own "Permissions needed: …" text is **not** recoverable through the
   generated client. The k8s `IsForbidden` branch is a different path and is unchanged. Two
   caveats carry over from 401: `convertNetworkErrors` runs first and swallows anything wrapped in
   a `*url.Error`, and a typed generated response (the folders client) must be converted with
   `internal/alerts.asAPIError` or `errors.As` misses it entirely.

### Future Architecture Directions

Based on TODO comments in code:

1. **Proper Manager Kind**: Currently uses kubectl as placeholder
2. **Resource Versioning**: Proper resourceVersion handling in updates
3. **Subresource Support**: Currently excluded, but may be needed
4. **Three-Way Merge**: Currently basic upsert, could use kubectl-style apply
5. **Timestamp/Checksum**: Manager metadata could include more provenance data

## Development Priorities & Technical Debt

### Current State Assessment (as of 2025-10-31)

**Quality Scores**:
- Architecture Quality: 9/10 (Excellent)
- Code Quality: 8/10 (Very Good)
- Testing Coverage: 6/10 (Moderate - needs improvement)
- Documentation: 9/10 (Excellent)
- Maintainability: 8/10 (Very Good)
- Security: 7/10 (Good - improvements needed)

**Key Metrics**:
- **Test Coverage**: Estimated 40-50% (target: 70%+)
- **Integration Tests**: None (docker-compose setup exists but unused)
- **Performance Limit**: ~10,000 resources before memory pressure (~1MB per 100 dashboards)
- **Scalability**: Hardcoded QPS=50, Burst=100 in rest.go:26-27
- **Overall Assessment**: Production-ready with identified improvement areas

### Testing Gaps

**Current Coverage Focus**:
- Selector parsing and resolution
- Filter matching logic
- Configuration loading and validation
- Resource grouping and sorting
- Manager field processing

**Missing Test Areas**:
- ❌ Integration tests with real Grafana instances (despite docker-compose setup)
- ❌ Push/Pull error scenarios and edge cases
- ❌ Concurrency edge cases (errgroup error propagation, cancellation)
- ❌ Discovery failure handling
- ❌ TLS configuration scenarios
- ❌ Live reload server functionality
- ❌ Folder dependency sorting edge cases

**Recommendations**:
1. Add integration test suite using docker-compose
2. Create mock Grafana API with test fixtures
3. Add concurrency tests for errgroup behavior
4. Use property-based testing for selector parsing
5. Extend table-driven tests for error cases
6. Target 70%+ coverage incrementally

### Technical Debt Inventory

#### High Priority (Address in Next 2-3 Sprints)

1. **Resource Versioning** (pusher.go:284)
   - *Current*: No resourceVersion handling in updates
   - *Impact*: Medium - Can cause race conditions in concurrent updates
   - *Effort*: Medium - Standard K8s conflict detection pattern
   - *Fix*: Implement conflict detection, retry logic, proper resourceVersion tracking

2. **Integration Tests**
   - *Current*: None exist despite docker-compose test environment
   - *Impact*: High - Limits confidence in changes, no E2E validation
   - *Effort*: High - Requires test infrastructure setup
   - *Fix*: Use existing docker-compose for smoke tests, test push/pull/delete workflows

3. **Three-Way Merge** (pusher.go:285)
   - *Current*: Simple upsert logic without proper server-side apply
   - *Impact*: Medium - Can cause conflicts in multi-manager scenarios
   - *Effort*: High - Requires careful implementation of kubectl-style apply
   - *Fix*: Implement field manager metadata, conflict resolution, force-conflicts flag

#### Medium Priority (Address in 6 Months)

4. **Manager Kind Placeholder** (resources.go:19)
   - *Current*: Uses "kubectl" as placeholder manager kind
   - *Impact*: Low - Functional but inaccurate in metadata
   - *Effort*: Low - String replacement and testing
   - *Fix*: Replace with "grafanapi" as proper manager identifier

5. **Test Coverage Improvement**
   - *Current*: Estimated 40-50% coverage
   - *Impact*: Medium - Some code paths untested
   - *Effort*: Medium - Incremental improvement over time
   - *Fix*: Add tests incrementally, focus on critical paths first

6. **Subresource Support Decision**
   - *Current*: Subresources containing "/" excluded in discovery (registry.go:212)
   - *Impact*: Low - May limit future extensibility
   - *Effort*: Medium - Requires design decision and implementation
   - *Fix*: Evaluate use cases, implement if needed, document decision

7. **Config File Permission Validation**
   - *Current*: Docs mention 0600 permissions but no code enforcement
   - *Impact*: Medium - Security risk for credential exposure
   - *Effort*: Low - Add validation on config load
   - *Fix*: Check file permissions, warn or error if too permissive

#### Low Priority (Address Opportunistically)

8. **Hardcoded Configuration Values** (rest.go:26-27)
   - *Current*: QPS=50, Burst=100 hardcoded
   - *Impact*: Low - Works for most use cases
   - *Effort*: Low - Add config fields and CLI flags
   - *Fix*: Make configurable via flags or config file

9. **Global Variables** (discovery/registry.go:19)
   - *Current*: `ignoredResourceGroups` as global var with lint exception
   - *Impact*: Low - Reasonable use but affects testability
   - *Effort*: Low - Move to configuration struct
   - *Fix*: Refactor into registry configuration

10. **Error Wrapping Consistency**
    - *Current*: Mixed use of wrapped and unwrapped errors
    - *Impact*: Low - Inconsistent error handling patterns
    - *Effort*: Low - Standardize on `fmt.Errorf` with `%w`
    - *Fix*: Establish error wrapping guidelines, apply consistently

11. **User Agent Configuration** (TODO in code)
    - *Current*: User agent not configurable
    - *Impact*: Low - Useful for tracking/debugging
    - *Effort*: Low - Add config option
    - *Fix*: Add user agent configuration support

12. **Alerts Resource Group** (discovery/registry.go:53)
    - *Current*: TODO comment to verify with alerting team
    - *Impact*: Low - May incorrectly filter alerts resources
    - *Effort*: Low - Verification and potential fix
    - *Fix*: Confirm with team, adjust filter if needed

13. **Raw-HTTP Seam Duplicated Across Four Packages**
    - *Current*: `internal/alerts/state.go` and `internal/explore/run.go` are ~90 lines of the same
      shape, not just the URL helper: `readBodySnippet` + `maxErrorBodySnippet` are byte-identical,
      as are the request-construction block (transport → `WrapWithSession` → bounded-timeout
      client) and the 200/401/default status tree. The trim-slash-and-append URL helper itself
      exists four times: `buildQueryURL` (`internal/explore/run.go`), `buildUserURL`
      (`internal/session/session.go`), `buildRotateURL` (`internal/config/session_source.go`) and
      `buildStateURL` (`internal/alerts/state.go`)
    - *Impact*: Low - each copy is independently tested and `dupl` (enabled in `.golangci.yaml`)
      does not fire on them; the risk is drift, e.g. a 401-handling fix landing in one copy only
    - *Effort*: Medium - one `internal/httputils.GetJSON(ctx, gCtx, path, op, out)` owning the URL
      join, org header, session wrap, timeout and status tree, then migrating all four call sites
      (each has its own error-message prefix and one bespoke branch, so the helper needs an
      operation label and a way to hand back the raw non-2xx body)
    - *Fix*: Extract when the planned read-only `grafanapi api PATH` passthrough group lands — that
      group adds a fifth call site and is the natural home for the shared seam. Deliberately
      deferred (YAGNI) until then: extracting it for a single new caller would leave a one-user
      helper next to three unmigrated copies

14. **Alerting Write Limits (`alerts pull`/`push`, `alerts rules pause|resume|delete`)**
    - *Current*: five accepted limits, all documented in the alerting guide and the command help
      rather than worked around silently:
      (a) **no `--prune`** — groups, contact points and policies that exist only in Grafana are
      left alone, so a push can add and replace but never remove an object the bundle omits;
      (b) **no optimistic concurrency** — the provisioning API has no ETag/version and ignores
      `If-Match`, so the last writer wins and two people pushing the same bundle cannot be
      serialised client-side;
      (c) **`isPaused` only persists through the group PUT** on Grafana 12.3.x — a single-rule
      `PutAlertRule` with only `isPaused` flipped returns 200 but does not persist for an Editor
      (5/5, both directions; Admin persists it), so pause/resume rewrites the whole group;
      (d) **objects with foreign provenance are refused** (`file`, `converted_prometheus`,
      `disabled`, …) rather than overwritten — correct, but it means a file-provisioned instance
      cannot be managed with this group at all;
      (e) **a field the vendored model does not declare is dropped on pull and wiped on push** —
      the bundle is encoded from the generated models, so a rule field a newer Grafana added
      (`metadata.editor_settings` is the known one) never reaches the file, and the push then
      sends a body without it, resetting it server-side. Note this is about *fields*, not values:
      values inside a known field round-trip fine since `ReadBundle` validates structurally rather
      than against the generated enums (see `files.go` above)
    - *Impact*: Low - (a) and (d) are deliberate safety choices; (b) is a server-side gap; (c) is a
      Grafana bug we work around correctly; (e) is bounded by how current the vendored client is
    - *Effort*: Low for (a) (a `--prune` flag plus `Delete` actions in `Plan`, which is why
      `Action` has no `Delete` value yet); none for (b)/(c) until Grafana changes; Low-to-Medium
      for (e) (re-vendor the client, or decode rules through a passthrough map the way the
      encoder already round-trips one)
    - *Fix*: add `--prune` when someone needs it; re-test (c) on each Grafana minor and collapse
      pause/resume to a single `PutAlertRule` once it persists for an Editor; for (e), re-vendor
      `grafana-openapi-client-go` when a dropped field actually bites

### Performance Bottlenecks & Recommendations

**Current Limitations**:
- **Memory**: All resources loaded into memory (no streaming)
- **Estimated Usage**: ~1MB per 100 dashboards
- **Practical Limit**: ~10,000 resources before memory pressure
- **Rate Limits**: QPS=50, Burst=100 (hardcoded, not configurable)
- **Connection Pooling**: No visible configuration

**Recommendations**:
1. **Add Streaming Support**: For large resource sets (>1000 resources), implement chunked processing
2. **Configurable Rate Limits**: Expose QPS/Burst as CLI flags or config options
3. **Progress Indicators**: Add for long-running operations (push/pull 1000+ resources)
4. **Caching**: Consider caching discovery results to disk for faster startup
5. **Memory Profiling**: Add `--profile` flag for memory/CPU profiling during operations
6. **Adaptive Rate Limiting**: Implement backoff and retry with adaptive rate adjustment

### Security Recommendations

**Current Security Posture**:
- ✅ Session cookie authenticated (`Cookie: grafana_session=...`), stored in the macOS Keychain
- ✅ Config file never contains the secret; only TLS key data is `datapolicy:"secret"`-tagged
- ✅ Cookie never accepted via flag or environment variable (interactive prompt only)
- ✅ TLS verification enabled by default
- ✅ Custom CA certificates and client certificate authentication supported

**Security Gaps & Improvements**:

1. **Config File Permission Validation** (High Priority)
   - *Issue*: No explicit file permission check in code
   - *Risk*: Non-secret context data (server, org-id/stack-id) exposed if file permissions too permissive
   - *Fix*: Add validation on config load, warn if not 0600

2. **Secret Encryption at Rest** — ✅ Addressed
   - The session cookie is stored in the macOS Keychain (ACL-only, bound to
     the binary's ad-hoc cdhash) rather than in the plaintext config file.
   - Remaining scope: this is macOS-only; there is no equivalent secret store
     for other platforms (distribution is macOS/arm64-only by design).

3. **Insecure TLS Warning** (Medium Priority)
   - *Issue*: TLS verification can be disabled with `Insecure: true`
   - *Risk*: Man-in-the-middle attacks if used in production
   - *Fix*: Add prominent warnings when `insecure-skip-verify` is used

4. **Input Validation** (Low Priority)
   - *Current*: Relies on Grafana API validation (no client-side schema validation)
   - *Risk*: Poor error messages, unnecessary network round-trips
   - *Fix*: Implement client-side schema validation using OpenAPI specs

### Code Quality Standards

**Current Strengths**:
- Clean separation of concerns (no circular dependencies)
- Consistent options pattern for commands
- Thoughtful error handling with custom types
- Minimal, focused interfaces for testability
- Good adherence to Go conventions

**Areas for Improvement**:

1. **Error Wrapping Consistency**
   - Standardize on `fmt.Errorf` with `%w` for all error wrapping
   - Ensure error chains are preserved for debugging

2. **Magic Numbers**
   - Extract hardcoded values to named constants
   - Examples: QPS/Burst limits, concurrency defaults, timeouts

3. **Documentation Comments**
   - Ensure all exported functions have GoDoc comments
   - Document non-obvious behavior and edge cases

4. **Test Coverage**
   - Require 70%+ coverage for new code
   - Add integration tests for new workflows
   - Use table-driven tests for all parsing/validation logic

### Prioritized Roadmap

**Short-term (0-6 months)**:
1. ✅ Add integration test suite with docker-compose
2. ✅ Implement resource versioning with conflict detection
3. ✅ Add config file permission validation
4. ✅ Improve test coverage to 70%+
5. ✅ Add progress indicators for long-running operations
6. ✅ Implement client-side schema validation using OpenAPI specs

**Medium-term (6-12 months)**:
1. 🔄 Implement proper three-way merge with server-side apply semantics
2. 🔄 Add streaming support for large resource sets
3. 🔄 Implement watch operations for real-time updates
4. 🔄 Add diff command (show changes before push, like kubectl diff)
5. 🔄 Replace manager kind placeholder with "grafanapi"
6. 🔄 Make rate limits configurable

**Long-term (12+ months)**:
1. 🔮 Plugin system for custom resource handlers
2. 🔮 Multi-cluster support (manage multiple Grafana instances simultaneously)
3. 🔮 GitOps integration (native support for flux/argocd patterns)
4. 🔮 Resource templates (template rendering similar to Helm)
5. 🔮 Advanced filtering (label selectors, field selectors)
6. 🔮 Batch operations with rollback support

### Comparative Analysis

**vs kubectl**:
- ✅ grafanapi successfully adopts kubectl UX patterns
- ✅ Context-based configuration matches kubectl
- ✅ Resource abstraction similar to K8s objects
- ❌ kubectl has more mature three-way merge
- ❌ kubectl has extensive plugin ecosystem

**vs terraform**:
- ✅ grafanapi is lighter-weight and dashboard-focused
- ✅ Better live development experience (serve command)
- ❌ terraform has state management and planning
- ❌ terraform has broader resource support

**vs grizzly**:
- ✅ grafanapi uses standard K8s patterns vs custom
- ✅ grafanapi has discovery system
- ❌ grizzly has longer history and maturity
- ❌ grizzly has template support

**Unique Value Proposition**:
1. K8s-native patterns for Grafana resources
2. Dynamic discovery of available resources
3. Live reload server for development workflows
4. Official Grafana Labs tool (better integration and support)

**Ideal Use Cases**:
- Dashboards-as-code workflows
- GitOps for Grafana resources
- Multi-environment Grafana management (dev/staging/prod)
- CI/CD automation
- Development with live preview

**Not Ideal For**:
- One-off dashboard edits (UI is faster)
- Non-technical users
- Grafana <12 (not supported)
- Extremely large deployments (>10,000 resources without streaming)

## Go Code Organization Standards

When writing Go code, always organize code symbols in a given file in this order:

1. exported constants
2. unexported constants
3. exported variables
4. unexported variables
5. exported functions
6. exported interface definitions
7. exported type definitions
8. exported methods
9. unexported methods
10. unexported interface definitions
11. unexported functions