# `grafanapi alerts` — read-only alert rules and contact points

## Overview

**What:** A new top-level command group exposing Grafana Unified Alerting read-only from the terminal:

```
grafanapi alerts rules list [--folder UID|PATH|LAST-SEGMENT] [--state firing|pending|recovering|normal|inactive|unknown|recording|unavailable] [-o table|json|yaml]
grafanapi alerts contact-points list [-o table|json|yaml]
```

`rules list` joins rule **definitions** (`GET /api/v1/provisioning/alert-rules`, generated client)
with live **evaluation state** (`GET /api/prometheus/grafana/api/v1/rules`, raw HTTP) on rule `uid`,
so one command answers both "what is defined" and "what is firing". `contact-points list` lists
notification contact points (`GET /api/v1/provisioning/contact-points`, generated client); the
server already redacts secrets (`"[REDACTED]"`), so output is safe to print.

**Why:** Grafana's own CLI (gcx) offers `alert rules list` / `alert contact-points list`; grafanapi
had no alerting support at all. Adding it here keeps one tool and one auth model (user session
cookie in the Keychain). The structure parallels gcx; the group is named `alerts` (plural) to match
grafanapi's own `resources`/`datasources` convention.

**Scope (group 1 of 3):** read-only only. No create/update/delete (that is the "admin level" the
user excluded), no `api` passthrough (group 2), no change to `resources` discovery (group 3).

**Integration:** thin Cobra wiring like `cmd/grafanapi/datasources` + a small `internal/alerts`
domain package like `internal/explore`. Reuses `grafana.ClientFromContext` (rotate-on-401,
`BasePath=/api`, org-id, TLS) and the raw-HTTP seam `httputils.NewTransport` +
`GrafanaConfig.WrapWithSession` for the one endpoint the generated client lacks. Exit-code contract
holds: 401 after rotation → `*runtime.APIError{Code:401}` → `fail.convertSessionErrors` → exit 2.

## Context (from discovery)

Findings report (2026-09-09, verified on a local `grafana/grafana:12.3` = 12.3.2 OSS with real cookie
sessions for Admin/Editor/Viewer, plus two remote Grafana 12.3.1 OSS instances with identical
alerting toggles):

- **Auth:** every read endpoint below returns 200 to a **Viewer** session cookie; anonymous → 401.
  On the Grafana 12.3.1 OSS instances an Editor and a Viewer session both read every endpoint →
  full read parity with gcx's Editor service account.
- **Generated client** (`github.com/grafana/grafana-openapi-client-go`, module cache
  `…@v0.0.0-20251202103709-7ef691d4df1d`):
  - `client.Provisioning.GetAlertRules(opts...) (*GetAlertRulesOK, error)`,
    payload `models.ProvisionedAlertRules` (`[]*ProvisionedAlertRule`: `UID`, `Title *string`,
    `FolderUID *string`, `RuleGroup *string`, `For *strfmt.Duration`, `IsPaused`, `Labels`,
    `Provenance models.Provenance` (string type), `Record *Record` (**recording rules are included**),
    `Updated strfmt.DateTime`, `Data []*AlertQuery`, …; many pointer fields — deref carefully).
  - `client.Provisioning.GetContactpoints(params *GetContactpointsParams, opts...)`, payload
    `models.ContactPoints` (`[]*EmbeddedContactPoint`: `UID`, `Name`, `Type *string`,
    `Settings models.JSON` (= `interface{}`), `DisableResolveMessage`, `Provenance`). Params has only
    an optional `Name *string` filter; nil params self-heal, but we pass
    `NewGetContactpointsParams().WithContext(ctx)` explicitly so the command context propagates.
    Likewise use `GetAlertRulesWithParams(NewGetAlertRulesParams().WithContext(ctx))` — plain
    `GetAlertRules(opts...)` never sees `ctx` and `contextcheck` is enabled.
  - **Error shape (verified):** both response readers have only `case 200` + `default:
    runtime.NewAPIError(..., response.Code())` (`get_alert_rules_responses.go:33`,
    `get_contactpoints_responses.go:33`). There is **no** typed `…Unauthorized` response — every
    non-200, including 401, is a `*runtime.APIError` carrying the real status code. No mapping
    helper is needed. Caveat: `APIError.Error()` marshals the opaque `ClientResponse`, so a 500
    reads `… (status 500): {}` with no server message.
  - **No** generated service for `/api/prometheus/grafana/api/v1/rules` (there is no
    `client/prometheus`); `models.RuleDiscovery` / `RuleGroup` / `AlertingRule` exist and would
    round-trip the fields we need, but nothing calls them and they don't model the `{status,data}`
    envelope. Wire shape: `{status, data:{groups:[{name, file (folder title), folderUid, interval,
    rules:[{name, uid, type ("alerting"|"recording"), state (alerting only:
    inactive|pending|firing|recovering), health, lastEvaluation, evaluationTime, totals{...},
    alerts:[{state: Normal|Alerting|Pending|Recovering|NoData|Error, ...}]}]}]}}`. Note the `alerts`
    array can contain Normal instances, so "active" must be computed, not `len(alerts)`.
- **Existing patterns:** `cmd/grafanapi/datasources/{command.go,table.go,command_test.go}` (thin
  wiring, `tableCodec`, `Args: cobra.NoArgs`, `CommandTestCase` + `httptest` +
  `testutils.NewFakeKeychainStore` + `cmdconfig.SetKeychainStore`); `cmd/grafanapi/explore/table_test.go`
  (codec tested in isolation); `internal/explore/run.go` (raw-HTTP seam, `queryTimeout`,
  `maxErrorBodySnippet`, unwrapped `runtime.NewAPIError(...,401)`); `internal/explore/run_test.go`
  and `internal/grafana/client_test.go` (domain tests build a `&config.Context{Grafana:
  &config.GrafanaConfig{Server: srv.URL, SessionCookie: "abc123"}}` literal — no temp config, no
  keychain); `cmd/grafanapi/fail/convert.go` (`convertNetworkErrors` runs before
  `convertSessionErrors`, so a 401 must not be wrapped in `*url.Error`; `fmt.Errorf("%w")` wrapping
  is fine); `cmd/grafanapi/io/format.go`; `cmd/grafanapi/root/command.go` (alphabetical
  `AddCommand`). Package docs live atop the primary file — the repo has **no** `doc.go` files.
- **URL helper duplication:** `buildQueryURL` (explore), `buildUserURL` (session), `buildRotateURL`
  (config) are three copies of trim-slash-and-append. A fourth is accepted here (YAGNI; extraction is
  earmarked for the `api` group), but `dupl` is enabled in `.golangci.yaml` — watch the lint gate.
- **Docs:** `docs/reference/cli/` is generated (`make cli-reference`, drift-checked in CI);
  hand-written guides in `docs/guides/*.md` + a card in `docs/guides/index.md`; `README.md` quick
  start lists commands; command inventory in `AGENTS.md`. **`CLAUDE.md` is a symlink to `AGENTS.md`
  — edit `AGENTS.md` only, never write `CLAUDE.md`.** `skill/grafanapi/SKILL.md` sections are
  numbered `## 1.`–`## 8.`; inserting a section means renumbering the tail.
- **Toolchain on this machine (verified 2026-09-09):** `devbox` is **not installed**, so every
  `make` target that depends on `check-binaries` (`lint`, `tests`, `build`, `deps`, `docs`,
  `cli-reference`) fails immediately. Use the underlying commands directly: `go mod vendor` (done;
  `.golangci.yaml` needs `vendor/`), `go build ./...`, `go test -v ./...`,
  `golangci-lint run -c .golangci.yaml`, `go build -o bin/grafanapi ./cmd/grafanapi`,
  `go run ./scripts/cmd-reference`, `mkdocs build` (needs `pip install -r requirements.txt`).
  `make test-env-*` targets do **not** gate on devbox and work. Local golangci-lint is 2.12.2 vs
  2.9 pinned in `devbox.json`; the unchanged tree already reports **14 findings** (saved to the
  session scratchpad `lint-baseline.txt`: gosec 5, govet 3, nolintlint 1, staticcheck 5, all in
  untouched files). Lint gate = **no new findings vs that baseline**, not zero.
- **Enabled linters that bite here:** `contextcheck` (pass `ctx` into generated calls),
  `gochecknoglobals` (no package-level `var` lookup tables — use a function or the
  `//nolint:gochecknoglobals // read-only lookup table` precedent from `fail/convert.go:37`),
  `dupl`, `ireturn`, `gosec`.
- **Integration test bed:** Makefile targets `test-env-up` (health wait), `test-env-status`,
  `test-env-clean` (`down -v`) over `docker-compose.yml` (Grafana 12.3 + MySQL, `admin:admin`);
  `testdata/integration-test-config.yaml` (context `local`, `http://localhost:3000`, org-id 1).
  `login --cookie-stdin --server … --org-id 1` accepts the cookie on stdin, so login is scriptable
  from `POST /login`. **Keychain ACL caveat:** the Keychain item trusts the code identity of the
  binary that wrote it; a rebuilt `bin/grafanapi` has a new ad-hoc cdhash and hits the 15 s
  "Allow" timeout. Therefore: build once, `login` with that exact `bin/grafanapi`, run all
  integration checks with it; after any rebuild, re-run the scripted login before checking again.

## Development Approach

- **testing approach**: Regular (code first, then tests) — user's choice
- complete each task fully before moving to the next; small focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task —
  success and error paths, table-driven where there is a matrix
- **CRITICAL: all tests must pass before starting next task** (`go test -v ./...`)
- **CRITICAL: update this plan file when scope changes during implementation**
- run `golangci-lint run -c .golangci.yaml` at **every** task gate (Tasks 1–4), compare against the
  14-finding baseline; new findings must be fixed, not `nolint`ed (except the documented
  `gochecknoglobals` lookup-table precedent)
- follow the Go symbol-ordering rule from `AGENTS.md` (exported consts → … → unexported funcs)
- no cookie value ever reaches logs, errors, or output
- `errors.As` form in tests: `var apiErr *runtime.APIError; require.ErrorAs(t, err, &apiErr)`

## Testing Strategy

- **unit tests**: `internal/alerts/*_test.go` with `httptest` servers and `config.Context` literals
  (200, 401, 500, malformed JSON, empty lists, join with missing state, recording rules, filters);
  `cmd/grafanapi/alerts/*_test.go` with `testutils.CommandTestCase` (temp config + fake keychain)
  for table/json/yaml output, flag validation, config-load failure, API error propagation, exit code
  via `fail.ErrorToDetailedError(err).ExitCode` (a `*int` — deref).
- **no e2e framework** in this repo; integration verification is a manual-but-scripted check against
  the docker Grafana (Task 6), recorded in this plan.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix
- keep plan in sync with actual work done

## Solution Overview

```
cmd/grafanapi/alerts/            thin Cobra wiring (no domain logic)
  command.go                     `alerts` parent (persistent --config/--context via cmdconfig.Options)
  rules.go                       `alerts rules` parent + `rules list` (flags, RunE)
  contactpoints.go               `alerts contact-points` parent + `contact-points list`
  table.go                       two tableCodecs (rules, contact points)
  command_test.go, table_test.go
internal/alerts/                 domain logic (no Cobra)
  rules.go                       package doc; ListRules: provisioning defs + prometheus state → []Rule
  state.go                       wire structs + FetchRuleStates (raw HTTP, explore pattern)
  contactpoints.go               ListContactPoints via generated client
  *_test.go
```

Key decisions:

1. **Join, don't pick.** `rules list` returns a flat `[]alerts.Rule` per rule: definition fields
   from provisioning plus `State`, `Health`, `LastEvaluation`, `ActiveAlerts` from the prometheus
   view, joined on `uid`. **Recording rules** (`Record != nil`) have no evaluation state by nature:
   they get `Type: "recording"`, `State: "recording"` so they never look like a broken join.
   Alerting rules with no state entry (not yet evaluated, or state endpoint failed with a non-auth
   error) get `State: "unknown"` — reviews 3 and 5 split this: the state-endpoint-failure half now
   reports `State: "unavailable"` and only for a request that does not depend on state — and the
   command still succeeds; definitions are the primary data;
   state is enrichment. A **401** from either call is fatal (exit 2). **Degraded-mode caveat:**
   `Folder` (title) comes only from the state view, so when the state call fails, titles are empty
   and `--folder <title>` matches nothing while `--folder <uid>` still works; the warn line says so
   and the guide documents it. Lists are always non-nil (`[]Rule{}` / `models.ContactPoints{}`) so
   `-o json` emits `[]`, never `null`, and `jq '.[]'` pipes keep working.
2. **Local wire structs for state**, not `models.RuleDiscovery`. The generated models are *not*
   lossy here (unlike explore's dataframes) — the choice is about the `{status,data}` envelope they
   don't model and the pointer/`strfmt` deref burden for ~8 fields. Record that nuance verbatim in
   `AGENTS.md` so nobody concludes the models are broken.
3. **Output = one flat struct, three codecs.** `-o json|yaml` encode `[]alerts.Rule` /
   `models.ContactPoints` directly; `table` codecs render fixed columns. `Settings` is only shown in
   json/yaml (already server-redacted); the table shows `NAME TYPE UID PROVENANCE`.
4. **Filters are client-side** (`--folder`, `--state`) over the joined list — no extra endpoints.
   `--folder` matches **UID or title** exactly (case-sensitive). `--state` is lowercased/trimmed,
   `normal` is aliased to `inactive`, then validated against `alerts.KnownStates()` (a function, not
   a global) = every value the column can show: `firing|pending|recovering|inactive|unknown|recording|unavailable`
   (`unavailable` added in review 5).
   `ActiveAlerts` counts alert instances whose state is **not** `Normal` (i.e. Alerting, Pending,
   Recovering, NoData, Error) — the wire `alerts` array includes Normal instances.
5. **Error text:** non-401 generated-client errors are wrapped in `internal/alerts` with an
   operation-scoped message (`alerts: list alert rules: … (status 500)`) via `%w`, so `errors.As`
   still reaches the `*runtime.APIError` and the user sees which call failed.
6. **Read-only by construction**: no write methods are wired; `GetContactpoints` gets
   `provisioning.NewGetContactpointsParams()` (no name filter flag this round — YAGNI).

## Technical Details

### `internal/alerts.Rule` (output shape)

```go
type Rule struct {
    UID            string            `json:"uid" yaml:"uid"`
    Title          string            `json:"title" yaml:"title"`
    Type           string            `json:"type" yaml:"type"`                 // alerting|recording
    FolderUID      string            `json:"folderUID" yaml:"folderUID"`
    Folder         string            `json:"folder,omitempty" yaml:"folder,omitempty"` // title from state view `file`
    RuleGroup      string            `json:"ruleGroup" yaml:"ruleGroup"`
    For            string            `json:"for" yaml:"for"`                   // "1m" (formatDuration trims trailing 0s/0m, "0s" when unset)
    IsPaused       bool              `json:"isPaused" yaml:"isPaused"`
    Provenance     string            `json:"provenance,omitempty" yaml:"provenance,omitempty"`
    Labels         map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
    Annotations    map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
    Updated        time.Time         `json:"updated" yaml:"updated"`           // always set by server
    State          string            `json:"state" yaml:"state"`               // inactive|pending|firing|recovering|unknown|recording|unavailable
    Health         string            `json:"health,omitempty" yaml:"health,omitempty"`
    LastEvaluation *time.Time        `json:"lastEvaluation,omitempty" yaml:"lastEvaluation,omitempty"` // nil when unjoined
    ActiveAlerts   int               `json:"activeAlerts" yaml:"activeAlerts"` // instances with state != Normal
}
```

`-o yaml` note (**corrected in Task 4**): `Rule` has explicit yaml tags.
`models.EmbeddedContactPoint` has only json tags — but goccy/go-yaml falls back to the json tag
when a field has no yaml tag, so `contact-points list -o yaml` emits the same camelCase keys as
`-o json` (`disableResolveMessage`, …) and honours `omitempty`. Keys are **not** sorted: goccy
walks fields in declaration order, and `EmbeddedContactPoint` merely happens to declare them
alphabetically (corrected during review 1 — the earlier "sorts keys alphabetically" claim was
wrong, and `Rule` output is visibly in declaration order: `uid, title, type, folderUID, folder,
ruleGroup, for, isPaused, provenance, labels, annotations, updated, state, health, lastEvaluation,
activeAlerts`). There is no key-name limitation to document; the key *names* are pinned by a test
either way (rules key order is not).

### Raw state call (`internal/alerts/state.go`)

- `GET {server}/api/prometheus/grafana/api/v1/rules`, `X-Grafana-Org-Id` when `OrgID != 0`,
  `httputils.NewTransport(gCtx)` wrapped with `gCtx.Grafana.WrapWithSession`, `http.Client{Timeout:
  stateTimeout (30s)}`; body decoded into `ruleDiscoveryResponse{Status string; Data struct{Groups
  []ruleGroup}}`; 401 → `runtime.NewAPIError("alert rule state", snippet, 401)` **unwrapped**; other
  non-2xx → `fmt.Errorf` with a `maxErrorBodySnippet`-bounded snippet.
- Returns `map[string]RuleState` keyed by uid (`Folder`, `Type`, `State`, `Health`,
  `LastEvaluation *time.Time`, `ActiveAlerts` = count of `alerts[].state != "Normal"`).

### Provisioning calls

- `gClient.Provisioning.GetAlertRulesWithParams(provisioning.NewGetAlertRulesParams().WithContext(ctx))`
  → `models.ProvisionedAlertRules`; convert each via `ruleFromProvisioned(*models.ProvisionedAlertRule)
  Rule` (nil-safe derefs; `For` via `formatDuration(time.Duration(*r.For))` which trims a trailing
  `0s`/`0m` so `1m0s` → `1m`, `1h0m0s` → `1h`, zero → `0s`; `Updated` via `time.Time(r.Updated)`;
  `Provenance` via `string(r.Provenance)`; `Type = "recording"` iff `r.Record != nil`).
- `gClient.Provisioning.GetContactpoints(provisioning.NewGetContactpointsParams().WithContext(ctx))`
  → `models.ContactPoints`, sorted by name, never nil; encoded as-is for json/yaml.
- Errors: every non-200 is already `*runtime.APIError` (see Context). 401 is returned as-is
  (`%w`-wrapped is fine); other codes are `%w`-wrapped with the operation name.

### Table output

```
FOLDER        GROUP        TITLE       UID         TYPE      STATE     HEALTH  FOR  PAUSED  ALERTS
Probe Folder  probe-group  probe-rule  rule-uid-1  alerting  inactive  ok      1m   false   0
```
```
NAME            TYPE   UID       PROVENANCE
email receiver  email
probe-slack     slack  cp-uid-1  api
```
`text/tabwriter` with `0, 4, 2, ' ', tabwriter.TabIndent|tabwriter.DiscardEmptyColumns`, sorted by
folder → group → title (rules) and name (contact points). Nil-safe derefs: `EmbeddedContactPoint.Type`
is `*string`.

### Flags

- `alerts rules list`: `Args: cobra.NoArgs`; `-o/--output` (default `table`), `--folder` (exact,
  case-sensitive match on `FolderUID` **or** `Folder` title), `--state` single value, lowercased,
  from `{firing,pending,recovering,inactive,normal,unknown,recording,unavailable}`; `Validate()` rejects
  anything else.
- `alerts contact-points list`: `Args: cobra.NoArgs`; `-o/--output` (default `table`).
- Parents `alerts`, `alerts rules`, `alerts contact-points` have no `RunE` (print help, exit 0 —
  as `session` does). `alerts` carries `--config` / `--context` via `cmdconfig.Options` bound as
  persistent flags (same as `resources`).

## Implementation Steps

### Task 1: `internal/alerts` — rule state wire structs and fetch

**Files:**
- Create: `internal/alerts/state.go`
- Create: `internal/alerts/state_test.go`

- [x] `state.go`: package doc comment (domain logic for `alerts`, no Cobra; expanded in Task 2),
      `RuleState` type (`Folder, Type, State, Health string; LastEvaluation *time.Time;
      ActiveAlerts int`), unexported wire structs (`ruleDiscoveryResponse`, `ruleGroup`,
      `alertingRule`, `alertInstance{State string}`), `stateTimeout`/`maxErrorBodySnippet` consts,
      `buildStateURL(server)`, `ActiveAlerts` = instances with `state != "Normal"`
- [x] `FetchRuleStates(ctx, gCtx *config.Context) (map[string]RuleState, error)`: transport via
      `httputils.NewTransport` + `gCtx.Grafana.WrapWithSession`, `X-Grafana-Org-Id`, status
      handling (200 decode; 401 → unwrapped `runtime.NewAPIError`; other → bounded snippet error);
      zero `lastEvaluation` (`0001-01-01…`) → nil
- [x] tests (`httptest` + `config.Context` literal): 200 with an alerting rule (state `firing`,
      `alerts` = one Alerting + one Normal + one Pending → `ActiveAlerts == 2`) and a recording rule
      → keyed map with `Folder` from `file`, `Type`, nil `LastEvaluation` for zero time; `status !=
      "success"` → error; malformed JSON → error; a `recovering` state passes through verbatim
- [x] tests: 401 → `var apiErr *runtime.APIError; require.ErrorAs` with `Code == 401` and not a
      `*url.Error`; 500 with long body → error contains truncated snippet; org-id header set/unset
- [x] run `go test ./internal/alerts/` and `golangci-lint run -c .golangci.yaml` (no new findings
      vs baseline) — must pass before Task 2

➕ **Task 1 notes:**
- The `alertingRuleType`/`recordingRuleType` consts planned for `state.go` were dropped from this
  task: nothing in `state.go` reads them (the wire `type` value is carried through verbatim), so
  `staticcheck`'s `unused` would have flagged them as a new finding. They belong in `rules.go`
  (Task 2), where the join actually branches on rule type.
- `statesByUID` skips state entries with an empty `uid` (they cannot be joined onto a provisioning
  definition, which is the map's only purpose) — covered by a test.
- ⚠️ **Lint baseline is nondeterministic**: `golangci-lint`'s default `max-same-issues: 3` reports
  only 3 instances of each repeated message, and *which* instances vary run to run. A control run
  with `internal/alerts` removed from the tree produced a different set of 14 findings than the
  saved baseline. The stable gate is therefore: **14 total, per-linter counts gosec 5 / govet 3 /
  nolintlint 1 / staticcheck 5, and zero findings in the touched paths** — all three verified for
  Task 1. Use that comparison (not a line-by-line diff) for Tasks 2–4.

### Task 2: `internal/alerts` — rules list (provisioning + join) and contact points

**Files:**
- Create: `internal/alerts/rules.go` (carries the package doc comment; points at
  `docs/plans/completed/20260909-alerts-command.md`)
- Create: `internal/alerts/contactpoints.go`
- Create: `internal/alerts/rules_test.go`
- Create: `internal/alerts/contactpoints_test.go`

- [x] `rules.go`: full package doc, `Rule` struct, `ruleFromProvisioned` (nil-safe; recording
      detection), `formatDuration`, `ListRulesOptions{Folder, State string}`, `KnownStates()
      []string` (function — `gochecknoglobals`) + `NormalizeState` (trim, lowercase, `normal` →
      `inactive`), `ListRules(ctx, gCtx, gClient, opts) ([]Rule, error)`:
      `GetAlertRulesWithParams(...WithContext(ctx))` → `FetchRuleStates` → join on uid (`unknown`
      for unjoined alerting rules, `recording` for recording rules; state entries with no
      definition are ignored) → filter (`--folder` on UID or title, exact) → sort
      folder/group/title → always return non-nil slice
- [x] state-fetch failure policy: 401 fatal (return as-is); other errors → warn via
      `logging.FromContext(ctx)` ("alert rule state unavailable; STATE shows unknown and FOLDER is
      empty for every rule") and continue with `unknown` — **but only for a request that does not
      depend on state**; see the review 3 note at the end of this plan
- [x] non-401 provisioning errors wrapped: `fmt.Errorf("alerts: list alert rules: %w", err)`
- [x] `contactpoints.go`: `ListContactPoints(ctx, gClient) (models.ContactPoints, error)` using
      `provisioning.NewGetContactpointsParams().WithContext(ctx)`, sorted by name, non-nil, same
      error wrapping
- [x] tests (`httptest` serving `/api/v1/provisioning/alert-rules` and
      `/api/prometheus/grafana/api/v1/rules`, `config.Context` literal, `grafana.ClientFromContext`):
      happy join; alerting rule without state → `unknown`; recording rule → `recording`, not
      `unknown`; state entry with no definition ignored; `--folder` by UID and by title, wrong case
      → no match; `--state firing`, `Firing` (case), `normal` alias, `recovering`, `unknown`,
      `recording`, `bogus` → error; state endpoint 500 → rules returned with `unknown`, `--state
      firing` → empty, `--folder <title>` → empty but `--folder <uid>` → match; state endpoint 401
      → `*runtime.APIError{401}`; provisioning 401 → `*runtime.APIError{401}`; provisioning 500 →
      error text contains `list alert rules` and `status 500`; empty provisioning list → `[]Rule{}`
      non-nil
- [x] tests: `ruleFromProvisioned` with all-nil pointers doesn't panic; `formatDuration` table
      (`1m0s`→`1m`, `1h0m0s`→`1h`, `90s`→`1m30s`, `0`→`0s`); unjoined rule marshals to JSON
      **without** a `lastEvaluation` key; contact points sorted, non-nil when empty; contact points
      401 → `*runtime.APIError{401}`; 500 → wrapped text
- [x] run `go test ./internal/alerts/` and `golangci-lint run -c .golangci.yaml` (no new findings
      vs baseline; `contextcheck` in particular) — must pass before Task 3

➕ **Task 2 notes:**
- The package doc was **moved** from `state.go` to `rules.go` (the plan's designated primary
  file) rather than duplicated: two package comments in one package is a lint/convention hazard
  (`revive` package-comments / `staticcheck` ST1000), and the repo's convention is one doc atop
  the primary file. `state.go` therefore has a one-line `package alerts` header now — a
  modification beyond Task 2's declared file list.
- `formatDuration` trims only *whole* trailing components (`m0s`/`h0m` suffixes), not a bare
  `0s`/`0m`: a naive `TrimSuffix(s, "0s")` turns `10s` into `1`. Pinned by the `10s` and
  `2h30m` rows of the duration table test.
- `ListRules` validates `opts.State` itself (normalize → `slices.Contains(KnownStates())` →
  error naming the valid values plus the `normal` alias), so the same guard covers the command
  layer and direct domain callers. The error lists `normal` even though `KnownStates()` does not
  contain it.
- Rule sort adds `UID` as a final tie-break after folder/group/title, so two identically named
  rules render in a stable order.
- Contact points sort by name then UID: Grafana allows duplicate names (a name is a UI grouping
  key, not an identifier), so name alone is not a total order. `null` payloads are normalized to
  a non-nil empty slice alongside the `nil` case.
- Shared test seam: `rules_test.go` owns `stubResponse`, `alertsServer`, `okResponse`,
  `alertRulesPath` and `contactPointsPath` for the whole `alerts_test` package
  (`contactpoints_test.go` reuses them); `statePath`/`nopStore`/`stateServer` stay in
  `state_test.go` from Task 1.

### Task 3: `cmd/grafanapi/alerts` — parent, `rules list`, rules table codec, tests

**Files:**
- Create: `cmd/grafanapi/alerts/command.go`
- Create: `cmd/grafanapi/alerts/rules.go`
- Create: `cmd/grafanapi/alerts/table.go`
- Create: `cmd/grafanapi/alerts/table_test.go`
- Create: `cmd/grafanapi/alerts/command_test.go`
- Modify: `cmd/grafanapi/root/command.go`

- [x] `command.go`: package doc ("thin Cobra wiring layer over internal/alerts"), `Command()` with
      `Use: "alerts"`, `Short`, `Long`, persistent `cmdconfig.Options.BindFlags`, adds `rules`
      (and, in Task 4, `contact-points`)
- [x] `rules.go`: `rules` parent + `list` (`Args: cobra.NoArgs`; `rulesListOptions{IO cmdio.Options;
      Folder, State string}`; `BindFlags` registers `table` codec + `DefaultFormat("table")` +
      `--folder`/`--state`; `Validate()` → `IO.Validate()` + `alerts.NormalizeState` checked against
      `alerts.KnownStates()`), `runRulesList`:
      Validate → LoadConfig → GetCurrentContext → ClientFromContext → `alerts.ListRules` →
      `Codec().Encode`; `Example:` includes an `-o json | jq` pipe
- [x] `table.go`: `rulesTableCodec` (`[]alerts.Rule`) — `Format()`/`Encode`/`Decode` per
      `datasources/table.go`, columns as in Technical Details
- [x] `root/command.go`: import + `rootCmd.AddCommand(alerts.Command())` first (alphabetical)
- [x] `table_test.go`: rules codec renders headers/rows/`unknown`/`recording`, exact `FOR` text
      (`1m`), empty list → header line only; wrong input type → `unsupported input type`; `Decode`
      errors (mirror `explore/table_test.go`)
- [x] `command_test.go` (temp config + `testutils.NewFakeKeychainStore` + `cmdconfig.SetKeychainStore`,
      test server scripting both rule endpoints): table headers + sorted rows; `-o json` decodes to
      `[]map[string]any` with `state`/`uid`, empty list → `[]` not `null`; `-o yaml` decodes;
      `--state bogus` → validation error; `--state normal`, `--folder` (UID and title) filter;
      provisioning 500 → error contains `status 500` and `fail.ErrorToDetailedError(err).ExitCode`
      is nil (→ exit 1); provisioning 401 → `*ExitCode == 2`; `--config /nonexistent` → error;
      `alerts` and `alerts rules` with no subcommand → help, exit 0
- [x] `go build ./...`, `golangci-lint run -c .golangci.yaml` (no new findings), `go test -v ./...`
      — must pass before Task 4 (if `dupl` fires on the two codecs/subcommands, prefer a small
      shared helper over `nolint`)

➕ **Task 3 notes:**
- `table.go` already carries the shared seam Task 4 needs, so the second codec cannot trip `dupl`
  or drift: a `tableFormat` const (the single source for both `RegisterCustomCodec` and
  `DefaultFormat`), `newTableWriter(io.Writer)` holding the one tabwriter configuration, and
  `errTableDecode()` for the identical `Decode` error. `dupl` did not fire on the rules codec
  alone; adding the helpers now keeps Task 4 from having to.
- `internal/alerts` does not export its `normalStateAlias`, so `cmd/grafanapi/alerts/rules.go`
  declares its own unexported copy. It is used only for the `--state` help text and the
  validation error message — never for matching, which is `alerts.NormalizeState`'s job.
- `rulesListOptions.listRulesOptions()` adapts the flags to `alerts.ListRulesOptions`, mirroring
  `explore.Options.queryOptions()`, so `runRulesList` stays a straight pipeline.
- ⚠️ Standalone-command tests see cobra's usage output: `SilenceUsage` is set on the **root**
  command only, so a validation error raised by `alerts rules list` in a `CommandTestCase` prints
  the usage block to stdout. The `--state bogus` test therefore asserts that no rule was rendered
  rather than that stdout is empty. Not a behavior change — the same is true of `datasources`.
- ⚠️ `dupword` (not in the earlier "linters that bite here" list) fired on a table-test
  expectation: a recording rule's `TYPE` and `STATE` cells are both `recording`, which reads as a
  repeated word inside a string literal. Fixed by building the expected rows from field slices
  joined with a space instead of one literal string — clearer as a column assertion anyway. Watch
  for it again in Task 4's contact-points rows.
- `table_test.go` collapses each rendered line's tabwriter padding with `strings.Fields` before
  comparing, so the assertions pin the column *contents and order* without hard-coding widths
  that any future column change would break.
- The parents-print-help table also covers `alerts --help`, since `Long`/`Short` on a parent with
  no `RunE` is the only thing a bare invocation shows.
- Verification: `go build ./...` clean; `go test $(go list ./... | grep -v internal/keychain)` all
  `ok` (keychain excluded per the toolchain note — it drives the real cgo Keychain);
  `go test -race ./cmd/grafanapi/alerts/ ./internal/alerts/` `ok`; `golangci-lint run -c
  .golangci.yaml` → **14 findings, gosec 5 / govet 3 / nolintlint 1 / staticcheck 5, zero in
  `cmd/grafanapi/alerts`, `internal/alerts` or `cmd/grafanapi/root`** (the stable gate from
  Task 1's note).

### Task 4: `cmd/grafanapi/alerts` — `contact-points list`, codec, tests

**Files:**
- Create: `cmd/grafanapi/alerts/contactpoints.go`
- Modify: `cmd/grafanapi/alerts/command.go`, `table.go`, `table_test.go`, `command_test.go`

- [x] `contactpoints.go`: `contact-points` parent + `list` (`Args: cobra.NoArgs`,
      `contactPointsListOptions{IO cmdio.Options}`, table default, `Example:` with `jq`),
      `runContactPointsList` mirroring Task 3's RunE order with `alerts.ListContactPoints`
- [x] `table.go`: `contactPointsTableCodec` (`models.ContactPoints`), nil-safe `Type` deref,
      columns `NAME TYPE UID PROVENANCE`
- [x] `command.go`: register `contact-points`
- [x] `table_test.go`: contact-points codec rows, nil `Type`, wrong input type
- [x] `command_test.go`: table headers/rows; `-o json` contains `settings` (redacted value) while
      table output does not; `-o yaml` pins the actual key shape (camelCase json tags — see the
      ⚠️ note below); empty list → `[]`; 401 → `*ExitCode == 2`; 500 → error contains `status 500`
- [x] `golangci-lint run -c .golangci.yaml` (no new findings), `go test -v ./...` — must pass
      before Task 5

➕ **Task 4 notes:**
- ⚠️ **The plan's `-o yaml` premise was wrong.** `internal/format`'s `YAMLCodec` uses
  goccy/go-yaml, which falls back to a field's **json tag** when it has no yaml tag. So
  `contact-points list -o yaml` emits the same camelCase keys as `-o json`
  (`disableResolveMessage`, `uid`, `provenance`, …), honours `omitempty` (the UID-less default
  email receiver has no `uid` key), and emits keys in field-declaration order — which for
  `EmbeddedContactPoint` coincidentally *is* alphabetical, as the generated model declares them
  that way (the original "sorts keys alphabetically" wording was corrected in review 1). It does
  **not** emit lowercased Go field names. `Test_ContactPointsListCommand_yamlOutput`
  pins that real shape (including the negative assertion on `disableresolvemessage`), so a
  future change to the codec's `yaml.UseJSONMarshaler`/tag-fallback options cannot silently
  rename every key. The "known limit" bullet and the Technical Details note below were corrected
  accordingly; Task 5's guide/release-notes items must **not** document a limit that does not
  exist. The same correction presumably applies to today's `datasources -o yaml`, which the
  original claim was extrapolated from — out of scope to verify here.
- `contactPointsTableCodec.Encode` skips `nil` slice elements rather than dereferencing them: the
  payload is `[]*models.EmbeddedContactPoint`, and `internal/alerts`' own sort accessors are
  already nil-safe for the same reason. The codec's skip was covered by a test from the start; the
  domain sort accessors' nil branches were **not** until review 1 added
  `TestListContactPoints_NilElementDoesNotPanic`.
- The wrong-input-type test feeds the codec a `[]alerts.Rule` (the other codec's input) rather
  than a string, so it also guards against the two codecs being wired to the wrong subcommand.
- Task 3's shared seam paid off exactly as predicted: `tableFormat`, `newTableWriter` and
  `errTableDecode` were reused unchanged and `dupl` did **not** fire on the second codec. Nor did
  `dupword` on the contact-points rows (no cell repeats a neighbour's value there), though the
  expected rows are still built from field slices for consistency with the rules rows.
- Parents are registered alphabetically (`contact-points` before `rules`), matching
  `root/command.go`'s convention.
- Verification: `go build ./...` clean; `go test $(go list ./... | grep -v internal/keychain)`
  all `ok`; `go test -race ./cmd/grafanapi/alerts/ ./internal/alerts/` `ok`; `golangci-lint run
  -c .golangci.yaml` → **14 findings, gosec 5 / govet 3 / nolintlint 1 / staticcheck 5, zero in
  `cmd/grafanapi/alerts` or `internal/alerts`** (the stable gate from Task 1's note).

### Task 5: Reference docs, guide, README, skill, inventory

**Files:**
- Modify: `docs/reference/cli/*` (generated)
- Create: `docs/guides/alerting.md`
- Modify: `docs/guides/index.md`, `README.md`
- Modify: `skill/grafanapi/SKILL.md`
- Modify: `AGENTS.md` (**not** `CLAUDE.md` — it is a symlink to `AGENTS.md`)

- [x] regenerate CLI reference (`rm -rf docs/reference/cli && go run ./scripts/cmd-reference`, the
      body of `make cli-reference`) and commit the new `grafanapi_alerts*.md` files; re-run and
      `git diff --exit-code docs/reference` to prove no drift
- [x] `docs/guides/alerting.md` (front-matter `title: Alerting`): what `rules list` shows (defs +
      state join; meaning of `unknown` and `recording`; `ACTIVE` counts non-Normal instances),
      degraded mode (state endpoint down → unfiltered list warns and reports `unknown`; a filtered
      one fails — see the review 3 note), `contact-points list` (secrets redacted server-side; `-o yaml` keys match `-o json` —
      do **not** repeat the retracted lowercased-field-name limit), role
      requirements (Viewer suffices for reads), `--folder`/`--state` incl. `normal` alias, `jq`
      examples; add a card to `docs/guides/index.md` (`:material-bell-outline:`)
- [x] `README.md`: add `alerts rules list` / `alerts contact-points list` to the quick-start list
- [x] `SKILL.md`: insert section "Alerting (`alerts`)" after "Discover datasources"/"Run queries"
      block and renumber `## N.` headings; add to "Common one-liners"; note exit-2 contract applies
- [x] `AGENTS.md`: command group 8 `alerts` in "Command Structure"; `cmd/grafanapi/alerts/` and
      `internal/alerts/` in "Core Packages"; "Alerts Flow" under Execution Flows; extend Key Design
      Patterns §6 with the nuance from Solution Overview §2 (models not lossy — envelope/ergonomics
      decision); mention `dupl`/URL-helper duplication as tracked debt for the `api` group
- [x] `mkdocs build` (after `pip install -r requirements.txt` if needed) succeeds without warnings
      for the new page

➕ **Task 5 notes:**
- **CLI reference:** `make cli-reference` is unusable here (no devbox), so its body ran directly:
  `rm -rf ./docs/reference/cli && go run scripts/cmd-reference/*.go "./docs/reference/cli"`. Five
  new pages (`grafanapi_alerts{,_rules,_rules_list,_contact-points,_contact-points_list}.md`) plus
  one modified (`grafanapi.md` gained the `alerts` SEE ALSO line). No-drift was proved twice: a
  second generation into a scratch directory is byte-identical (`diff -r`), and after staging,
  a third generation leaves `git diff --exit-code docs/reference` clean — the same comparison
  `make cli-reference-drift` performs in CI.
- **README:** the two commands went into a **new** `## Example: inspect alerting` section rather
  than into the existing `## Example: query a datasource` shell block. That block is a
  datasource-query walkthrough, not a command inventory, and dropping `alerts` commands into it
  would misfile them; the new section mirrors the shape of the adjacent
  `## Example: keep a session alive` section (three commands + a guide link).
- **SKILL.md:** beyond the new "5. Alerting (`alerts`)" section and the 6→9 renumbering of the
  tail, two smaller edits were needed for the section to be reachable and consistent: the
  frontmatter `description` gained alerting triggers ("check which Grafana alert rules are defined
  or firing, list notification contact points") — a skill section that nothing can trigger is dead
  weight — and §1's authenticated-command list now reads
  `datasources`, `explore`, `alerts ...`, `resources ...` so the exit-2 contract visibly covers the
  new group. Three one-liners were added (`--state firing`, a state+title `jq` pipe, a contact-point
  `jq` pipe).
- **AGENTS.md:** the URL-helper duplication is recorded as **Technical Debt Inventory item 13**
  (Low Priority, "Trim-Slash URL Helper Duplicated Four Times", fix earmarked for the `api` group),
  cross-referenced from a bullet in the new `internal/alerts/` Core Packages entry. No "Known
  Issues" section was added (user rule). `CLAUDE.md` re-verified as a symlink to `AGENTS.md` after
  the edits.
- **`-o yaml`:** Task 4's retraction was honoured everywhere — the guide's "Output formats" section
  states positively that json and yaml use the *same* camelCase keys (so a `jq` filter translates
  to `yq`); no lowercased-field-name limit appears in the guide, README, SKILL.md or AGENTS.md.
  (Review 1 correction: the guide's "yaml sorts keys alphabetically" sentence was wrong and now
  states that both formats emit keys in field-declaration order.)
- `mkdocs.yml` has no explicit `nav` and uses `mkdocs-nav-weight`, but no guide carries a `weight`
  front-matter key — so the new page needs no ordering key and the card order in
  `docs/guides/index.md` (alerting inserted after "Query datasources") is what readers actually
  see.
- Verification: `go build ./...` clean (SKILL.md is `go:embed`-ed, so a docs-only change still
  has to compile); `mkdocs build --strict` into a scratch output dir → exit 0, zero warnings, with
  `guides/alerting/index.html` and `reference/cli/grafanapi_alerts_rules_list/index.html` present.
  No Go source was touched, so the lint/test gates from Tasks 1-4 stand unchanged.

### Task 6: Integration verification against docker Grafana 12.3

**Files:**
- none in repo (scripted checks; results recorded in this plan)

- [x] `make test-env-up`
- [x] seed with an `admin:admin` cookie jar (`curl -s -c jar -H 'Content-Type: application/json'
      -d '{"user":"admin","password":"admin"}' localhost:3000/login`), all `POST` with
      `-b jar -H 'Content-Type: application/json'`:
      - `/api/datasources`
        `{"name":"testdata","type":"grafana-testdata-datasource","access":"proxy","uid":"testdata"}`
      - `/api/folders` `{"uid":"probe-folder","title":"Probe Folder"}` (uid is fixed here, so
        `--folder probe-folder` is the UID check and `--folder "Probe Folder"` the title check)
      - `/api/v1/provisioning/contact-points`
        `{"name":"probe-slack","type":"slack","settings":{"url":"https://hooks.slack.com/services/T000/B000/SECRET","recipient":"#probe"}}`
      - `/api/v1/provisioning/alert-rules` (twice: once as-is → `provenance: api`, once with
        `-H 'X-Disable-Provenance: true'` and `"title":"probe-rule-ui"` → empty provenance, to
        exercise both renderings):
        `{"title":"probe-rule","ruleGroup":"probe-group","folderUID":"probe-folder","condition":"C","for":"1m","noDataState":"NoData","execErrState":"Error","labels":{"team":"probe"},"annotations":{"summary":"probe"},"data":[{"refId":"A","relativeTimeRange":{"from":600,"to":0},"datasourceUid":"testdata","model":{"refId":"A","scenarioId":"csv_metric_values","stringInput":"1,20,90,30,5,0"}},{"refId":"C","relativeTimeRange":{"from":0,"to":0},"datasourceUid":"__expr__","model":{"refId":"C","type":"threshold","expression":"A","conditions":[{"evaluator":{"type":"gt","params":[0]}}]}}]}`
- [x] `go build -o bin/grafanapi ./cmd/grafanapi`; scripted login with **that** binary:
      `curl -s -c jar -H 'Content-Type: application/json' -d '{"user":"admin","password":"admin"}' localhost:3000/login`
      → extract `grafana_session` from the jar → `bin/grafanapi --config testdata/integration-test-config.yaml login --context local --server http://localhost:3000 --org-id 1 --cookie-stdin`
      (cookie only ever on stdin; never echoed)
- [x] `bin/grafanapi --config testdata/integration-test-config.yaml alerts rules list` shows the
      both seeded rules with a real `STATE` (wait ≥1 eval interval for non-`unknown`; the threshold
      `>0` on `1,20,90,…` should end up `firing`) and `PROVENANCE` `api` vs empty; `-o json | jq
      '.[0].state'` works; `--state firing` filters; `--folder probe-folder` and `--folder "Probe
      Folder"` both match
- [x] `… alerts contact-points list` shows `email receiver` + `probe-slack`; `-o json` shows
      `settings.url == "[REDACTED]"`
- [x] create a Viewer user (admin API), log in as it into a second context (`--context local-viewer`)
      with the same binary, repeat both list commands → exit 0 (Viewer parity on the real binary)
- [x] stale-session path: revoke the viewer's session server-side (`POST /api/admin/users/:id/revoke-all-auth-tokens`
      as admin), then `alerts rules list --context local-viewer` → exit **2** with the stale-session
      message
- [x] against a real (non-docker) context, read-only: `alerts rules list` and
      `alerts contact-points list` succeed with the built binary (expect a one-time Keychain
      "Always Allow" prompt since the binary changed) — record counts only. Automated attempts
      exited 1 with `keychain access timed out after 15s` until the human answered the dialog once;
      re-run afterwards against a real Grafana 12.3.1 OSS context (Editor session), read-only:
      `alerts rules list -o json` → exit 0, **35 rules**, state histogram `{firing: 3, inactive: 32}`
      (no `unknown`, no `unavailable`, so the state view joined every rule);
      `alerts contact-points list -o json` → exit 0, **10 contact points**. No other context was
      contacted.
- [x] `make test-env-clean`; record results here: ➕

➕ **Task 6 results (Grafana 12.3.2, docker, `admin/admin`):**

- **Env:** `make test-env-up` exit 0, `grafanapi-grafana` + `grafanapi-mysql` healthy,
  `/api/health` → `200 {"database":"ok","version":"12.3.2"}`. Built with
  `go build -o bin/grafanapi ./cmd/grafanapi` (exit 0) — `make build` is unusable here (no devbox).
- **Seeding:** datasource `testdata` `200`; folder `probe-folder`/"Probe Folder" `200`; contact point
  `probe-slack` `202` (already `url:"[REDACTED]"` in the POST response — the redaction is
  server-side, as documented); alert rules `201` twice — `probe-rule` → `provenance: api`,
  `probe-rule-ui` (with `X-Disable-Provenance: true`) → provenance absent.
- **⚠️ The plan's alert-rule payload cannot fire as written.** Its `data` goes straight from the
  query `A` to the threshold `C`, with no reduce step, so every evaluation failed with
  `invalid format of evaluation results for the alert definition C: looks like time series data,
  only reduced data can be alerted on` and the rules parked at `state=inactive health=error
  activeAlerts=1` (verified directly against `/api/prometheus/grafana/api/v1/rules`). Both rules were
  therefore `PUT` back with a `refId B` reduce step (`{"type":"reduce","reducer":"max","expression":"A"}`)
  and the threshold repointed at `B` — `max(1,20,90,30,5,0) = 90 > 0` → fires. `probe-rule-ui`'s
  update also carried `X-Disable-Provenance: true`, so the `api` vs empty provenance split survived
  the fix. **A future re-run of this task should seed the three-node (A→B→C) payload directly.**
- **State join / eval timing (group interval 60s, `for: 1m`):** polled `alerts rules list -o json`
  at 10s intervals — `inactive` (0-40s) → `pending` (40-90s) → first `firing` at 90s → both `firing`
  at ~100s. No `unknown` was ever observed, i.e. the provisioning↔state join worked on every poll.
- **`alerts rules list`** exit 0, both rules on one line each:
  `Probe Folder | probe-group | probe-rule | dfxr3mrwd68zka | alerting | firing | ok | 1m | false | 1`
  and the same for `probe-rule-ui`.
  - **`PROVENANCE` is not a rules-table column** (it is one only on the contact-points table). As
    shipped, provenance is a rules **payload** field: `jq -r '.[] | "\(.title) prov=\(.provenance // "<empty>")"'`
    → `probe-rule prov=api` / `probe-rule-ui prov=<empty>`, and `-o yaml` shows `provenance: api`
    present on the first rule and absent on the second. Both renderings are therefore exercised; the
    checkbox's wording just assumed a column that the Task 3 table design does not have.
  - `-o json | jq '.[0].state'` → `"firing"`. `-o yaml` → same camelCase keys as json (matches the
    Task 4 retraction). Key *order* is field-declaration order, not alphabetical — the
    "alphabetically sorted" wording here was wrong and was corrected in review 1.
  - `--state firing` → 2 rows, exit 0. `--state normal` (the alias) → header only, 0 rows, exit 0.
  - `--folder probe-folder` (UID) → 2 rows; `--folder "Probe Folder"` (the folder path, from the
    state endpoint) → 2 rows. Both forms match. Only root-level folders were seeded, so the
    nested-folder case was missed here; review 1 verified on the same Grafana 12.3.2 that `file`
    is the folder *full path* (`Parent Folder/Child Folder`, with `/` inside a title escaped as
    `\/`) and made `--folder` accept the UID, the full path or the path's last segment.
- **`alerts contact-points list`** exit 0 → `email receiver` (email, no UID, empty PROVENANCE) and
  `probe-slack` (slack, `ffxr3m4exjfuoa`, `api`); `-o json` → `settings.url == "[REDACTED]"`,
  `settings.recipient == "#probe"` (only the secret field is redacted).
- **Viewer parity:** user `viewer` created via `POST /api/admin/users` (`id: 2`), `PATCH
  /api/org/users/2 {"role":"Viewer"}` → `GET /api/org/users` confirms `viewer Viewer`. Logged into
  `--context local-viewer` with the same binary via `--cookie-stdin`. Both list commands exit **0**
  with byte-identical rows to the admin context (2 rules `firing`, 2 contact points, same redaction)
  — Viewer really does suffice for both reads, on the real binary.
- **Stale session → exit 2:** `POST /api/admin/users/2/revoke-all-auth-tokens` **does not exist on
  12.3.2** (`404 {"message":"Not found"}`, and the CLI still returned exit 0 afterwards — the session
  was untouched). The working endpoint is **`POST /api/admin/users/:id/logout`** (`200 {"message":
  "User logged out"}`, after which `GET /api/admin/users/2/auth-tokens` → `[]`). **Update this
  checkbox's endpoint for future runs.** With the session actually gone, both commands behaved as
  specified — nothing on stdout, exit **2**, and stderr:
  `Error: Grafana session is stale or unauthorized` / `Details: alerts: list alert rules: [GET
  /v1/provisioning/alert-rules] GetAlertRules (status 401): {}` / `Suggestions: • Run: grafanapi
  login update`. `contact-points list` is identical bar `[GET /v1/provisioning/contact-points]
  GetContactpoints (status 401)`. So the exit-2 contract holds on **both** endpoints against a real
  401, and rotation-then-fail leaves the `*runtime.APIError{401}` rendering intact (Common Gotcha 6).
- **Real (non-docker) context:** verified after the human answered the one-time Keychain dialog —
  rules list exit 0 (35 rules, 3 firing / 32 inactive, none `unknown`/`unavailable`), contact-points
  list exit 0 (10). Counts only; no other remote context was contacted.
- **Hygiene:** the cookie was only ever piped from the curl jar into `--cookie-stdin`, never echoed,
  logged or written to a file. All jars deleted after the run; `make test-env-clean` (removes
  volumes) exit 0. `bin/grafanapi` deliberately left on disk (gitignored) for Task 7. No Go source
  or generated docs were touched by this task. One tracked file *was* rewritten as a side effect:
  `login` normalizes the whole config file it writes, so
  `testdata/integration-test-config.yaml` lost its explanatory comment block and gained the
  `local-viewer` context — restored with `git checkout --` after the run, so only the plan is
  committed. (Worth knowing before the next integration run: point `--config` at a scratch copy.)

### Task 7: Verify acceptance criteria

- [x] `alerts rules list` and `alerts contact-points list` exist, read-only, gcx-parallel structure
- [x] `-o table|json|yaml` on both; table columns as specified; sorted output; recording rules
      never shown as `unknown`
- [x] exit 0 on success, exit 2 on 401 (both endpoints), exit 1 on other failures — asserted in
      unit tests via `fail.ErrorToDetailedError`
- [x] cookie never printed/logged; secrets only as server-redacted values
- [x] `session`/keepalive untouched (no changes under `internal/config`, `internal/keychain`,
      `cmd/grafanapi/session`)
- [x] `go build ./...`, `go test -v ./...`, `golangci-lint run -c .golangci.yaml` (no new findings
      vs baseline), CLI reference regenerated with no diff, `mkdocs build` clean

➕ **Task 7 evidence (audit only — no code changed; every criterion met, no gaps):**

- **Commands exist / read-only / gcx-parallel:** `alerts --help` lists exactly `contact-points`
  and `rules`; `alerts rules list --help` and `alerts contact-points list --help` both exit 0 with
  `Args: cobra.NoArgs` flag sets. Read-only proved structurally, not by inspection of intent: the
  only generated-client calls in the whole change are `Provisioning.GetAlertRulesWithParams`
  (`internal/alerts/rules.go:141`) and `Provisioning.GetContactpoints`
  (`internal/alerts/contactpoints.go:30`), and the only hand-rolled request is
  `http.NewRequestWithContext(ctx, http.MethodGet, …)` (`internal/alerts/state.go:61`). A grep for
  any `Post|Put|Delete|Patch` client method or non-GET `http.Method*` across
  `cmd/grafanapi/alerts` + `internal/alerts` returns nothing.
- **Formats / columns / sort / recording:** `-o` on both subcommands advertises
  `json, table, yaml (default "table")`. Column headers pinned as field slices in
  `cmd/grafanapi/alerts/table_test.go`: `rulesTableHeader()` = `FOLDER GROUP TITLE UID TYPE STATE
  HEALTH FOR PAUSED ALERTS` and `contactPointsTableHeader()` = `NAME TYPE UID PROVENANCE`, both
  matching Technical Details. Sort is asserted in the domain layer where it lives:
  `TestListRules_SortedByFolderGroupTitle` pins `u3,u2,u1,u4` (folder → group → title) and
  `TestRulesTableCodec_Encode_PreservesInputOrder` pins that the codec does not re-sort;
  `TestListContactPoints_SortedByName` + `_SameNameTieBreakByUID` cover the contact-points order.
  Recording rules: `internal/alerts/rules_test.go:219`
  (`assert.Equal(t, "recording", recording.State, "a recording rule must never read as unknown")`),
  `TestListRules_DegradedStateEndpoint` re-checks it with the state endpoint down
  (`rules_test.go:463`), and the table codec renders `recording`/`recording` for TYPE/STATE
  (`table_test.go:104`). json/yaml/table are each covered per subcommand
  (`Test_{Rules,ContactPoints}ListCommand_{tableOutput,jsonOutput,jsonEmptyListIsArray,yamlOutput}`).
- **Exit codes:** `cmd/grafanapi/main.go:29` defaults to `exitCode = 1` and only overrides it from a
  non-nil `DetailedError.ExitCode`, so "nil ⇒ exit 1" is the contract the tests assert.
  `Test_RulesListCommand_apiError401` (`command_test.go:378-382`) and
  `Test_ContactPointsListCommand_apiError401` (`command_test.go:557-561`) both
  `require.NotNil(detailed.ExitCode)` + `assert.Equal(2, *detailed.ExitCode)` + summary contains
  `stale or unauthorized`; the matching `_apiError500` tests both
  `assert.Nil(...ExitCode)` (⇒ exit 1) and assert the wrapped text (`list alert rules` /
  `list contact points` + `status 500`). The raw state call's dead-session path is
  `TestFetchRuleStates_DeadSession401` (`state_test.go:214`), which asserts
  `errors.As` to `*runtime.APIError` with `Code == 401` **and** `assert.NotErrorAs` on `*url.Error`
  — i.e. unwrapped, so `convertNetworkErrors` cannot swallow it first (Common Gotcha 6).
- **Cookie / secrets:** a case-insensitive grep for `Cookie`, `grafana_session`, `SessionCookie` or
  any header-dump pattern across the non-test sources of both new packages returns nothing — the
  cookie is only ever attached by the shared `WrapWithSession` transport, which the new code does
  not inspect. There is exactly one log call in the change,
  `internal/alerts/rules.go:152` (`logging.FromContext(ctx).Warn(stateUnavailableWarning, logs.Err(err))`),
  and the error it logs is built only from URL/status/`readBodySnippet` (`state.go:198-209`) — no
  headers. Secrets: docs state redaction is server-side in all four places
  (`docs/guides/alerting.md:152`, `skill/grafanapi/SKILL.md:160`, `README.md:57`, `AGENTS.md:151`),
  the contact-points table has no `SETTINGS` column by design (`table.go:53-55`), and
  `TestListContactPoints_SortedByName` asserts the settings payload still carries `[REDACTED]`.
- **`session`/keepalive untouched:** `git diff --stat main...HEAD -- internal/config
  internal/keychain cmd/grafanapi/session internal/launchd internal/session` → **empty**. The full
  branch diff touches only `cmd/grafanapi/alerts`, `internal/alerts`, `cmd/grafanapi/root/command.go`
  (+2 lines) and docs/plan files.
- **Gates (all run directly — `devbox` is not installed on this machine):**
  - `go build ./...` → exit 0.
  - `go test -v $(go list ./... | grep -v internal/keychain)` → exit 0, **27 packages `ok`, zero
    `FAIL`**, including `cmd/grafanapi/alerts` (1.18s) and `internal/alerts` (4.23s).
    `internal/keychain` was **excluded**: it is cgo against the real `Security.framework` and would
    prompt for Keychain access (same exclusion Tasks 3-5 used).
  - `golangci-lint run -c .golangci.yaml` → **14 findings, gosec 5 / govet 3 / nolintlint 1 /
    staticcheck 5, and zero under `cmd/grafanapi/alerts`, `internal/alerts` or `cmd/grafanapi/root`**
    — the stable per-linter gate from Task 1's note (a line-by-line diff is meaningless because
    `max-same-issues: 3` varies which instances are reported).
  - CLI reference: `rm -rf ./docs/reference/cli && go run scripts/cmd-reference/*.go
    "./docs/reference/cli"` (the exact body of the Makefile's `cli-reference` target, minus
    `$(RUN_DEVBOX)`) then `git diff --exit-code docs/reference` → **clean, no drift**; nothing had to
    be restored.
  - `mkdocs build --strict` into a scratch output dir (never into the repo) → **exit 0, zero
    WARNING/ERROR lines**, with `guides/alerting/index.html` and
    `reference/cli/grafanapi_alerts_rules_list/index.html` present.
  - Working tree after all gates: only `.grafanapi-alerts-brief.md` untracked — no regenerated docs,
    no `bin/`, no `vendor/` churn.
- `CLAUDE.md` re-verified as a symlink to `AGENTS.md` (`ls -la`) and never written.
- Task 6's real-context checkbox was blocked at this point on an interactive macOS Keychain
  "Always Allow" click; it was completed afterwards by the human (see Task 6 results).

### Task 8: [Final] Wrap up

- [x] re-read `AGENTS.md` inventory for accuracy against the shipped code
- [x] leave `.grafanapi-alerts-brief.md` untracked (user's brief, not part of the change) — never
      `git add -A`; stage files explicitly
- [x] `git mv docs/plans/20260909-alerts-command.md docs/plans/completed/`
- [ ] commit on feature branch `alerts-command`; open PR (GitHub — `avitsrimer/grafanapi`, not
      Bitbucket) — BLOCKED: PR is opened by the human author

➕ **Task 8 results (inventory reconciliation, docs only — no code changed):**

- **`AGENTS.md` audited against the shipped code**, section by section: command group 8 (rules-table
  columns vs `cmd/grafanapi/alerts/table.go:38` `FOLDER GROUP TITLE UID TYPE STATE HEALTH FOR PAUSED
  ALERTS`, contact-points columns vs `table.go:70` `NAME TYPE UID PROVENANCE`, `KnownStates()` list,
  `--folder`/`--state` semantics), the `cmd/grafanapi/alerts/` and `internal/alerts/` Core Packages
  entries (`ListRules` order, never-nil slice, `applyState`'s recording/unknown decision,
  `formatDuration` trimming, degraded-mode warning, `contactpoints.go` sort + tie-break), the Alerts
  Flow block (validate → provisioning → state → join/filter/sort → render, and the
  contact-points "steps 1, 2, 4, 5" note), Key Design Patterns §6 and Technical Debt item 13.
- **One inaccuracy found and fixed:** "Command Structure" still said *seven* main command groups
  while the numbered list runs 1-8 (`alerts` is group 8, wired at
  `cmd/grafanapi/root/command.go:66`) → now "eight". That is the only edit to `AGENTS.md`.
- **The two known corrections were already correct as shipped, so nothing to change:** `PROVENANCE`
  is a rules **payload** field and *not* a rules-table column anywhere (`AGENTS.md:144`,
  `docs/guides/alerting.md:64` explicitly lists `provenance` among the fields omitted from the
  table, `skill/grafanapi/SKILL.md`'s rules sample header, `README.md:51`), while the
  contact-points table does carry it (`AGENTS.md:151`, `alerting.md:139/147/167`,
  `SKILL.md:159`, `docs/reference/cli/grafanapi_alerts_contact-points_list.md:11`). The table
  columns named in `AGENTS.md` match `table.go` byte for byte.
- **§6 spot-checks against `vendor/`:** there is indeed no `client/prometheus` service in the
  vendored generated client (`vendor/.../client/` lists `recording_rules` but no prometheus rules
  service) while `models/rule_discovery.go`, `rule_group.go` and `alerting_rule.go` all exist — so
  the "not lossy, but no service + no envelope + deref ergonomics" wording stands. Item 13's four
  call sites all still exist (`buildRotateURL` `internal/config/session_source.go:387`,
  `buildQueryURL` `internal/explore/run.go:127`, `buildStateURL` `internal/alerts/state.go:234`,
  `buildUserURL` `internal/session/session.go:132`).
- `README.md`, `docs/guides/alerting.md` and `skill/grafanapi/SKILL.md` needed no edits, so
  `SKILL.md`'s `go:embed` content is unchanged. `mkdocs build --strict` re-run after the plan edit →
  exit 0, zero WARNING/ERROR lines. `CLAUDE.md` re-verified as a symlink to `AGENTS.md` (`ls -la`)
  and never written.
- Working tree at hand-off: `AGENTS.md` + this plan staged and committed; `.grafanapi-alerts-brief.md`
  still untracked and nothing else dirty (`git status --porcelain`).

## Post-Completion

**Manual verification:**
- run `grafanapi alerts rules list --context production` once (Viewer) and eyeball against the
  Alerting UI; confirm folder titles and states match
- after `brew upgrade` of the released binary, confirm the Keychain "Always Allow" prompt appears
  exactly once and keepalive keeps working (binary path in the LaunchAgent plist is the stable
  Homebrew symlink)

**Follow-up groups (separate plans):**
- group 2: `grafanapi api PATH` read-only passthrough — extract the shared trim-slash URL helper and
  raw-GET-JSON seam (`internal/httputils`) from explore/session/config/alerts then
- group 3: lift `notifications.alerting.grafana.app` from `ignoredResourceGroups` for read-only
  `resources list/get` of receivers/routingtrees/timeintervals/templategroups; validate pull/push
  semantics (opaque `metadata.name`, stripped secure fields) before allowing push

**Known limits to document in the release notes:**
- rules are served by the provisioning API; UI-created rules appear (empty provenance) but rules in
  folders the user cannot read are omitted server-side
- folder **paths** and evaluation states both come from the state endpoint; if it is unavailable an
  unfiltered list still works (warning + `unavailable` + empty `FOLDER`), while `--folder`, or a
  `--state` other than `unavailable`/`recording`, fails rather than returning a misleading empty
  list (see the review 5 note)
- ~~`contact-points list -o yaml` uses lowercased Go field names~~ — **not a limit**: goccy falls
  back to the generated model's json tags, so yaml keys match json (verified in Task 4); keys come
  out in field-declaration order (alphabetical for contact points only because the generated model
  declares them that way)
- `rules.alerting.grafana.app` does not exist on 12.3.x; when it lands, revisit whether `resources`
  should own rules

➕ **Review 2 note — `ActiveAlerts` must come from `alerts[]`, never from `totals`
(live-verified on Grafana 12.3.2, docker `admin/admin`):**

Review 1 replaced the instance scan with `sum(totals) - totals["normal"]` plus `?limit_alerts=0`,
to keep the response proportional to the number of *rules*. Review 2 flagged the arithmetic as
unsound; the docker env settles it. Seeded four rules in one folder — one that fires cleanly, and
three whose query `A` points at a non-existent datasource UID (so evaluation errors) differing only
in `execErrState` — each with exactly **one** alert instance:

| `execErrState` | `alerts[].state` | rule `totals` | `sum-normal` | skip-`error` | truth |
|---|---|---|---|---|---|
| `Error` (clean query) | `Alerting` | `{"alerting":1}` | 1 ✓ | 1 ✓ | 1 |
| `Error` | `Error` | `{"error":1}` | 1 ✓ | **0 ✗** | 1 |
| `OK` | `Normal (Error)` | `{"normal":1}` | 0 ✓ | 0 ✓ | 0 |
| `Alerting` | `Alerting (Error)` | `{"alerting":1,"error":1}` | **2 ✗** | 1 ✓ | 1 |

`totals` is therefore **not a partition of the instance set** — an Alerting-because-of-an-error
instance is counted under both `alerting` and `error` — and no arithmetic over it is exact for all
four rows: `sum(totals) - totals["normal"]` over-counts row 4, and treating `error` as
non-additive under-counts row 2. Nothing in the payload distinguishes the two cases, because
`execErrState` is not reported. (Review 2 predicted `{"normal":1,"error":1}` for `execErrState: OK`;
12.3.2 actually reports `{"normal":1}`. The predicted shape is kept as a fixture anyway.)

Two further shapes observed:

- **State reasons.** `alerts[].state` carries the *reason* in parentheses — `Normal (Error)`,
  and by the same rule `Normal (KeepLast)`, `Normal (Paused)`, `Alerting (KeepLast)`. So even the
  exact scan has to trim `" (…)"` before comparing against `Normal`, or every reason-carrying
  Normal instance would be counted as active. Review 1's scan did not.
- **Folder paths and backslashes.** A `/` inside a folder title is escaped: folder `a/b` under
  `Probe Folder` → `file: "Probe Folder/a\/b"`. A literal backslash is **not** escaped: folder
  `bs\` with child `Child` → `file: "bs\/Child"`, byte-identical to what a root folder titled
  `bs/Child` would produce. The ambiguity is irreducible; `folderLeaf` resolves it in favour of
  the escaped reading (one segment, `bs/Child`), which is now stated in its doc comment and pinned
  by four cases in `TestListRules_FolderFilterNestedPath`.

**Resolution:** `?limit_alerts=0` dropped, `totals` no longer decoded, `ActiveAlerts` is an exact
`alerts[]` scan with the state reason trimmed. The size-for-exactness trade is deliberate and
documented in `activeAlerts`/`buildStateURL`. Re-verified end-to-end against the same live env:
the `Alerting (Error)` rule now renders `ALERTS=1`, and the other three are unchanged.

➕ **Review 3 note — degraded mode must not answer a state-dependent question:**

Review 3 found that degraded mode turned an explicit filter into a silent false negative. With the
state endpoint failing (non-401), every rule got `State: "unknown"` and no folder path, and the
filters were then applied to those fabricated values: `alerts rules list --state firing -o json`
printed `[]` and exited **0** during a state-endpoint outage — indistinguishable from "nothing is
firing", and invisible to the `jq` recipes in the README, the guide and `SKILL.md`. `--folder` had
the same hole from the other side: paths were unavailable, so only the UID form could match.

**Resolution:** degrading is now allowed only for a request that does not depend on state.
`stateDependentFilter(folder, state)` names the offending filter, and `ListRules` turns a non-401
state failure into `fmt.Errorf("alerts: cannot apply %s without the alert rule state: %w", ...)` —
a plain error, so it renders as exit 1, while a `401` is still returned unwrapped for exit 2.

| request | state endpoint down (non-401) |
|---|---|
| `alerts rules list` | warns on stderr, all alerting rules `unavailable` (`unknown` until review 5), empty `FOLDER`, exit 0 |
| `--state unavailable` / `--state recording` | same degrade: both values are assigned without the state view |
| `--state unknown` | error, exit 1 **since review 5** — it degraded here originally, which silently redefined it as "every alerting rule" |
| `--state firing\|pending\|recovering\|inactive` (incl. the `normal` alias) | error, exit 1 |
| `--folder <uid>` / `<path>` / `<leaf>` | error, exit 1 — a UID cannot be told apart from a path or a title |

The warning text lost its now-wrong second clause (`--folder matches UIDs only` → `FOLDER is empty
for every rule`), and the guide/`SKILL.md`/`AGENTS.md`/command help were rewritten around the new
policy.

**Leaf-title matching (review 3, moderate):** `--folder <leaf>` matching every folder whose path
ends in that segment is **kept and documented** rather than dropped. It is what makes
`--folder "Probe Folder"` work for a root folder and `--folder "Child Folder"` work for the title
the Grafana UI actually shows; the cost is that two folders sharing a leaf title
(`A/Reports`, `B/Reports`) are selected together. That is a visible superset — `FOLDER` carries the
full path, and naming either path in full selects just that folder — not a silent miss.
`TestListRules_FolderFilterSharedLeafTitle` pins it.

**Sub-threshold fixes:** `cell` now also maps Unicode format characters (`Cf`, i.e. the bidi
overrides, which re-order a terminal line without changing the layout bytes); an alert instance
with no `state` at all counts as **Normal** (a malformed payload must not inflate `ALERTS`, the one
number the output carries); the cmd-layer test servers gained the catch-all handler their doc
comments already claimed, which is what surfaced the session-rotate request they now script
explicitly as a 401.

➕ **Review 4 note — `--folder` by path/leaf silently dropped unjoined rules (major):**

Review 4 found the mirror image of review 3's hole, this time with a *healthy* state endpoint.
`applyState` set `Rule.Folder` only from the rule's own per-rule state entry, so a rule the state
view did not list — one Grafana has not evaluated yet — kept `Folder == ""` and `matchesFolder`
could only match it by UID. `alerts rules list --folder "Probe Folder"` therefore omitted such
rules with no warning and exit **0**, contradicting the documented "a rule that exists but has no
state entry is still listed" policy, and explaining the blank `FOLDER` cells observed in the same
list.

The payload already carried the fix: every group object has `folderUid` next to `file` (present in
the fixtures all along), so a folder's path is resolvable *per folder*, not only per rule.

**Resolution:** `ruleGroup` now decodes `folderUid`; `FetchRuleStates` returns
`RuleStates{ByUID, FolderPaths}` (two maps, two independent joins) instead of the bare uid-keyed
map; `applyState` falls back to `FolderPaths[r.FolderUID]` on a join miss. `--folder` needed no
change once `Folder` is populated. A group carrying only one half of the mapping contributes no
entry — an entry pointing at an empty path would look resolved while matching only an empty
`--folder`.

| rule | state entry | folder in state response | `FOLDER` | `--folder <path>`/`<leaf>` |
|---|---|---|---|---|
| evaluated | yes | yes (own group) | path | matches |
| unevaluated, folder has other evaluated rules | no | yes (sibling group) | path (**new**) | matches (**new**) |
| unevaluated, *every* rule in the folder unevaluated | no | no | empty | UID only (residual) |

The residual row is irreducible — a folder with no group anywhere in the response has no path in the
payload at all — and is now documented in the guide's `--folder` section, `matchesFolder`,
`ListRulesOptions.Folder` and `AGENTS.md`.

**401-before-`stateDependentFilter` precedence (review 4, major/moderate):** the ordering was
correct but unpinned — the 401 test ran unfiltered and every `stateDependentFilter` case used a 500.
`errors.As` alone cannot pin it, since wrapping the `401` in `cannot apply ...: %w` still satisfies
`errors.As` *and* still exits 2; only the message differs, and it would tell a user with a dead
cookie to retry with fewer flags. `TestListRules_StateEndpoint401BeatsStateDependentFilter` and
`Test_RulesListCommand_stateEndpoint401WithFilter` now assert the `*runtime.APIError{401}` (and
`*ExitCode == 2` at the cmd layer) **and** `NotContains(err.Error(), "cannot apply")`; both fail
when the two branches are swapped, verified by swapping them.

**Sub-threshold fix:** the detached doc comment on `matchesFolder` (a blank line between the comment
and the func) was reattached.

➕ **Review 5 note — `unavailable` is its own state, distinct from `unknown`:**

Review 3 stopped degraded mode from answering a state-dependent filter, but left `--state unknown`
and `--state recording` exempt because "the join assigns both without the state view". Review 5
found that exemption is wrong for `unknown`, and that the fix for it also closes the last
degraded-mode false negative:

1. **`--state unknown` changed meaning during an outage.** `applyState` stamped `unknown` on *every*
   alerting rule in degraded mode, so a filter the user (and the guide) meant as "not yet evaluated"
   silently became "every alerting rule", exit 0.
2. **A degraded run was not machine-detectable.** The warning is on stderr — invisible to
   `alerts rules list -o json | jq 'map(select(.state == "firing"))'`, which returned `[]` with exit
   0. Reachable without an outage: the request sends no `limit_alerts` and the timeout is a flat
   30s, so a large org can time out on its own.

**Resolution:** the two conditions are now two values.

| value | means | assigned when |
|---|---|---|
| `unknown` | the state view **was** read and has no entry for this rule | healthy state view, rule not evaluated yet |
| `unavailable` | the state view could **not** be read | non-401 state failure, on every alerting rule |

`unavailableState` joins `KnownStates()`; `applyState(states, unjoined)` takes the value to stamp on
an unlisted alerting rule as a parameter (`ListRules` passes `unknownState` normally and
`unavailableState` after a degraded state call) rather than hardcoding it; `stateDependentFilter`
exempts `recording` and `unavailable` **only**, so `--state unknown` now fails in degraded mode
exactly like `--state firing`, and `--state unavailable` is the filter that works during an outage.
The warning text, `Rule.State`/package/`ListRulesOptions` docs, `--state` flag help (auto-derived
from `KnownStates()`), command `Long`, guide (states table, filtering, degraded mode, jq recipes),
`README.md`, `SKILL.md` and `AGENTS.md` were updated together, and the CLI reference regenerated.

**Case-insensitive `--state` matching (review 5, note):** the filter is lowercased by
`NormalizeState` while `Rule.State` carries the endpoint's value verbatim, so `matches` now compares
with `strings.EqualFold`. Grafana sends lowercase rule states today; an exact comparison would turn
an upstream casing change into "every `--state` filter matches nothing", reported as a successful
empty list. Pinned by `TestListRules_StateFilterIsCaseInsensitive`.

**Synthetic example UIDs (review 5, minor):** the sample tables in the guide, `SKILL.md`, this plan's
design section and the cmd fixtures used Grafana-generated UIDs (`ffxqra7e9kw00b`, …) from the local
docker probe. They read as if copied from a live instance, so they are now obviously synthetic
(`rule-uid-1`, `rule-uid-2`, `cp-uid-1`), with the sample tables re-aligned to the narrower column.
The docker probe *results* recorded above keep their observed values, which are labelled as such.

**Local working notes (review 5, major):** `/.grafanapi-*.md` is now in `.gitignore` (this is a
public repo; a scratch brief sitting untracked at the root is one `git add -A` away from being
published).
