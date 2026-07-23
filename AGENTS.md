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

grafanapi follows the Cobra command pattern with seven main command groups:

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
   - `resources serve`: Serve resources locally with live reload

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

### Core Packages

**cmd/grafanapi/** - CLI command implementations
- `root/`: Root command setup with logging and flags
- `login/`: `login` / `login update` — interactive session-cookie authentication
- `config/`: Configuration management commands
- `resources/`: Resource manipulation commands
- `explore/`: `explore` — thin Cobra wiring (flags, a `table` `format.Codec` adapter) over
  `internal/explore`; no domain logic lives here
- `datasources/`: `datasources` — thin Cobra wiring (flags, a `table` `format.Codec` adapter)
  calling the generated Grafana client directly; no domain logic to extract for a single list call
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
- `fail/`: Error handling and detailed error messages
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
- `render.go`: `RenderTable` — `text/tabwriter` rendering for the default `table` output format
- **Deliberate raw-HTTP decision**: the query path decodes the raw `/api/ds/query` response body
  into these local structs instead of calling the generated client's
  `Datasources.QueryMetricsWithExpressions`, because the vendored `models.Frame`/`models.Field`
  have no `schema`/`data` split and no values field at all — the go-openapi consumer discards every
  column value (`data.values`) while decoding, so its payload cannot be re-marshalled into the real
  wire shape. Datasource **lookup** still goes through the generated client, since its response
  models (`models.DataSource`) round-trip correctly; only the query response is lossy.

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
  `cmd/grafanapi/fail.convertSessionErrors`
- Only used by `login`/`login update` (which always paste a fresh cookie and validate it
  directly) — unrelated to `internal/config`'s `SessionSource` rotation, which only ever kicks in
  for *already-authenticated* commands hitting a stale cookie mid-session

**internal/secrets/** - Secret management
- Secure handling of sensitive configuration data (currently just TLS key data; the session cookie is never serialized to disk)

**internal/logs/** - Structured logging
- slog-based logging with verbosity levels
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
  `SetKeychainStore(store)` / `SetPrompter(p)` function, each returning a restore closure to
  `defer`. This keeps commands free of dependency-injection plumbing while still allowing tests to
  avoid the real Keychain/TTY.

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

**grafanapi** is a ~12,500 line Go codebase that provides a kubectl-like interface for managing Grafana resources. It bridges Grafana's REST API with Kubernetes-style resource management patterns, enabling Infrastructure-as-Code workflows for Grafana dashboards, folders, and other resources.

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
   - Reverse proxy to Grafana
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

**Network Security**:
- TLS verification enabled by default
- Option to disable (insecure-skip-verify) for dev/test
- Custom CA certificates supported
- Client certificate authentication supported

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
   `session.ErrUnauthorized`. `cmd/grafanapi/fail.convertSessionErrors` maps the two differently:
   the bare sentinel renders as the login-rejected message ("Grafana rejected the provided session
   cookie" — meant for `login`/`login update`), while `*runtime.APIError{Code:401}` renders as the
   stale-session message ("Grafana session is stale or unauthorized" / `login update` suggestion).
   Returning the wrong one shows the wrong message to the user.

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