# `grafanapi alerts` — Editor-level writes (alerting as code)

## Overview

**What:** Extend the read-only `alerts` group (shipped on branch `alerts-command`) with the writes an
**Editor** can perform on Grafana Unified Alerting, in grafanapi's dashboards-as-code style:

```
grafanapi alerts pull  [-p ./alerting] [--folder-uid UID]... [-o yaml|json]   # Grafana → disk (rule groups, contact points, policies)
grafanapi alerts push  [-p ./alerting] [--dry-run] [--ui-editable] [--on-error ignore|fail|abort]   # disk → Grafana
grafanapi alerts rules pause  UID...  [--dry-run]
grafanapi alerts rules resume UID...  [--dry-run]
grafanapi alerts rules delete UID...  [--dry-run]
```

**Why:** the read side answers "what is defined / what is firing"; teams also need to back up,
review and promote alerting between environments, and to pause/resume/delete rules from a terminal
or CI. All of it is Editor-level on the provisioning API — the same API the read path already uses.

**Scope:** everything an Editor may do — create/update alert rules (via rule groups), delete rules,
create/update contact points, replace the notification policy tree, pause/resume. **Excluded
(org-admin level or not requested):** Alertmanager config reset (`ResetPolicyTree`),
`…/export?decrypt=true`, editing provisioning-protected objects from the UI side, data sources,
mute timings/templates, deleting contact points, pruning remote-only groups (`--prune` is future work).

**Integration:** new files in `internal/alerts` (domain) + `cmd/grafanapi/alerts` (thin wiring),
generated client `Provisioning.*` write methods (models are lossless here — see Context), the existing
`internal/format` codecs for files, a small shared `--on-error` package extracted from
`cmd/grafanapi/resources`, and the same exit-code contract (dead-session 401 → exit 2). Branch
`alerts-write` is cut from `alerts-command` (merge after it).

## Context (from discovery)

Verified 2026-09-11 against docker `grafana/grafana:12.3` (12.3.2 OSS) with Admin/Editor/Viewer
session cookies, and the generated client `grafana-openapi-client-go@v0.0.0-20251202103709-7ef691d4df1d`.

### Generated client — write surface (all under `Provisioning`)

| Op | Provenance param | Body / payload | Success |
|---|---|---|---|
| `GetAlertRule(UID)` | — | `*models.ProvisionedAlertRule` | 200 |
| `PostAlertRule(params)` / `PutAlertRule(params)` / `DeleteAlertRule(params)` — params-only, no positional form | `XDisableProvenance *string` on all three | `*models.ProvisionedAlertRule` | 201 / 200 / 204 |
| `GetAlertRuleGroupWithParams(NewGetAlertRuleGroupParams().WithFolderUID(f).WithGroup(g))` — **the positional `GetAlertRuleGroup(group, folderUID)` (and `DeleteAlertRuleGroup`) take group FIRST; positional forms are banned for these two** | — | `*models.AlertRuleGroup{FolderUID, Title, Interval int64 (seconds), Rules []*ProvisionedAlertRule}` | 200 |
| `PutAlertRuleGroup(NewPutAlertRuleGroupParams().WithFolderUID(f).WithGroup(g).WithBody(b))` — params-only (there is **no** `…WithParams` alias for any write method) | `XDisableProvenance` | `*models.AlertRuleGroup` | 200 |
| `PostContactpoints` / `PutContactpoint(UID)` | `XDisableProvenance` | `*models.EmbeddedContactPoint{UID, Name, Type *string, Settings JSON, DisableResolveMessage}` | **202** / **202** (`models.Ack`) |
| `DeleteContactpoints(UID)` | **none** — out of scope anyway | — | 202 |
| `GetPolicyTree` | **none** | `*models.Route` | 200 |
| `PutPolicyTree` | `XDisableProvenance` | `*models.Route` | **202** |
| `Folders.GetFolderByUID(uid)` | — | `*models.Folder` | 200 / 404 |
| `GetAlertRulesExport` etc. | never set `Format` (JSON consumer; `format=yaml` breaks decoding) | `*models.AlertingFileExport` | 200 |

- 400 → a **per-operation** typed struct (`*PutAlertRuleGroupBadRequest`, `*PostContactpointsBadRequest`,
  …), each implementing `GetPayload() *models.ValidationError`; there is no shared type, so the one
  matcher that works for all six ops is
  `var ve interface{ GetPayload() *models.ValidationError }; errors.As(err, &ve)`. Every other
  non-2xx (incl. **403** and 401) → default `*runtime.APIError{Code}`. 401 stays exit 2 through
  `fail.convertSessionErrors`; **403 currently renders as "Unexpected error … (status 403): {}"** —
  a pre-existing converter gap, fixed in Task 6. `runtime.APIError.Response` marshals to `{}`, so the
  Grafana "Permissions needed: …" text is **not** recoverable through the generated client.
- `ProvisionedAlertRule` required pointers: `Condition, Data, ExecErrState, FolderUID, For, NoDataState,
  OrgID, RuleGroup, Title` — enforced by the generated `Validate(strfmt.Default)`; `Updated`/`Provenance`
  are read-only. `OrgID *int64` has `json:"orgID"` **without** `omitempty` (nil → `orgID: null`).
  `Updated` is `strfmt.DateTime` (a struct): **`omitempty` has no effect in `encoding/json`**, so a
  zeroed value serialises as `"updated":"0001-01-01T00:00:00.000Z"` in JSON while goccy YAML omits
  it — hence the generic-map encoding in decision 1. **Key-name gotcha:** `AlertRuleGroup.FolderUID`
  is `folderUid` but `ProvisionedAlertRule.FolderUID` is `folderUID` — both spellings appear in one
  group file. nil slices serialise as `null` in JSON and `[]` in YAML through the codecs; through the
  generic map both become `null`.
- `models.Provenance` is a bare `string` with **no enum**; values seen in the wild: `""`, `"api"`,
  `"file"`, `"converted_prometheus"`, `"disabled"`.
- The `GetAlertRules` / `GetAlertRuleGroup` payloads feed straight back into the write bodies —
  **lossless round-trip**, so no local wire structs are needed for writes. `internal/format`'s YAML
  codec honours the models' `json` tags (verified: `folderUid`, `execErrState`, `isPaused`, `orgID`).

### Live semantics (12.3.2)

- **Permissions:** Editor gets 201/200/202/204 on every write above; Viewer gets **403** with body
  `{"accessErrorId":…,"message":"You'll need additional permissions … Permissions needed: any of
  alert.provisioning:write, …","title":"Access denied"}`. Reads and `…/export` are 200 for both.
- **Provenance (rules, contact points, policies behave the same):** POST without header →
  `provenance: "api"`; POST with `X-Disable-Provenance: true` → `""`. PUT without header on `""` →
  *upgrades* to `"api"`. PUT **with** header on `"api"` → **500** (rules; reason only in server log) /
  **403** `alerting.notifications.invalidProvenance` (policies). DELETE without header on `"api"` →
  204; DELETE **with** header → **409** `alerting.provenanceMismatch`. A group PUT without header
  re-stamps every rule as `"api"`. Resources with any **other** provenance (`file`, …) are owned by
  another provisioner and were not exercised — they must be refused, not guessed at.
- **Rule-group PUT:** `rules: []`/omitted → interval-only update, rules preserved; non-empty `rules` →
  **full replace** (rules missing from the array are deleted). Re-PUTting a fetched group is a no-op.
- **Pause/resume:** no dedicated endpoint. A single-rule `PutAlertRule` whose body was the exact
  `GetAlertRule` payload with only `isPaused` flipped (all required pointers present, header matching
  the stored `api` provenance) returned 200 but the change was **not persisted for an Editor**
  (5/5, both directions; Admin persisted it; Editor persisted it when another field changed too).
  The **group PUT** with `isPaused` flipped persisted for Editor 5/5 in both directions ⇒ pause/resume
  goes through the group. (Recorded so a future Grafana fix can collapse Task 5 to a single PUT.)
- **Concurrency:** none — no ETag/version, `If-Match` ignored, last writer wins.
- **Validation:** missing folder → 400 `invalid alert rule: folder does not exist` (group PUT into a
  missing folder → 403 for Editor, permission check runs first); folders are **never auto-created**.
  `DELETE /alert-rules/{unknown}` → **500** (not 404).
- **Export ≠ import:** `…/export` returns the *file-provisioning* format (`groups[].name`, folder
  **title**, `interval: "3m"`, no `folderUID/orgID/ruleGroup`); the write endpoints reject it
  (400). No import endpoint exists ⇒ our on-disk format mirrors the write models instead.
- **Secrets:** contact-point settings come back `"[REDACTED]"` everywhere; **PUT with the literal
  `"[REDACTED]"` keeps the stored secret**, a literal new value replaces it ⇒ pull→edit→push is safe;
  never print/decrypt.
- **No server-side dry-run** on the provisioning API ⇒ `--dry-run` is client-side (fetch, diff, print).

### Repo conventions (verified, with the corrections from review)

- Options pattern (`opts.setup/BindFlags`, `Validate()` first in `RunE`). Precedent from `resources`:
  `-p/--path` (**repeatable** `StringSliceVarP` on `resources push`; `alerts push` takes a single path
  because a bundle is one directory tree — deliberate divergence), `--dry-run`, `--max-concurrent`,
  `--on-error ignore|fail|abort`. **`resources delete` requires `--force` only for un-named selectors**
  (`cmd/grafanapi/resources/delete.go:93`); fully named deletes need no `--force` ⇒ `alerts rules delete
  UID...` takes **no** `--force`, only `--dry-run`. **`resources push` has no `-o`**; it prints a
  summary via `cmdio.Success/Warning/Error` ⇒ `alerts push` does the same (plan table on `--dry-run`,
  summary on apply), and `-o` exists only on `alerts pull` (file format, as on `resources pull`).
- `OnErrorMode`, `bindOnErrorFlag`, `StopOnError`, `FailOnErrors` live **unexported** in
  `cmd/grafanapi/resources/onerror.go` (+ `onerror_test.go`), used by `pull.go`, `validate.go`,
  `delete.go`, `get.go`, `push.go` ⇒ Task 0 moves them to `cmd/grafanapi/onerror` (exported) and
  deletes the originals; `resources` and `alerts` import it. **`internal/alerts` never imports a
  `cmd/` package**: like `remote.PushRequest{StopOnError}` it takes `StopOnError bool`; the cmd layer
  owns `Mode` and applies `FailOnErrors()` to the result.
- `alerts rules list --folder` accepts UID, full path or last path segment (resolved through the state
  view); `alerts pull` has no state view ⇒ its flag is `--folder-uid` (UID only). `GetAlertRulesParams`
  has no folder filter (filtering is client-side), so an empty folder and a missing one are
  indistinguishable from the list ⇒ each `--folder-uid` is validated with `Folders.GetFolderByUID`
  (404 → error), the same call the push pre-check uses.
- `resources push` prints **one** summary line (`%d resources pushed, %d errors`); `alerts push`
  prints one `cmdio` line per change plus a summary — a deliberate divergence (few objects, large
  blast radius per object), stated in the guide.
- `internal/alerts` uses a concrete `*goapi.GrafanaHTTPAPI` and `httptest` servers (no interface seam);
  keep that. Errors `fmt.Errorf("alerts: <op>: %w", err)`; `logging.FromContext(ctx)`; AGENTS.md symbol
  ordering; `CLAUDE.md` is a symlink to `AGENTS.md` (edit AGENTS.md only; no "Known Issues" section —
  append limits to the existing *Technical Debt Inventory → Low Priority*).
- Stale "read-only / inspect" wording lives in **many** places: `cmd/grafanapi/alerts/command.go`
  (package doc, `Short: "Inspect …"`, `Long`), `cmd/grafanapi/alerts/rules.go` (`Short/Long: "Inspect
  alert rules"`), `internal/alerts/rules.go` package doc, `docs/guides/alerting.md`, `README.md`,
  `AGENTS.md` (two spots), `skill/grafanapi/SKILL.md` ("Nothing in this group can create, update or
  delete…"). Task 7c rewrites the Go ones; Task 8 runs a mechanical grep gate over the rest.
- `cmd/grafanapi/fail/convert.go`: `convertAPIErrors` handles only `*k8sapi.StatusError`;
  `convertSessionErrors` is where `*runtime.APIError` is type-asserted; `convertNetworkErrors` runs
  first and swallows anything wrapped in `*url.Error` — applies to 403 exactly as to 401.
  `convert_test.go` exists (line ~76 asserts the k8s Forbidden rendering — **not** changed by this plan).
  The converter-list comment in `ErrorToDetailedError` (~line 53) and the doc comment above
  `convertSessionErrors` (~180-194) describe only the 401 cases and must be updated with the rename.
- `internal/format`: YAML decoder is `yaml.Strict()` (unknown key → error) but `JSONCodec.Decode` is a
  plain `json.NewDecoder` **without** `DisallowUnknownFields` ⇒ `ReadBundle` decodes JSON with its own
  strict decoder so a typo'd key fails the same way in both formats.
- `login --cookie-stdin` requires `--server`; `login` writes a real Keychain item per context
  (`grafanapi:<context>`), which integration runs must delete afterwards.
- **Toolchain:** `devbox` absent → `go build ./...`, `go test -race $(go list ./... | grep -v internal/keychain)`,
  `golangci-lint run -c .golangci.yaml` (baseline 14 findings in untouched files: gosec 5/govet 3/
  nolintlint 1/staticcheck 5; gate = zero findings in touched files, counts unchanged), CLI reference
  `rm -rf ./docs/reference/cli && go run ./scripts/cmd-reference` + `git diff --exit-code docs/reference`,
  `<scratch>/mkdocs-venv/bin/mkdocs build --strict -d <scratch>/site-check`, `make test-env-up/-clean`.
- **Confidentiality gate (public repo)** — run before every commit over `git diff --name-only
  --diff-filter=AM alerts-command...HEAD` files and the plan; must print nothing. The term list
  (deployment/employer names, environment labels, ticket prefixes, wiki tools, the maintainer's
  personal name) is kept **outside the repository** (session scratchpad `confidential-terms.txt`) so
  the gate itself never spells the terms into a tracked file:
  `git diff --name-only --diff-filter=AM alerts-command...HEAD | tr '\n' '\0' | xargs -0 grep -n -i -E -f <terms-file> | grep -v -i -E 'evaluat|templat|situat'`
  (zsh does not word-split an unquoted variable — always pipe through `xargs -0`).
  Example UIDs in docs/tests are synthetic (`rule-uid-1`, `cp-uid-1`, `folder-uid-1`).

## Development Approach

- **testing approach**: Regular (code first, then tests)
- one task at a time; every task ships its tests; all gates green before the next task
- **CRITICAL: update this plan when scope changes** (➕ / ⚠️ notes)
- dead-session 401 stays an unwrapped `*runtime.APIError`; never log or print secrets/cookies
- writes are **Editor-level only**; nothing here calls `ResetPolicyTree`, `decrypt=true`, or any
  admin endpoint; no contact-point deletes; no pruning

## Testing Strategy

- **unit**: `internal/alerts/*_test.go` with `httptest` scripting the provisioning endpoints
  (provenance header presence/absence per stored value, refusal of foreign provenance, 403 bodies,
  500 on unknown rule delete, group replace semantics, filename collisions); `cmd/grafanapi/alerts/*_test.go`
  with `testutils.CommandTestCase` (temp config + fake keychain) for flags, dry-run output, exit codes
  (`fail.ErrorToDetailedError`).
- **integration** (Task 9): docker 12.3 with Editor and Viewer users — pull → edit → push round-trip,
  pause/resume/delete, Viewer 403 rendering, secrets preserved on push.

## Progress Tracking

- `[x]` when done; ➕ for discovered work; ⚠️ for blockers; keep in sync

## Solution Overview

```
cmd/grafanapi/onerror/          Mode (ignore|fail|abort), BindFlag, StopOnError/FailOnErrors — extracted from resources
internal/alerts/
  files.go        on-disk layout + read/write via internal/format (yaml default, json accepted)
  provenance.go   three-way classifier for X-Disable-Provenance
  pull.go         Pull: fetch selected groups / contact points / policy tree → Bundle
  diff.go         Plan: local vs remote → ordered change set (create/update/unchanged)
  push.go         Push: apply a Plan (contact points → policies → rule groups)
  ruleops.go      SetRulesPaused (group PUT) / DeleteRules (GET-then-DELETE)
cmd/grafanapi/alerts/
  pull.go push.go ruleops.go plan_table.go   thin wiring ; command.go + rules.go docs rewritten
cmd/grafanapi/fail/convert.go    *runtime.APIError{403} → "permission denied" rendering (exit 1)
```

Key decisions:

1. **On-disk format mirrors the write models** (Grafana's export format isn't importable):
   ```
   <path>/
     rules/<folderUID>/<sanitised group title>.<yaml|json>   # models.AlertRuleGroup
     contact-points.<ext>                                     # []models.EmbeddedContactPoint
     policies.<ext>                                           # models.Route
   ```
   `folderUid`/`title` **inside** the file are authoritative. `sanitizeFileName` maps `/`, `\`, control
   characters and a leading `.` to `_`; because that is many-to-one, `WriteBundle` **fails** (naming both
   groups) when two groups in one folder sanitise to the same file — no silent overwrite.
   **Encoding goes through a generic map:** each model is `json.Marshal`ed, decoded into
   `map[string]any`, the read-only keys `id`, `updated`, `provenance` are deleted (recursively for
   `routes[]` in the policy tree and for each `rules[]` entry), and the map is encoded with the chosen
   `internal/format` codec — so the on-disk key set is identical in YAML and JSON and `updated` never
   leaks (it would in JSON otherwise). `orgID` is **kept** as fetched (nil would serialise as
   `orgID: null`); on push it is overwritten from the remote group when it exists, else from the
   context's `org-id` **if non-zero** (Cloud contexts use `stack-id` and have `org-id` 0), else left as
   in the file. The guide tells users not to hand-edit `orgID`. **Stale files:** `pull` removes files
   under `rules/` (only within the layout, and only within the selected folders when `--folder-uid` is
   given) that don't correspond to a fetched group, so a group deleted in Grafana is not resurrected
   by the next push; `contact-points`/`policies` files are always rewritten.
2. **Rule groups are the unit of rule writes.** Push PUTs each local group whole (Grafana's replace
   semantics) — a rule missing from the file *is deleted from that group*; `--dry-run` lists those
   removals explicitly. Groups present remotely but absent locally are **left alone** (no prune).
   Folders must already exist — push pre-checks each `folderUid` via `Folders.GetFolderByUID` and fails
   that group with "folder <uid> does not exist; push folders first (`grafanapi resources push`)".
3. **Provenance-aware writes, three-way:** `provenance.go` classifies the stored value —
   `""` → send `X-Disable-Provenance: true`; `"api"` → omit the header; **anything else**
   (`file`, `converted_prometheus`, `disabled`, unknown) → refuse the write with
   `alerts: <op> <kind> <id>: provenance "<v>" is managed outside the API` (a per-item failure that
   honours `--on-error`). For a **new** resource `--ui-editable` picks the mode (default off →
   `provenance: api`, matching the read side) — it affects **creates only**; updates always mirror the
   stored provenance (flag help says so). A rule group whose rules carry **mixed** provenance is
   refused the same way. `DeleteAlertRule` follows the rule (header iff stored `""`); contact-point
   deletes are out of scope (the client has no provenance param there anyway).
4. **Ordering:** contact points → policy tree → rule groups; within the rule-group phase, PUTs run
   in an errgroup bounded by `--max-concurrent` (which also bounds the pre-fetch). Contact points match
   by `uid`, else by `name`; a local entry without `uid` whose name matches several remote points is
   a `Plan` error. Policy tree is a single PUT. `Change.ID` is `<folderUID>/<title>` for groups,
   `uid` (or `name` when uid-less) for contact points, `policies` for the tree.
5. **Client-side dry-run and diff.** `Plan(local, remote Bundle) ([]Change, error)` is pure and
   unit-tested; actions are `Create|Update|Unchanged` only (no `Delete` until `--prune` exists).
   `push --dry-run` prints the plan table and exits 0 without writing. Documented limit: **no
   optimistic concurrency exists server-side** — last writer wins.
   ➕ (Task 12) a dry run also runs the **read-only** checks that decide whether a write is allowed
   (folder pre-check, provenance, redacted-secret) and prints the entries an apply would refuse as
   `✘ would fail: …` below the table; it still exits 0 and ignores `--on-error`.
6. **Push fetches only what it needs:** `Pull` accepts a group selection, so `Push` fetches the local
   groups' remote counterparts (one `GetAlertRuleGroup` each), one `GetContactpoints`, one
   `GetPolicyTree` — not the whole instance.
7. **Pause/resume via group PUT** (Editor-safe; see Context): fetch each rule → group by
   `(folderUID, ruleGroup)` → fetch group → flip `isPaused` → `PutAlertRuleGroup(params)` with the
   group's (uniform) provenance header. No post-write read-back in code (the group PUT persisted 5/5 live);
   Task 9 verifies end-to-end. **Delete**: `GetAlertRule` first so an unknown UID is a clean
   "not found" (the raw DELETE 500s); `--dry-run` supported; no `--force` (named deletes, per precedent).
8. **403 rendering** in `cmd/grafanapi/fail/convert.go`: inside the converter that already asserts
   `*runtime.APIError` (`convertSessionErrors`, renamed `convertAuthErrors`), `Code == 403` →
   `permissionDeniedError`: summary "Grafana denied the request: your user lacks the required
   permission", `Parent` kept (the `{}` detail from the opaque response is accepted), exit 1.
   ➕ (Task 12) the suggestion is **operation-neutral** — this converter matches every openapi 403,
   so naming alerting's Editor requirement there was wrong advice for `explore`, `datasources`,
   `config check` and `login`. The role requirement lives in the alerting help and guide instead. The k8s `IsForbidden` branch and its existing test are
   **unchanged**; the `*url.Error` ordering caveat applies as for 401.
9. **Secrets:** pulled contact points carry `"[REDACTED]"` for secure fields; pushing them back keeps
   the stored secret (verified); `Plan` walks `settings` **recursively** (it is `interface{}`, secure
   values can nest) and treats any `"[REDACTED]"` leaf as unchanged.
   ➕ (Task 12) that is a **PUT** property. A POST has no stored secret to keep, so a *create*
   carrying the marker is refused per item rather than sent — the pull→push-into-another-instance
   case. Redaction is also only over the type's **schema-declared** secure fields: a token in a
   webhook URL or a custom header reaches `contact-points.<ext>` in plain text, so the bundle is a
   sensitive artifact (0600) and the docs say so. Nothing decrypts or prints a
   secret. Policy-tree comparison and provenance zeroing are likewise **recursive over `routes[]`**.

## Technical Details

### Flags

- `alerts pull`: `-p/--path` (default `./alerting`), `--folder-uid UID` (repeatable; unknown → error),
  `-o/--output yaml|json` (file format, default yaml, via `cmdio.Options` with the table codec not
  registered), `--max-concurrent` (default 10).
- `alerts push`: `-p/--path` (single path — a bundle is one tree, unlike `resources push`'s repeatable
  `-p`), `--dry-run`, `--ui-editable` (creates only), `--on-error ignore|fail|abort` (default `fail`,
  `onerror.BindFlag`), `--max-concurrent`. Output: dry-run → plan table; apply → one
  `cmdio.Success/Error` line per change + summary (deliberate divergence from `resources push`'s single
  line).
- `alerts rules pause|resume|delete UID...`: `Args: cobra.MinimumNArgs(1)`, `--dry-run`.

### Types

```go
// diff.go
type Action string // Create | Update | Unchanged
type Kind   string // RuleGroup | ContactPoint | PolicyTree
type Change struct{ Action Action; Kind Kind; ID string; Detail []string } // Detail: "+ title (uid)", "- title (uid)", "~ interval 60s→120s"
func Plan(local, remote Bundle) ([]Change, error)

// provenance.go
type provenanceMode int // modeDisableHeader | modeAPI | modeForeign
func classifyProvenance(stored string) provenanceMode
func disableProvenanceHeader(mode provenanceMode) *string // ptr("true") for modeDisableHeader, nil otherwise
func createHeader(uiEditable bool) *string

// pull.go
type GroupRef struct{ FolderUID, Title string }
type PullOptions struct{ FolderUIDs []string; Groups []GroupRef; MaxConcurrent int } // Groups non-empty → fetch only those

// push.go  (no cmd/ imports — StopOnError bool, as remote.PushRequest does)
type PushOptions struct{ DryRun, UIEditable, StopOnError bool; MaxConcurrent int; OrgID int64 }
type FailedChange struct{ Change Change; Err error }
type Result struct{ Applied []Change; Failed []FailedChange }   // cmd layer: onerror.Mode.FailOnErrors(len(Failed))

// ruleops.go
type RuleOpOptions struct{ Paused, DryRun bool }
type RuleOpResult struct{ UID, Title, Group string; Changed bool; Err error }
func SetRulesPaused(ctx, gClient, uids []string, opts RuleOpOptions) ([]RuleOpResult, error)
func DeleteRules(ctx, gClient, uids []string, dryRun bool) ([]RuleOpResult, error)

// files.go
func WriteBundle(dir string, f format.Format, b Bundle, keepFolders []string) error // keepFolders: nil = prune all stale rules/ files
func ReadBundle(dir string) (Bundle, error) // strict JSON (DisallowUnknownFields) + strict YAML
```

### Error mapping

- 401 → unwrapped `*runtime.APIError` (exit 2) — never wrapped in `*url.Error`.
- 403 → `permissionDeniedError` (exit 1) — Task 6.
- 400 (any `…BadRequest` with `GetPayload() *models.ValidationError`) → `alerts: push <kind> <id>: <Message>`
  via one shared `validationMessage(err) (string, bool)` helper.
- foreign provenance → per-item error (above); unknown rule on delete → `alerts: rule <uid> not found`.
  ➕ (Task 12) that message wraps the exported `alerts.ErrRuleNotFound` sentinel, matched by a
  `convertAlertsErrors` arm → "Alert rule not found", exit 1 (never a `*fail.DetailedError` returned
  from cmd, which would short-circuit the 401/403 renderings).

## Implementation Steps

### Task 0: Extract `--on-error` into `cmd/grafanapi/onerror`

**Files:**
- Create: `cmd/grafanapi/onerror/onerror.go`; Move: `cmd/grafanapi/resources/onerror_test.go` →
  `cmd/grafanapi/onerror/onerror_test.go`
- Delete: `cmd/grafanapi/resources/onerror.go`
- Modify: `cmd/grafanapi/resources/{pull,validate,delete,get,push}.go` (confirm the list with
  `rg -n 'OnErrorMode|bindOnErrorFlag|StopOnError|FailOnErrors' cmd/`)

- [x] move the type (`Mode`), constants, `Validate`, `BindFlag(flags, *Mode)`, `StopOnError`,
      `FailOnErrors` and the exact flag help text into the new package; keep behaviour byte-identical;
      delete the original (no alias)
      ➕ constants named `onerror.Ignore` / `Fail` / `Abort` (package name carries the `OnError`
      prefix); doc comments generalised from "resources" to "items", but the **flag help text is
      byte-identical** (still says "resources"), so the CLI reference does not drift
- [x] update all five `resources` call sites; moved test adapted to the new names; all `resources`
      tests pass
      ➕ `cmd/grafanapi/resources` now reports "[no test files]" — `onerror_test.go` was its only
      test file; the coverage moved wholesale to `cmd/grafanapi/onerror`
- [x] tests for the new package: parse/validate table (valid three + invalid), flag default `fail`
      ➕ also covers `BindFlag` parsing a provided value and keeping an invalid value for `Validate`
- [x] gates (`go build ./...`, race tests, lint zero-in-touched, CLI reference no drift — help text
      must not change) — must pass before Task 1

### Task 1: On-disk bundle format — `internal/alerts/files.go`

**Files:**
- Create: `internal/alerts/files.go`, `internal/alerts/files_test.go`

- [x] `Bundle{Groups []*models.AlertRuleGroup; ContactPoints []*models.EmbeddedContactPoint; Policies *models.Route}`,
      layout constants, `sanitizeFileName`
- [x] `WriteBundle(dir, format, bundle, keepFolders)`: model → `json.Marshal` → `map[string]any` →
      delete `id`/`updated`/`provenance` (recursively for `rules[]` and `routes[]`) → encode with the
      `internal/format` codec; keeps `orgID`; dirs `0o750`, files `0o600`; **fails on a sanitised-name
      collision** within a folder, naming both groups; **prunes stale `rules/**` files** not produced
      by this write (restricted to `keepFolders` when non-nil); rewrites `contact-points`/`policies`
      ➕ stripping is **targeted, not blanket-recursive**: the group map strips `id`/`updated`/
      `provenance` from each `rules[]` entry only, the contact-point map from each element, the route
      map from every node down `routes[]`. A blanket recursion would also delete a legitimate `id`
      inside a query's `data[].model`, which is free-form JSON.
      ➕ **number normalisation was required and is not in the original design**: decoding into `any`
      the ordinary way makes every number a `float64`, which the YAML encoder writes as
      `interval: 600.0` / `maxDataPoints: 43200.0`; `json.Number` is worse (goccy quotes it, so
      `interval: "600"`). `normalizeNumbers` decodes with `UseNumber()` and re-types each literal as
      `int64` when integral, `float64` otherwise. Pinned by `TestWriteBundle_KeepsIntegersIntegral`.
      ➕ a `nil` ContactPoints/Policies writes **no** file (a `null` file would read back as an empty
      object and push as a wipe), and the top-level files are removed in *every* recognised extension
      before the current one is written, so `pull -o json` after `pull -o yaml` cannot leave two files
      disagreeing about the same object.
- [x] `ReadBundle(dir) (Bundle, error)`: walks the layout, detects yaml/json per file, decodes JSON
      with `DisallowUnknownFields` and YAML with the strict codec, calls each model's generated
      `Validate(strfmt.Default)` and additionally requires `interval > 0` and non-empty
      `folderUid`/`title`; errors name the file; missing sub-parts → zero-value, not error
      ➕ recognised extensions are `.yaml`, `.yml`, `.json`; anything else under `rules/` (a README,
      an editor backup) is ignored by both the reader and the pruner. Two files for the same
      top-level object (`policies.yaml` + `policies.json`) are an error naming both.
      ➕ groups are returned sorted by `folderUid` then `title`, so a bundle reads identically twice.
- [x] tests: write→read round-trip (yaml + json) preserves every write-relevant field; **identical
      key set in both formats**, asserted exactly for a group file (incl. `folderUid` on the group vs
      `folderUID` on rules, `orgID` present, no `id`/`updated`/`provenance` at any depth incl. nested
      routes); `sanitizeFileName` table (`/`, `\`, control chars, leading `.`, `..`); collision →
      error names both; stale group file removed on re-write, kept when its folder is outside
      `keepFolders`; unknown key → error in **both** formats naming the file; malformed file → error;
      missing policies file → nil Policies
      ➕ `testpackage` forces `package alerts_test`, so `sanitizeFileName` is covered **through**
      `WriteBundle` (the produced file name is asserted per input) rather than by a direct table.
      ⚠️ observed on-disk key set for a group file, **identical in YAML and JSON**:
      `folderUid`, `interval`, `title`, `rules[]` → `condition`, `data[]` (`refId`, `datasourceUid`,
      `model`), `execErrState`, `folderUID`, `for`, `isPaused`, `noDataState`, `orgID`, `ruleGroup`,
      `title`, `uid`. Contact points: `uid`, `name`, `type`, `settings`. Policies: `receiver`,
      `group_by`, `active_time_intervals`, `mute_time_intervals`, `routes[]` (recursively).
- [x] gates — must pass before Task 2

### Task 2: Provenance classifier + Pull — `internal/alerts/provenance.go`, `pull.go`

**Files:**
- Create: `internal/alerts/provenance.go`, `provenance_test.go`, `pull.go`, `pull_test.go`

- [x] `provenance.go` per Technical Details; `groupProvenance(rules) (provenanceMode, error)` → error
      on mixed values
      ➕ **the public surface is three exported wrappers, not the plan's unexported four.**
      `internal/alerts` tests are `package alerts_test` (testpackage) and `unused` is on, so a
      purely-unexported `classifyProvenance`/`disableProvenanceHeader`/`createHeader` would be both
      untestable and flagged until Task 4 uses them. Shipped instead:
      `ProvenanceHeader(stored string) (*string, error)` (update/delete — mirrors the stored value),
      `CreateProvenanceHeader(uiEditable bool) *string` (creates only, was `createHeader`) and
      `GroupProvenanceHeader(rules) (*string, error)`; `provenanceMode`, `classifyProvenance`,
      `disableProvenanceHeader` and `groupProvenance` stay unexported behind them exactly as designed.
      ➕ the foreign/mixed errors are **unprefixed** (`provenance "file" is managed outside the API`,
      `rules carry mixed provenance ("api" and "")`) so Task 4/5 wrap them as
      `alerts: <op> <kind> <id>: %w` and the plan's message comes out verbatim.
      ➕ an **empty** rule group classifies as unclaimed (header `true`), and a `nil` rule in the
      slice is skipped rather than treated as provenance `""`.
      ➕ `groupProvenance` returns `modeForeign` (not the zero value `modeDisableHeader`) alongside
      its error, so a caller that ignored the error would refuse the write rather than claim rules.
- [x] `Pull(ctx, gClient, opts PullOptions) (Bundle, error)`: if `opts.Groups` set → one
      `GetAlertRuleGroupWithParams` per ref (404 → absent, not error); else validate every
      `FolderUIDs` entry with `Folders.GetFolderByUID` (404 → `alerts: pull: folder <uid> not found`),
      `GetAlertRulesWithParams` → distinct `(folderUID, ruleGroup)` filtered by `FolderUIDs` → group
      fetches (errgroup, bounded); plus `GetContactpoints`, `GetPolicyTree`; sorted, non-nil slices
      ⚠️ **Context table correction:** there is no `Provisioning.GetPolicyTree(params)` — the
      positional `GetPolicyTree(opts ...ClientOption)` takes *no* params, so passing a context
      requires `GetPolicyTreeWithParams(NewGetPolicyTreeParams().WithContext(ctx))`. Same for
      folders: `GetFolderByUID(uid)` is positional, so the ctx-carrying form is
      `GetFolderByUIDWithParams(NewGetFolderByUIDParams().WithContext(ctx).WithFolderUID(uid))`.
      ➕ contact points reuse `ListContactPoints` rather than repeating the drop-nils-and-sort; the
      consequence is that a contact-point failure during a pull is prefixed
      `alerts: list contact points:` rather than `alerts: pull …`.
      ➕ group refs are deduped **and sorted** before fetching (a group of N rules produced N refs),
      so the fetch order — and any error naming the first failing group — is deterministic.
      ➕ `MaxConcurrent <= 0` falls back to `defaultMaxConcurrent = 10`; `errgroup.SetLimit(0)`
      would deadlock.
      ➕ `fetchGroup` returns `(*models.AlertRuleGroup, bool, error)`; a plain `(nil, nil)` for the
      404-absent case is a `nilnil` lint finding.
- [x] 401 → unwrapped; others `alerts: pull <what>: %w`
      ⚠️ **the folders client breaks the "every non-200 is a `*runtime.APIError`" assumption**: its
      generated reader declares typed 401/403/404/500 responses, so a 401 from the folder check is a
      `*folders.GetFolderByUIDUnauthorized` and `errors.As(err, &*runtime.APIError)` in
      `cmd/grafanapi/fail` would **miss it** — a stale session would exit 1 with an opaque message
      instead of exiting 2. `pull.go`'s `asAPIError(operation, err)` converts any typed response
      (they all expose `Code() int`) into `runtime.NewAPIError(op, err, code)`. Task 4's folder
      pre-check must use it too, and Task 6's 403 rendering depends on it for the same reason.
- [x] tests: classifier table (`""`, `api`, `file`, `converted_prometheus`, `disabled`, garbage), mixed
      group → error; pull two folders/three groups; `FolderUIDs` filter (folder GET asserted);
      unknown folder → 404 → error; existing-but-empty folder → empty result, no error;
      `Groups` selection issues exactly N group GETs and no list call; 404 group → absent; 401 shape;
      500 wrapped; concurrency bound honoured
      ➕ the 401 test is a two-case table covering **both** transports (provisioning *and* the typed
      folder check), which is the only thing pinning the `asAPIError` conversion above.
      ➕ `pull_test.go` adds its own request-recording `pullFixture` (path counts + peak in-flight)
      on top of the shared `stubResponse`/`okResponse` helpers; `alertsServer` records nothing, and
      "issues exactly N GETs and no list call" / "concurrency bound honoured" are both assertions
      about requests rather than about the returned bundle.
      ➕ also covered: nil client, duplicate refs in `opts.Groups` fetched once.
- [x] gates — must pass before Task 3
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (concurrency test re-run `-count=8`, no flake); `golangci-lint` back at the 14-finding
      baseline (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the four
      new files; `go run ./scripts/cmd-reference` → no drift.

### Task 3: Plan/diff — `internal/alerts/diff.go`

**Files:**
- Create: `internal/alerts/diff.go`, `internal/alerts/diff_test.go`

- [x] `Plan(local, remote Bundle) ([]Change, error)` — pure, deterministic (sorted by kind, id)
      ➕ `Kind` values sort into the **apply order** as plain strings (`ContactPoint` < `PolicyTree` <
      `RuleGroup`), so one sort gives both a readable plan and the order push walks; `Action`/`Kind`
      constants are the capitalised names from Technical Details (`ActionCreate`, `KindRuleGroup`, …),
      leaving the wording of the table to Task 7b's renderer.
- [x] rule groups: `Create` (absent remotely), `Update` (interval or any rule differs; `Detail` lists
      `+`/`-`/`~ <rule uid/title>` and `~ interval`), `Unchanged`; remote-only groups omitted
      ➕ **detail-empty ⟺ unchanged**: `AlertRuleGroup` carries nothing but its identity, its
      interval and its rules, so the action is derived from the details rather than computed twice.
      ➕ rules are matched by **uid**, so moving a rule within a group file is not reported against
      that rule and a rule with no uid is always a `+`; the detail lines are sorted, which groups
      them `+` / `-` / `~` (marker byte order) after the single interval line. (Revised in Task 13:
      the *order* itself is a change, reported once per group as `~ rule order`.)
      ➕ a uid-less new rule renders as `+ <title>` with **no** empty parentheses.
      ➕ `orgID`/`folderUID`/`ruleGroup` are compared like any other field even though push
      overwrites them — they are part of the write body, and a file disagreeing about them is worth
      reporting (pinned by the "claiming another folder and group" case).
- [x] contact points: match `uid` → `name`; ambiguous name → error naming the entry; `settings`
      compared as a recursive walk where any `"[REDACTED]"` leaf compares as unchanged
      ➕ the redaction walk (`equalRedacted`) is applied to the **whole encoded contact point**, not
      only to `settings`: the rule is "a local `[REDACTED]` leaf equals whatever stands there
      remotely", and settings is the only place the marker can legitimately appear anyway. A marker
      never hides a **sibling** change (pinned).
      ➕ `Change.Detail` is left empty for contact points and the policy tree — neither has inner
      structure a line could describe, and a settings dump would print user data into the plan.
- [x] policy tree: deep-compare `Route` ignoring `Provenance` **recursively over `Routes`**; nil
      local → omitted
- [x] tests: table over Create/Update/Unchanged per kind, rule add/remove/modify details, redaction
      rule at top level and nested, nested-route provenance ignored, ambiguous name error,
      determinism, empty local → empty plan
      ➕ also covered: nil slice elements skipped, a uid match winning over a same-named point, and
      `TestPlan_RuleGroupNormalizesEncodings` — the three ways a file and a fetched model spell the
      same value (nil vs empty rule list, float64 vs int64 in `data[].model`, nil vs empty
      annotations). Each normalisation was mutation-checked: removing `dropEmptyValues` fails the
      policy-tree "empty list and absent one" case.
      ⚠️ `unparam` (enabled, and silent at the Task-1 baseline because the helper had only two call
      sites) starts reporting `files_test.go`'s `testRule` as soon as `diff_test.go` calls it: it
      names **one** constant parameter at a time, so silencing `folderUID` surfaced `group`. Fixed by
      making the "moved rule" case pass a genuinely different `folder-uid-2`/`other-group` rather
      than by touching the shared helper — a new caller of a shared test helper must vary its
      arguments or it re-opens this finding.
- [x] gates — must pass before Task 4
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (`-count=3` on the new tests, no flake); `golangci-lint` back at the 14-finding baseline
      (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the two new files;
      `go run ./scripts/cmd-reference` → no drift.
      ➕ three small changes landed in `files.go` so the diff and the writer share one projection
      instead of two: `encodableValue` now returns an **unprefixed** error (its two write call sites
      wrap it with the file path, `comparableValue` wraps it with `alerts: plan:`), the per-item
      strippers `stripContactPointFields`/`stripRuleFields` were split out of the list/group ones
      (`stripContactPointListFields` is the list form `WriteBundle` passes), and nothing else moved.

### Task 4: Push — `internal/alerts/push.go`

**Files:**
- Create: `internal/alerts/push.go`, `internal/alerts/push_test.go`

- [x] `Push(ctx, gClient, local Bundle, opts PushOptions) (Result, error)`: remote = `Pull` with
      `Groups` from local → `Plan` → apply (skip `Unchanged`)
      ➕ **`PullOptions.Groups` now distinguishes nil from empty** (the same convention
      `WriteBundle`'s `keepFolders` already uses): nil discovers every group, a non-nil empty
      selection fetches none. Without it a bundle holding only `contact-points.yaml` would fall
      through to `GetAlertRules` and pull the whole instance — the opposite of decision 6. One-line
      change in `pullGroupRefs` (`len(opts.Groups) > 0` → `opts.Groups != nil`) plus the doc; the
      existing `PullOptions{}` callers are unaffected.
      ➕ `Result.Applied` carries the `Unchanged` entries too (they needed no request), and both
      halves of the result are sorted with `compareChanges` at the end, so a concurrent push prints
      in plan order rather than in completion order.
- [x] folder pre-check per local group (`Folders.GetFolderByUID`) → per-group failure with the
      documented message
      ➕ `folderExists(ctx, gClient, uid) (bool, error)` was split out of `pull.go`'s
      `validateFolders` and is now shared by both callers (each supplying its own message prefix);
      it keeps the `asAPIError` conversion, which is what makes a 401/403 from the *push* pre-check
      exit 2/1 rather than 1-with-an-opaque-message.
      ➕ the pre-check runs once per distinct folder UID, only for groups that will actually be
      written, and a non-404 failure there is **fatal** (same session, same permissions for every
      write that would follow) while a 404 fails only that folder's groups.
- [x] apply order: contact points (POST with `createHeader` / PUT with the stored-provenance header;
      foreign → per-item failure) → policy tree (PUT; foreign → failure) → rule groups (PUT whole
      group; header from `groupProvenance` of the **remote** group, `createHeader` for a new group;
      set `OrgID` per decision 1; set `FolderUID`/`RuleGroup` on each rule)
      ➕ each phase walks the **local bundle** and pairs it with the plan by ID (`changesByID`),
      rather than walking the plan and looking objects up — so there is no "planned but missing
      locally" branch to invent an error for. `groupID(group)` and `kindLabel(kind)` were added to
      `diff.go` for that (the former replaces the inline `FolderUID + "/" + Title` in
      `planRuleGroups`, so plan and push cannot drift apart on the ID spelling).
      ➕ write bodies are **copies**: `ruleGroupBody`/`contactPointBody`/`routeBody` copy the struct
      (and, for a group, each rule) before pinning `folderUID`/`ruleGroup`/`orgID` and clearing
      `id`/`updated`/`provenance`, so `Push` never mutates the bundle `Plan` was computed from.
      ⚠️ `updated` is a `strfmt.DateTime` struct, so `omitempty` does nothing and every rule in a
      PUT body carries `"updated":"0001-01-01T00:00:00.000Z"`. It is read-only server-side (Grafana
      overwrites it), and a file-read rule has the same zero value, so this is the shape the normal
      path produces either way; Task 9 confirms it live.
      ⚠️ the policy tree is **never a real create**: `GetPolicyTreeOK.readResponse` does
      `o.Payload = new(models.Route)`, so `remote.Policies` is never nil after a successful pull and
      the `CreateProvenanceHeader` branch in `pushPolicyTree` is defensive only (it mirrors the same
      defensive branch in `planPolicyTree`). `--ui-editable` therefore never applies to the tree.
- [x] `StopOnError` (abort) stops at the first failure; otherwise all changes are attempted and
      `Result.Failed` is returned for the cmd layer to turn into `fail`/`ignore` semantics; 400s
      rendered through `validationMessage`; group PUTs bounded by `MaxConcurrent`
      ➕ **abort runs the rule-group PUTs one at a time** (`applyRuleGroupsInOrder`): "stop at the
      first failure" is only meaningful when the writes are ordered, and a bounded-but-concurrent
      phase would have several in flight past the point the user asked to stop.
      ➕ the concurrent path uses a **plain `errgroup.Group`, not `errgroup.WithContext`**:
      cancelling the siblings of a failed write would report them as failures too, when what
      actually happened is that they never ran. Every write finishes; only a 401 is propagated as
      the error that stops the push (nothing left could succeed once the session is gone), which is
      also how a 401 escapes `StopOnError=false` as the exit-code contract requires.
      ➕ `validationMessage` drops the `%w` chain for a 400 on purpose — the typed `…BadRequest` has
      nothing else a caller needs, and the message is the whole point ("invalid alert rule: folder
      does not exist" rather than the generated client's rendering of the response).
      ➕ the six phases live on an unexported `pushRun` struct (client, both bundles, options, the
      accumulating `Result`) so each one takes a change or a target instead of the same six
      parameters; `record(change, err)` is the single place a change becomes Applied or Failed and
      is also what returns the stop signal.
- [x] `OrgID` per decision 1 (remote → context non-zero → file)
- [x] dry-run: returns the plan, zero non-GET requests
- [x] tests (`httptest` asserting method/path/body and exact `X-Disable-Provenance` presence): create +
      update for all three kinds; header per stored provenance (`""`, `api`); `file` provenance →
      failure recorded, others proceed when `StopOnError=false`, stop when true; 400 → message from
      payload; `OrgID` fallback table (remote / context / file; Cloud `org-id` 0 skipped); group
      replace body
      contains every local rule; missing folder → per-group failure; 403 → error wraps
      `*runtime.APIError{403}`; 401 unwrapped; `"[REDACTED]"` verbatim in PUT body; dry-run zero writes;
      push fetches only local groups (assert no list call)
      ➕ `push_test.go` has its own `pushFixture` keyed by **method + path** (a push GETs and writes
      the same addresses, so `pull_test.go`'s path-only table cannot say which response belongs to
      which) and records method/path/`X-Disable-Provenance`/body per request; `pushRoutes` merges a
      test's own routes onto the reads every push performs. The header is asserted as a `*string`,
      since "absent" and `"true"` are different instructions.
      ➕ also covered: `--ui-editable` applies to the create and **not** to the update in the same
      push; 403 twice (per-object on a write, fatal on the folder pre-check); 401 three times (the
      group pre-fetch, the typed folder check, a write); a bundle with **no** rule groups issues no
      rule requests at all.
      ⚠️ two lint traps in the new test file: `modernize` rejects a `func ptr(string) *string`
      helper (`new("true")` is the Go 1.26 spelling), and `testifylint`'s require-error rule fires
      on an `assert.NoError` that is **not** the last statement of its block — the recording helper
      therefore stores the request first and asserts the body read afterwards, since it runs on the
      server goroutine and must not call `require`.
- [x] gates — must pass before Task 5
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (`internal/alerts` re-run `-count=3`, no flake); `golangci-lint` back at the 14-finding
      baseline (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the four
      touched files; `go run ./scripts/cmd-reference` → no drift; confidentiality gate empty.

### Task 5: Rule operations — `internal/alerts/ruleops.go`

**Files:**
- Create: `internal/alerts/ruleops.go`, `internal/alerts/ruleops_test.go`

- [x] `SetRulesPaused(ctx, gClient, uids, RuleOpOptions)`: `GetAlertRule` per uid (unknown → not-found,
      no writes); group by `(folderUID, ruleGroup)`; `GetAlertRuleGroupWithParams`; rules already in the
      requested state → `Unchanged`; else flip and `PutAlertRuleGroup(params)` with the group's
      provenance header (mixed/foreign → failure for that group)
      ➕ both entry points run on an unexported `ruleOpRun` (client + the deduped uids in the order
      the user typed them + the outcome recorded per uid), mirroring `push.go`'s `pushRun`. Its
      `record(result) error` is the single place a uid gets an outcome and is also the stop signal,
      exactly as `pushRun.record` is; the only thing that stops a run is a 401, since these commands
      have no `--on-error`.
      ➕ outcomes are kept **by uid and emitted in input order** rather than appended: the work is
      done group by group, so an appended report would reorder the command line. A uid the run never
      reached (a 401 ended it) has no line rather than an invented one.
      ➕ uids are **deduped**, first occurrence winning: two mentions of one rule would otherwise be
      two full replacements of its group racing each other.
      ➕ the PUT body is `ruleGroupBody(group, nil)` — push's copy helper, reused — and the flip is
      applied to the **copies**, so the fetched group stays as Grafana reported it and is still the
      only source `GroupProvenanceHeader` reads. Passing `nil` for orgID is what keeps each rule's
      own org (a pause must not re-home a rule).
      ➕ a named uid the fetched group no longer holds is a per-uid failure
      (`rule group <id> no longer holds it`), not a silent drop.
      ⚠️ a group-fetch failure during a pause keeps `fetchGroup`'s own `alerts: pull rule group
      <id>:` prefix rather than being re-wrapped as `alerts: pause …` — re-wrapping would double the
      prefix. Same precedent as Task 2's contact-point reuse (`alerts: list contact points:` inside a
      pull).
- [x] `DeleteRules(ctx, gClient, uids, dryRun)`: `GetAlertRule` first; `DeleteAlertRule` with the
      stored-provenance header; foreign → refuse
      ➕ `fetchRule` returns the same `(model, bool, error)` triple as `fetchGroup` (a `(nil, nil)`
      absent case is a `nilnil` finding), and reads both a 404 **and** a 200 carrying no payload as
      absence — the alternative to the latter is a nil dereference two calls later.
      ➕ an empty uid is absence too, without a request: `GET /alert-rules/` is the *list* endpoint,
      so a stray empty argument would otherwise decode a rule array into one rule.
- [x] tests: two uids one group → exactly one PUT with both flipped; resume; already-paused → no PUT;
      unknown uid → not-found before any write; mixed provenance → failure, no PUT; delete header iff
      `""`; foreign provenance delete refused; dry-run zero writes; 401/403 shapes
      ➕ `ruleops_test.go` reuses `push_test.go`'s `pushFixture` verbatim (method+path routing, header
      recorded as a `*string`) and adds only `rulePath`/`opRule`/`opGroup`; no new fixture type.
      ➕ also covered: a uid named twice is one rule and one PUT; two groups are two PUTs; results come
      back in the order the user named the uids; a resume PUT carries the rules **nobody named** with
      their state untouched (the group-replace half of decision 7); `Title`/`Group` are empty for a uid
      that could not be looked up; nil client for both entry points.
      ⚠️ the `unparam` trap from Task 3 again: every shared test helper here varies each parameter
      across call sites on purpose (`opRule`'s group is `probe-group` **and** `other-group`, its
      provenance `api`/`""`/`file`, its paused flag both ways).
- [x] gates — must pass before Task 6
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (`internal/alerts` re-run `-count=3`, no flake); `golangci-lint` back at the 14-finding baseline
      (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the three touched
      files; `go run ./scripts/cmd-reference` → no drift; confidentiality gate empty.
      ➕ two one-line reuse edits outside `ruleops.go`, so neither message nor ID has two spellings:
      `push.go`'s `changeError` now delegates to a new `operationError(operation, target, err)` (the
      same 400-`validationMessage` substitution the rule ops need, byte-identical push output), and
      `diff.go`'s `groupID` delegates to `groupRefID(GroupRef)` — the form the rule ops have, since
      they address a group by ref before they have fetched it.

### Task 6: 403 rendering — `cmd/grafanapi/fail/convert.go`

**Files:**
- Modify: `cmd/grafanapi/fail/convert.go`, `cmd/grafanapi/fail/convert_test.go`

- [x] rename `convertSessionErrors` → `convertAuthErrors`; add `apiErr.Code == http.StatusForbidden` →
      `permissionDeniedError(err)` (summary/suggestion per decision 8, `ExitCode` nil, `Parent` kept)
      ➕ the single `Code == 401` `if` became a `switch apiErr.Code` with the 401 and 403 cases, so the
      `errors.As` assertion is performed once rather than twice; the 401 arm still returns the very same
      `staleSessionError(err)`.
      ➕ new `permissionDeniedSuggestion` const (next to the two existing suggestion consts) naming the
      Editor role *and* pointing at `grafanapi config current-context`, since a 403 is just as often the
      right role on the wrong instance.
- [x] keep 401 branches and ordering unchanged; leave the k8s `IsForbidden` path and its existing
      test untouched; update the converter-list comment in `ErrorToDetailedError` and the doc comment
      above the renamed function to cover 403
- [x] tests: 403 `*runtime.APIError` → new summary, exit code nil, contains "Editor"; 401 still
      exit 2 (both existing paths); 403 wrapped in `*url.Error` documented as network error (test
      pins current behaviour so the caveat stays visible)
      ➕ the 403 test also asserts the suggestion does **not** mention `login update` — the whole point
      of the branch is that re-authenticating cannot help.
- [x] gates — must pass before Task 7a
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (`cmd/grafanapi/fail` verbose: all 4 `TestErrorToDetailedError_*` groups PASS, incl. both
      pre-existing 401 paths and the untouched k8s Forbidden subtest); `golangci-lint` back at the
      14-finding baseline (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in
      every touched file; `rm -rf docs/reference/cli && go run ./scripts/cmd-reference` → no drift;
      confidentiality gate empty.
      ⚠️ the rename left five stale `convertSessionErrors` mentions in **Go comments** elsewhere; all
      five were updated (comment-only, zero behaviour change): `internal/alerts/state.go`,
      `internal/alerts/state_test.go` (in scope) **plus** `internal/explore/run.go`,
      `internal/session/errors.go`, `cmd/grafanapi/session/refresh.go`. Those last three are packages
      Task 10 asserts are "untouched" — its `git diff --stat` check must accept exactly one comment
      line each. `AGENTS.md`'s two mentions (lines ~460, ~961) are deliberately left for Task 8, which
      already owns the 403-converter doc edit.

### Task 7a: `alerts pull` wiring

**Files:**
- Create: `cmd/grafanapi/alerts/pull.go`; Modify: `cmd/grafanapi/alerts/command.go`,
  `cmd/grafanapi/alerts/command_test.go`

- [x] `pull` command (flags per Technical Details; `-o` restricted to yaml/json), RunE: Validate →
      LoadConfig → ClientFromContext → `alerts.Pull` → `alerts.WriteBundle` → `cmdio.Success` summary
      (counts per kind, path)
      ➕ the file format is taken from `opts.IO.Codec().Format()` rather than by casting
      `opts.IO.OutputFormat` to a `format.Format`: `Codec()` is the same lookup `Validate` already
      passed, so `WriteBundle` can never be handed a string no codec answers to. The `Codec()` call
      therefore sits **before** `alerts.Pull`, so a format problem still costs no network call.
      ➕ `--folder-uid` is a `StringArrayVar` (like `explore`'s `--param`), not the `StringSliceVarP`
      `resources` uses for `-p`: a folder UID is an opaque identifier and comma-splitting it would
      silently invent two lookups out of one.
      ➕ `--path`/`--max-concurrent` carry `resources`' own validation and byte-identical messages
      (`--path is required`, `max-concurrent must be greater than zero`), so the two command groups
      cannot drift on the same flag.
      ➕ `keepFolders` is `--folder-uid` when given and nil otherwise, which is exactly
      `WriteBundle`'s nil-vs-empty contract; the summary counts the policy tree as 0 or 1 rather
      than naming it, so one line reads uniformly across the three kinds.
      ➕ `command.go`'s "read-only" wording is deliberately **left alone**: `pull` reads from Grafana
      and writes only to disk, so the package doc's "no write method of the generated client's
      provisioning service is wired up" is still literally true. Task 7c owns that rewrite.
- [x] register under `alerts`
      ➕ added between `contact-points` and `rules` so Cobra's alphabetical help ordering is the
      order the group is registered in.
- [x] tests (`CommandTestCase` + temp dir + `httptest`): layout written, yaml default, json with `-o`;
      `--folder-uid` filter; unknown uid → error; 401 → exit 2
      ➕ `pull_test.go` adds a request-recording `pullFixture` (a path→`pullStub` table plus a
      `sawPath` assertion) rather than another `newTestServer` variant: a pull drives five different
      addresses and the folder-check assertions are about *which* were requested. It reuses
      `command_test.go`'s `newTestConfig`, `failUnexpectedPath`, `rotatePath`, `alertRulesPath`,
      `contactPointsPath` and `contactPointsBody` unchanged.
      ➕ also covered: `-o table` **and** `-o toml` both rejected with "json, yaml" and nothing
      written (the table codec is deliberately not registered); a filtered pull prunes the selected
      folder's stale file and leaves the unselected folder's alone; the unknown-folder error is
      raised *before* the rule listing (`sawPath(alertRulesPath)` false); a 500 leaves no partial
      bundle; `--path ""` and `--max-concurrent 0|-2` fail before any config load;
      `--config /nonexistent` → error, exit code nil.
      ⚠️ two lint traps in the new test file, both caught by the gate and fixed: `staticcheck`
      QF1008 rejects `fixture.Server.Close` on an embedded `*httptest.Server` (`fixture.Close`), and
      `unparam` reports the shared setup helper's `*pullFixture` result as unused, so it returns
      `(configFile, dir, restore)` only — the tests that assert requested paths build their own
      route table and keep the fixture themselves.
- [x] gates — must pass before Task 7b
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (all 10 `Test_PullCommand_*` groups PASS); `golangci-lint` back at the 14-finding baseline
      (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the three touched
      files; `rm -rf docs/reference/cli && go run ./scripts/cmd-reference` twice → the expected new
      `grafanapi_alerts_pull.md` plus an updated `grafanapi_alerts.md`, then `git diff --exit-code
      docs/reference` clean; confidentiality gate empty. `mkdocs.yml` has no explicit nav, so the new
      reference page needs no nav edit.

### Task 7b: `alerts push` wiring + plan table

**Files:**
- Create: `cmd/grafanapi/alerts/push.go`, `cmd/grafanapi/alerts/plan_table.go`, `plan_table_test.go`;
  Modify: `command.go`, `command_test.go`

- [x] `push` command (`-p`, `--dry-run`, `--ui-editable`, `onerror.BindFlag`, `--max-concurrent`);
      RunE: Validate → LoadConfig → ClientFromContext → `alerts.ReadBundle` →
      `alerts.Push(…, PushOptions{StopOnError: mode.StopOnError(), OrgID: gCtx.Grafana.OrgID, …})`;
      dry-run renders the plan; apply prints one `cmdio` line per change (✔/✘) + summary and returns
      `mode.FailOnErrors(len(result.Failed))`
      ⚠️ **`onerror.Mode.FailOnErrors()` takes no argument** — it is the bool predicate
      `resources push` already uses (`FailOnErrors() && summary.FailedCount() > 0`), not the
      `FailOnErrors(n) error` the plan wrote. Changing its signature would touch five `resources`
      call sites for nothing, so `reportPush` keeps the predicate and builds the error itself.
      ➕ that error **wraps the first failure** (`"%d change(s) failed to push: %w"`) rather than
      only counting them: a write Grafana refused must still render as Task 6's permission-denied
      message, which `fail.ErrorToDetailedError` derives from the underlying `*runtime.APIError`.
      Counting alone would flatten every cause into one opaque "N changes failed".
      ➕ Applied lines (Unchanged included) print first and the failures last, each half already in
      plan order, so the thing to act on is the last thing on screen.
      ➕ `--ui-editable`'s help text says "objects this push CREATES", mirroring `PushOptions`'
      own doc, and `-p` is a single `StringVarP` (a bundle is one tree) with `resources`'
      byte-identical `--path is required` / `max-concurrent must be greater than zero` messages.
      ➕ registered between `pull` and `rules`, keeping the registration order Cobra's alphabetical
      help ordering prints.
      ⚠️ `command.go`'s package doc ("read-only by construction: no write method … is wired up")
      and the group's `Long` ("Every command in this group is read-only") became **false** the
      moment `push` was registered, so both sentences were corrected here to "reading needs Viewer,
      pushing needs Editor". Task 7c still owns the full rewrite (the group `Short`, `rules.go`,
      `internal/alerts/rules.go`); this was the minimum that stops the shipped help from lying.
- [x] `plan_table.go`: a plain renderer (not a registered codec) writing `ACTION KIND ID DETAIL` via
      `newTableWriter` + `cell`; multi-line `Detail` as indented rows; summary "N to create, M to
      update, K unchanged"
      ➕ the first detail line shares the change's own row and only the rest become indented
      follow-on rows, so a single-line detail stays on one line; an entry with no detail still
      emits the empty DETAIL cell, which is what keeps tabwriter's columns aligned.
- [x] tests: dry-run prints plan and performs no writes; apply prints results; `--on-error` invalid →
      validation error; `fail` vs `ignore` exit behaviour with one failed change; 403 → permission
      summary, exit 1; 401 → exit 2; renderer rows incl. detail lines, control chars sanitised
      ➕ `plan_table_test.go` is a **whitebox `package alerts`** test with `//nolint:testpackage`,
      exactly as `table_test.go` already is: the renderer is unexported and is not a codec anyone
      selects with `-o`, so driving it directly is what lets the tests pin the row shape without a
      command, a config and an httptest server. `push_test.go` stays blackbox (`alerts_test`).
      ⚠️ the folder pre-check runs for an **update** too, not only a create, so the command
      fixture's base route table has to script `GET /api/folders/<uid>` for the happy path — a push
      of an existing group is not "reads only what the plan needs" in that one respect.
      ➕ `push_test.go` reuses `pull_test.go`'s `pullStub`/`okStub`/`groupPath`/`folderPath`/
      `policiesPath` and `command_test.go`'s `newTestConfig`/`rotatePath`, adding only a
      method+path-keyed `pushFixture` (a push GETs and writes the same addresses) with `saw` and
      `writes` helpers.
      ➕ the bundle is written as **literal YAML files** rather than through `WriteBundle`: strict
      decoding of exactly the key spellings a user hand-edits (`folderUid` on the group, `folderUID`
      on the rule, `for: 1m` into a `strfmt.Duration`) is part of what the command promises.
      ➕ also covered: the apply path issues all three writes and never lists every rule; a missing
      bundle directory fails before any write; `--config /nonexistent` → error, exit code nil.
      ⚠️ the `unparam` trap once more: a `contactPointPath(uid)` helper every caller passed
      `"cp-uid-1"` was reported, so it is a `pointPath` **constant** instead.
- [x] gates — must pass before Task 7c
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (`cmd/grafanapi/alerts` re-run `-count=3`, no flake); `golangci-lint` back at the 14-finding
      baseline (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the four
      touched files; `rm -rf docs/reference/cli && go run ./scripts/cmd-reference` twice → the
      expected new `grafanapi_alerts_push.md` plus an updated `grafanapi_alerts.md` (whose Synopsis
      carries the corrected group wording), then `git diff --exit-code docs/reference` clean;
      confidentiality gate empty.

### Task 7c: `alerts rules pause|resume|delete` wiring + doc rewrite

**Files:**
- Create: `cmd/grafanapi/alerts/ruleops.go`; Modify: `cmd/grafanapi/alerts/rules.go`,
  `cmd/grafanapi/alerts/command.go`, `internal/alerts/rules.go` (package doc), `command_test.go`

- [x] `pause`/`resume`/`delete` under `rules` (`MinimumNArgs(1)`, `--dry-run`), per-uid `cmdio` result
      lines, non-zero exit if any uid failed
      ➕ **pause and resume are one constructor**, `rulesPauseCmd(configOpts, paused bool)`, registered
      twice — they are one write, so two near-identical `cobra.Command` literals would be both a `dupl`
      finding and a place for the two help texts to drift. Everything that differs lives in a
      `pauseWording{short, action, gerund}` the constructor picks by the same bool; the three
      operations themselves are one `ruleOperation{verb, done, skipped, apply}` value, so `runRuleOp`
      and `reportRuleOp` are written once for all three.
      ➕ `deleteOperation`'s `apply` is `alerts.DeleteRules` **directly** (identical signature);
      wrapping it in a closure is a `gocritic` unlambda finding. `SetRulesPaused` still needs its
      closure, which is where `RuleOpOptions{Paused}` is filled in.
      ➕ there is **no `Validate()`**: the uids are positional arguments Cobra has already counted and
      `--dry-run` is a bool, so `RunE` starts at `LoadConfig` (the order the rest of the group has,
      minus a step that would have nothing to check).
      ➕ result lines are `<uid> (<title> in <group>) <outcome>` — the group is named because a pause
      rewrites it whole, and a uid that could not be looked up at all prints on its own rather than
      with empty parentheses. Outcomes: `paused`/`resumed`/`deleted`, `already paused`/`not paused`
      for a rule that needed no write, and under `--dry-run` the same words prefixed `would be`, in
      the summary line too.
      ➕ the aggregate error mirrors `reportPush` exactly (`"%d rule(s) failed to <verb>: %w"` wrapping
      the **first** failure), which is what keeps a 403 rendering as permission-denied and a
      not-found uid as a plain exit 1. A run the internal layer cut short (401) still prints the
      lines it collected, then returns that error unchanged, so the report never hides an exit 2.
- [x] rewrite `cmd/grafanapi/alerts/command.go` (package doc, `Short`, `Long`), `cmd/grafanapi/alerts/rules.go`
      (`Short`/`Long` "Inspect alert rules" → covers pause/resume/delete) and `internal/alerts/rules.go`
      package doc: reads need Viewer, writes need Editor; list write commands; provenance rule;
      group-replace; last-writer-wins; no admin operations
      ➕ the group `Short` is "Inspect **and manage** …" (same for `rules`): the group still inspects,
      so dropping the word would be as wrong as keeping it alone.
      ➕ `internal/alerts/rules.go`'s package doc is now split under `# Reading` / `# Writing`
      godoc headings, the write half being a one-line-per-file list (files/pull/diff/push/ruleops/
      provenance) plus the secrets paragraph — the reading half is unchanged.
- [x] tests: happy pause/resume/delete; not-found → exit 1; dry-run zero writes; help for the leaves;
      `rg -n -i 'read-only|inspect' cmd/grafanapi/alerts internal/alerts` → only intentional hits
      ➕ `ruleops_test.go` reuses `push_test.go`'s `pushFixture` (method+path routing) rather than
      adding a second recording server, which needed **two small edits there**: `recordedPush` now
      also carries the request body and the `X-Disable-Provenance` header as a `*string` (absent and
      `"true"` are different instructions), and `saw` delegates to a new `requestsFor(method, path)`.
      `record` reads the body first and keeps its `assert.NoError` as the **last statement** of the
      function — the `testifylint` require-error trap Task 4 hit.
      ➕ covered beyond the list: a two-uid pause is exactly **one** group PUT whose body flips only
      the named rules and leaves the group's third rule at `isPaused:false`; resume on an unclaimed
      group sends `X-Disable-Provenance: true` while a claimed one sends no header; already-paused →
      no write at all, exit 0; 401 on **both** the rule lookup and the group write → exit 2; 403 on a
      delete → permission summary, exit 1; a bare `pause`/`resume`/`delete` → Cobra's
      "requires at least 1 arg"; `rules --help` lists all four leaves and each write leaf's help
      shows `UID [UID...]`, `--dry-run` and the Editor requirement.
      ⚠️ the wording gate's remaining hits are all intentional: `contactpoints.go`'s "Inspect
      notification contact points" (that subgroup really is read-only — `list` only), the "Inspect and
      manage" in `command.go`/`rules.go`, `provenance.go`'s "read-only in the UI", and every
      "read-only fields/keys" in `files.go`/`diff.go`/`push.go`/`rules.go` and their tests, which mean
      the fields Grafana owns.
- [x] gates — must pass before Task 8
      ➕ `go build ./...` clean; `go test -race` over every package but `internal/keychain` passes
      (`cmd/grafanapi/alerts` re-run `-count=2`, no flake); `golangci-lint` back at the 14-finding
      baseline (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5) with **zero** findings in the four
      touched files (two were found and fixed on the way: `gocritic` unlambda, above, and a `gci`
      formatting finding in the new test file); `rm -rf docs/reference/cli && go run
      ./scripts/cmd-reference` twice → the three expected new pages
      (`grafanapi_alerts_rules_{pause,resume,delete}.md`) plus the Short/Long updates that ripple into
      `grafanapi.md`, `grafanapi_alerts*.md`, then `git diff --exit-code docs/reference` clean;
      confidentiality gate empty.

### Task 8: Docs, skill, inventory

**Files:**
- Modify: `docs/reference/cli/*` (regenerated), `docs/guides/alerting.md`, `README.md`,
  `skill/grafanapi/SKILL.md`, `AGENTS.md`

- [x] regenerate CLI reference; no drift
      ➕ `rm -rf ./docs/reference/cli && go run ./scripts/cmd-reference` twice then
      `git diff --exit-code docs/reference` → **no drift**, as expected: Task 7c already committed
      every regenerated page and Task 8 changed no Cobra string.
- [x] **stale-wording gate:** `rg -n -i 'read-only|nothing in this group|cannot create' README.md AGENTS.md docs/guides/alerting.md skill/grafanapi/SKILL.md`
      — every hit reviewed and rewritten (guide line ~8 and ~311, README ~47, AGENTS ~136/~273,
      SKILL ~131)
      ➕ all five named hits rewritten. README ~47 ("Read-only access to Grafana Unified
      Alerting") → "Rule definitions joined onto their live evaluation state (a Viewer session is
      enough)"; SKILL ~131 ("Read-only. Nothing in this group can create…") → "Reading needs
      Viewer, writing needs Editor"; guide ~8 and the whole *Scope* section rewritten.
      ➕ **the gate now prints nothing at all for README/guide/SKILL** and exactly five hits for
      AGENTS.md, every one intentional and reviewed: the `contact-points` **subgroup** really is
      read-only (`list` only); "read-only keys"/"read-only fields" in the `files.go` and
      ServerFieldsStripper bullets mean the fields Grafana owns; discovery's "read-only or internal
      resource groups"; and the still-planned "read-only `grafanapi api PATH` passthrough group".
      ⚠️ the guide's own sentence about admin operations was reworded from "Nothing in this
      group performs an org-admin operation" to "**No command in** this group performs…" — the
      original wording was correct but tripped the `nothing in this group` half of the gate, and a
      gate that always fires is a gate nobody reads.
- [x] guide: "Alerting as code" (layout incl. the `folderUid`/`folderUID` spelling gotcha and "do not
      hand-edit `orgID`", pull→edit→push, dry-run first, group replace warning, stale-file pruning on
      pull, folders first, `--ui-editable` = creates only, provenance incl. foreign-provenance refusal,
      secrets stay redacted and are preserved, no concurrency, no prune, per-change output differs from
      `resources push`), "Pause, resume, delete", permissions table (Viewer reads / Editor writes /
      admin-only excluded)
      ➕ the permissions table replaces the old prose section and is **per command** (six rows)
      rather than per role, since that is the question a reader actually arrives with; the
      role-independent facts (silent folder filtering on reads, org-wide contact points, the 403
      rendering and its exit **1**) stay as prose under it.
      ➕ "Alerting as code" is eight `###` subsections (bundle layout / stale-file pruning / review
      the plan first / a rule group is written whole / folders must exist first / who owns an
      object: provenance / secrets stay redacted / applying, and what the output means) so each
      warning has an anchor to link to; the group-replace one is an `!!! danger` admonition, the
      last-writer-wins one an `!!! warning`, matching the file's existing admonition style.
      ➕ the sample plan table and the sample apply/pause/delete output were transcribed from the
      **actual** renderers rather than invented: `ACTION KIND ID DETAIL` with the first detail line
      on the change's own row (`plan_table.go`), the `N to create, M to update, K unchanged`
      summary, `✔`/`✘`/`⚠` from `cmdio.Success/Error/Warning`, `✔ Update RuleGroup
      folder-uid-1/probe-group`, `%d change(s) applied, %d error(s)`, `<uid> (<title> in <group>)
      paused`, `%d rule(s) %s, %d unchanged, %d error(s)` and the literal
      `alerts: rule rule-uid-9 not found`. Counts in each sample add up.
      ➕ an **exit-code table** (0 / 1 / 2) closes "Pause, resume, delete", and the *Stale sessions*
      section gained the contrasting 403 paragraph plus the "a 401 mid-push ends the push rather
      than being recorded as one more failed change" sentence — the one exit-code behaviour a
      reader of `--on-error` would otherwise guess wrong.
      ➕ *Output formats* was scoped to "both **list** commands" with a pointer that `-o` means the
      *file* format on `alerts pull` — the two meanings of one flag letter in one group.
- [x] README quick examples; SKILL.md §5 write commands + safety notes (dry-run first, exit codes,
      never push to a real instance without reviewing the diff); AGENTS.md: command group 8 write
      commands, `cmd/grafanapi/onerror`, `internal/alerts` new files, "Alerts push flow", provenance
      rule under Key Design Patterns, 403 converter; **append** limits (no `--prune`, no server-side
      concurrency, `isPaused` via group PUT) to Technical Debt Inventory → Low Priority — no new section
      ➕ README gained a **second** alerting section ("Example: alerting as code") beside the
      existing inspect one, rather than growing the first: the two need different role callouts
      (Viewer vs Editor) and the write one carries the five limits in a single paragraph.
      ➕ SKILL.md: the write half is a new **§5.1** ("Alerting as code — writes") opening on a
      bolded safety block (always `--dry-run` first; never apply against a real instance without
      the user reviewing the plan; exit codes 0/1/2; never ask for a secret), so an agent reading
      top-down hits the safety rules before the commands. §9 gained the 403/exit-1 line, the
      one-liners three write examples, and the **frontmatter `description`** now names pulling and
      pushing alerting configuration and pausing/resuming/deleting rules — the description is what
      triggers the skill, so a write request would not have matched it otherwise.
      ➕ SKILL.md §5.1 also states the degraded/`unavailable` state applies to `rules list` **only**
      and has no bearing on writes, which is the one cross-section confusion the read half invites.
      ➕ AGENTS.md command group 8: three new bullets (`pull`, `push`, `rules pause|resume|delete`)
      carrying the flags, the nil-vs-empty and creates-only semantics, the exact output lines, and
      the `FailOnErrors()`-is-a-predicate correction; the group header now says "Inspect **and
      manage**" and records that `contact-points` is still read-only.
      ➕ AGENTS.md Core Packages: a new `onerror/` bullet (constants `Ignore`/`Fail`/`Abort`, the
      predicate signature, and **why the flag help still says "resources"** — byte-identical help
      is what keeps the CLI reference from drifting), a rewritten `alerts/` cmd bullet covering
      `pull.go`/`push.go`/`ruleops.go`/`plan_table.go`, and six new `internal/alerts` file bullets
      (`files.go` incl. number normalisation, targeted stripping, kept `orgID`, nil→no file and the
      format-switch cleanup; `provenance.go`; `pull.go` incl. `asAPIError` and the typed folders
      client; `diff.go`; `push.go`; `ruleops.go`).
      ➕ "Alerts Push Flow" was added as a sixth **Execution Flow** in the same fenced-block style
      as the others, six numbered stages plus a closing paragraph mapping the rule ops onto stages
      4-6. Key Design Patterns gained item **7, "Provenance-Aware Alerting Writes (refuse, never
      guess)"**, tied back to pattern 3 (manager metadata) since it is the same instinct.
      ⚠️ the 403 converter is documented as **Common Gotchas item 7**, not as a Key Design
      Pattern: the existing 401 gotcha (item 6) is where `convertAuthErrors` is already described,
      and splitting the two halves of one `switch apiErr.Code` across two sections would be the
      drift the note exists to prevent. Both stale `convertSessionErrors` mentions (the
      `internal/session` bullet and gotcha 6) are renamed, which closes the Task 6 ⚠️.
      ➕ Technical Debt Inventory → Low Priority gained item **14** (four lettered limits: no
      `--prune`, no optimistic concurrency, `isPaused` only through the group PUT on 12.3.x,
      foreign provenance refused) with the usual Current/Impact/Effort/Fix shape. No new section,
      no "Known Issues" heading.
- [x] `mkdocs build --strict` clean; `go build ./...` after SKILL.md (go:embed); confidentiality gate empty
      ➕ `mkdocs build --strict -d <scratch>/site-check` → exit 0, INFO lines only, zero
      WARNING/ERROR (never built into the repo). `go build ./...` clean after the SKILL.md edits.
      `golangci-lint run -c .golangci.yaml` → the unchanged 14-finding baseline
      (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5), all in untouched files.
      ⚠️ the confidentiality gate must be run with the file list **split explicitly** — in `zsh`
      an unquoted `$FILES` does not word-split, so `grep -n … $FILES` treats the whole newline-
      joined list as one file name ("File name too long") and silently checks nothing. Piping the
      list through `tr '\n' '\0' | xargs -0 grep` is the form that works; deleted files
      (`cmd/grafanapi/resources/onerror*.go`) must also be filtered out or they add "No such file"
      noise to an otherwise empty gate. Gate output: **empty**.

### Task 9: Integration verification (docker 12.3, Editor + Viewer)

**Files:** none (results recorded here, counts/exit codes only)

- [x] `make test-env-up`; seed via admin cookie: datasource `testdata`, folder `probe-folder`, one
      A→B(reduce max)→C(threshold gt 0) rule in group `probe-group`, one Slack contact point; create
      `editor`/`viewer` users (POST `/api/admin/users`, PATCH `/api/org/users/<id>` role); build
      `bin/grafanapi`; scripted `login --cookie-stdin --server http://localhost:3000 --org-id 1
      --context local-editor|local-viewer` into a **scratch copy** of
      `testdata/integration-test-config.yaml` (cookie only via stdin; jars deleted afterwards)
      ➕ the env was already up from an earlier session (`make test-env-up` is idempotent);
      `GET /api/health` → `{"database":"ok","version":"12.3.2"}`, i.e. the very build the Context
      section was discovered against. Seeding all returned 2xx; the seeded rule came back
      `provenance: "api"`, the contact point likewise on a follow-up GET (the POST response omits
      the field). Roles confirmed via `GET /api/org/users` → Admin/Editor/Viewer.
      ⚠️ **a stale Keychain item from a previous session's binary blocks `login`**: the first
      `login --context local-viewer` failed with `keychain access timed out after 15s` (exit 1)
      because `grafanapi:local-viewer` still existed, written by a differently-signed build, so
      `securityd` wanted an "Always Allow" dialog. `security delete-generic-password` then a retry
      → exit 0. Nothing to fix in the tool (the error text already says exactly this); an
      integration run must **delete the items first**, not only afterwards.
      ➕ the default `email receiver` contact point (uid `""`, provenance `""`) ships with a clean
      12.3 install, so every bundle in this run carries **two** contact points, and the uid-less one
      is matched by name throughout — an unplanned but useful exercise of that `Plan` branch.
- [x] editor: `alerts pull -p <tmp>` → layout present, file key set as designed; edit group file (add a
      rule, interval 120); `alerts push --dry-run` → `update` with `+ rule`, `~ interval`; `alerts push`
      → exit 0; `rules list` shows both; re-push → all unchanged
      ➕ pull exit 0, summary `1 rule group(s), 2 contact point(s) and 1 policy tree(s) written`;
      layout exactly `rules/probe-folder/probe-group.yaml`, `contact-points.yaml`, `policies.yaml`.
      The on-disk key set matches the Task-1 ⚠️ **verbatim** against a real instance: group
      `folderUid`/`interval` (integer `60`, not `60.0`)/`title`/`rules[]`, each rule
      `folderUID`/`orgID: 1`/`ruleGroup`/`uid`/`isPaused`/`execErrState`/`noDataState`/`for: 1m0s`,
      and **no** `id`/`updated`/`provenance` anywhere; the contact point's `url` is `"[REDACTED]"`.
      ➕ dry-run rendered exactly `Update RuleGroup probe-folder/probe-group  ~ interval 60s→120s`
      with `+ probe-rule-2` on the indented follow-on row, summary `0 to create, 1 to update,
      3 unchanged`; apply printed the four `✔` lines plus `✔ 4 change(s) applied, 0 error(s)`,
      exit 0; `rules list -o json | jq length` → 2.
      ⚠️ **re-pushing the edited file without re-pulling is not a no-op** — the newly created rule
      has a server-assigned uid the local file does not, so `Plan` (uid-matched, Task 3) reports
      `+ probe-rule-2` **and** `- probe-rule-2 (<new uid>)` for the same title, and applying it
      would replace the rule with a fresh copy. `alerts pull` first, then push → `0 to create,
      0 to update, 4 unchanged` (verified). Working as designed, but it is the one workflow trap a
      user meets on their first edit; the guide's "a rule group is written whole" section should say
      "pull again after adding a rule".
- [x] remove a rule from the file → dry-run `- rule`; push → rule gone (replace semantics)
      ➕ dry-run `Update RuleGroup probe-folder/probe-group  - probe-rule-2 (<uid>)`, apply exit 0,
      `rules list -o json | jq length` → 1 (only `probe-rule` left). Group-replace confirmed live.
- [x] contact point: change `recipient`, keep `url: "[REDACTED]"` → push exit 0; admin
      `…/export?decrypt=true` shows the original secret intact (record "intact" only)
      ➕ dry-run `Update ContactPoint <uid>` (the sibling `email receiver` stayed `Unchanged`, so the
      redaction walk did not hide the recipient change); apply exit 0. Admin
      `GET /api/v1/provisioning/contact-points/export?decrypt=true&name=probe-slack&format=json`
      (JSON only with `format=json` — the default body is YAML) → recipient `#probe-changed`,
      secret **intact**. Decision 9 holds end-to-end.
- [x] `rules pause <uid>` → `isPaused: true` in `rules list -o json`; `resume` → false; `delete <uid>`
      → gone; unknown uid → not-found, exit 1
      ➕ pause: `✔ <uid> (probe-rule in probe-group) paused` + `✔ 1 rule(s) paused, 0 unchanged,
      0 error(s)`, exit 0, `isPaused` → `true`; resume → `false`, exit 0. **Decision 7's group PUT
      persists for an Editor**, confirming the Context finding on 12.3.2.
      ➕ delete `--dry-run` → `would be deleted`, zero writes; delete → exit 0, `jq length` → 0.
      ➕ unknown uid → `✘ rule-uid-does-not-exist alerts: rule rule-uid-does-not-exist not found`,
      `✘ 0 rule(s) deleted, 0 unchanged, 1 error(s)`, exit **1**.
      ⚠️ that aggregate error renders under `fail`'s generic **"Unexpected error"** heading (the
      per-uid line above it is correct and the exit code is right). A plain `errors.New` has no
      converter, so this is pre-existing `fail` behaviour rather than an alerts bug, and the 403/401
      paths are unaffected — but "Unexpected error" for a uid the user simply mistyped reads badly.
      Repro: `alerts rules delete <unknown-uid>`. Left for the review phase to decide (a `not found`
      sentinel + converter arm would fix it); **no Go change made in Task 9**.
- [x] viewer: `alerts push` → exit 1 with the permission-denied summary (no "Unexpected error");
      `alerts pull` → exit 0
      ➕ with the rule group deleted the bundle planned a `Create`, so the push actually attempted a
      write: `✘ Create RuleGroup probe-folder/probe-group: … PutAlertRuleGroup (status 403): {}`,
      `⚠ 3 change(s) applied, 1 error(s)`, then Task 6's summary **"Grafana denied the request:
      your user lacks the required permission"** with the Editor/`config current-context`
      suggestion, exit **1**. No "Unexpected error" anywhere.
      ⚠️ worth stating in the guide: a Viewer push whose plan is **all-unchanged** exits **0** — no
      write is attempted, so no 403 is ever raised. The 403 needs a pending change to surface.
      ➕ viewer `alerts pull` → exit 0 (`0 rule group(s), 2 contact point(s) and 1 policy tree(s)`),
      so reads really are Viewer-level including the folder pre-checks.
- [x] revoke editor session (`POST /api/admin/users/<id>/logout`) → `alerts push --dry-run` → exit 2
      ➕ `{"message":"User logged out"}`, then push `--dry-run` → **"Grafana session is stale or
      unauthorized"**, suggestion `grafanapi login update`, detail `alerts: pull rule group
      probe-folder/probe-group: … GetAlertRuleGroup (status 401): {}`, exit **2**. The 401 arrived on
      the push's own pre-fetch (rotation exhausted: the revoked cookie cannot rotate), which is the
      path the exit-code contract cares about.
- [x] `make test-env-clean`; delete scratch config/jars and the Keychain items written by `login`
      (`security delete-generic-password -s grafanapi -a grafanapi:local-editor`, same for
      `local-viewer`); record results
      ➕ `make test-env-clean` removed both containers, both volumes and the network (exit 0);
      scratch config, both cookie jars, both bundles and the seed payloads deleted (scratch dir
      empty). Both Keychain items deleted with exit 0 and **no prompt** (the binary that wrote them
      was the one that deleted them — no rebuild in between); `security find-generic-password`
      afterwards → exit 44 (absent) for both.

**Results (docker `grafana/grafana:12.3` → 12.3.2 OSS, one run, 2026-09-11):**

| Check | Outcome |
|---|---|
| `alerts pull` (Editor) | exit 0 · 1 group / 2 contact points / 1 policy tree · key set matches Task 1 exactly |
| add rule + interval 120 → `push --dry-run` | exit 0 · `~ interval 60s→120s`, `+ probe-rule-2` · `0 to create, 1 to update, 3 unchanged` |
| `push` (apply) | exit 0 · `4 change(s) applied, 0 error(s)` · rules list → 2 |
| re-pull → `push --dry-run` | exit 0 · `0 to create, 0 to update, 4 unchanged` |
| remove rule → `push` | exit 0 · `- probe-rule-2 (<uid>)` in the plan · rules list → 1 (group replace) |
| contact point recipient change, `url: "[REDACTED]"` | push exit 0 · admin decrypt export → **secret intact**, recipient updated |
| `rules pause` / `resume` | exit 0 / exit 0 · `isPaused` `true` then `false` (group PUT persists for Editor) |
| `rules delete --dry-run` / `delete` | exit 0 · zero writes / exit 0 · rules list → 0 |
| `rules delete <unknown>` | exit **1** · `alerts: rule <uid> not found` (under the generic "Unexpected error" heading — ⚠️ above) |
| Viewer `alerts push` (pending Create) | exit **1** · "Grafana denied the request: your user lacks the required permission" · no "Unexpected error" |
| Viewer `alerts pull` | exit 0 |
| Editor session revoked → `push --dry-run` | exit **2** · "Grafana session is stale or unauthorized" · suggestion `login update` |
| teardown | `make test-env-clean` exit 0 · jars/scratch removed · both Keychain items deleted, exit 0, no prompt |

No tool defect blocked any check; the two ⚠️ items above (generic "Unexpected error" heading on a
not-found uid; re-push before re-pull duplicating a uid-less rule) are recorded for the review phase
and were **not** fixed here.

### Task 10: Verify acceptance criteria

- [x] pull/push/pause/resume/delete exist; admin ops absent (`rg 'ResetPolicyTree|Decrypt|DeleteContactpoints' cmd internal` → none)
      ➕ `alerts --help` and all six subcommand helps exit **0**; flags as designed —
      `pull`: `--folder-uid` (stringArray) / `-o` / `-p` / `--max-concurrent`;
      `push`: `--dry-run` / `--ui-editable` / `--on-error` / `--max-concurrent` / `-p` and **no `-o`**;
      `rules pause|resume|delete`: `--dry-run` only. `rg -n '\-\-force|"force"' cmd/grafanapi/alerts
      internal/alerts` finds **no flag** — only the two doc strings explaining why there is none.
      ➕ `rg -n 'ResetPolicyTree|Decrypt\b|DeleteContactpoints|decrypt=true' cmd internal` → **zero
      hits of any kind**, production or test. The only related text is two absence-asserting doc
      comments in `internal/alerts/rules.go` ("nothing here calls an org-admin endpoint, decrypts a
      secret or deletes a contact point"; "never decrypted or printed"), which are lowercase
      `decrypt` and therefore outside the gate pattern.
- [x] provenance three-way logic tested for every write/delete path; group replace documented and
      dry-run lists removals; foreign provenance refused
      ➕ **all six** POST/PUT/DELETE call sites derive `WithXDisableProvenance` from a helper, none
      hard-codes it: `push.go` PostContactpoints (`CreateProvenanceHeader`, :308), PutContactpoint
      (`ProvenanceHeader(remote.Provenance)`, :451), PutPolicyTree (`Create…`/`ProvenanceHeader`,
      :335-338), PutAlertRuleGroup (`Create…`/`GroupProvenanceHeader`, :620-623); `ruleops.go`
      DeleteAlertRule (`ProvenanceHeader(rule.Provenance)`, :154) and PutAlertRuleGroup
      (`GroupProvenanceHeader(group.Rules)`, :212).
      ➕ foreign refused, with tests: `TestProvenanceHeader` table (`file`,
      `converted_prometheus`, `disabled`, an unknown provisioner → refused),
      `TestGroupProvenanceHeader_Foreign`, `TestPush_ForeignProvenanceIsARefusal`
      (`alerts: push rule group folder-uid-1/group-a: provenance "file" is managed outside the API`),
      `TestDeleteRules_ForeignProvenanceIsARefusal`. Mixed refused:
      `TestGroupProvenanceHeader_Mixed` + `TestSetRulesPaused_MixedProvenanceIsARefusal`
      (`rules carry mixed provenance ("api" and "")`, no PUT).
      ➕ removals: `diff_test.go:105/:147` pin `- rule two (rule-uid-2)` in `Change.Detail`, and
      `plan_table_test.go:62` asserts the **rendered** line `- rule three (rule-uid-3)`; confirmed
      live in Task 9 (`- probe-rule-2 (<uid>)`). Group replace documented in
      `docs/guides/alerting.md` §"A rule group is written whole" (:409) with the danger admonition
      "A rule missing from a group file is deleted from that group" (:411-413), and in both
      `alerts push --help` and `alerts --help`.
- [x] exit codes: 0 / 2 (401) / 1 (403, validation, not-found) — unit-tested
      ➕ `*ExitCode == 2` + summary "stale or unauthorized" asserted for **pull**
      (`cmd/.../pull_test.go:400`), **push** (`push_test.go:425`) and the rule ops
      (`ruleops_test.go:294`, a two-case table: the rule lookup on `delete`, the group write on
      `pause`; `resume` is the same `SetRulesPaused` path with the flag inverted).
      ➕ 403 → **nil** ExitCode + "lacks the required permission": `push_test.go:405`,
      `ruleops_test.go:318`. Nil ExitCode for validation/not-found: unknown folder
      (`pull_test.go:377`), unknown uid (`ruleops_test.go:226`), refused object
      (`push_test.go:346`), invalid flags/config (`push_test.go:476`, `pull_test.go:463`); 400 →
      `validationMessage` at `internal/alerts/push_test.go:542`.
      ➕ the folders-client typed-401 path **is** pinned: `internal/alerts/pull_test.go:309` case
      "from the typed folder check" exercises `asAPIError` (`pull.go:127`) converting a
      `*folders.GetFolderByUIDUnauthorized` into a `*runtime.APIError{401}`.
- [x] secrets never printed; `"[REDACTED]"` preserved on push (unit + integration)
      ➕ `rg -n 'Cookie|grafana_session|SessionCookie|decrypt' internal/alerts cmd/grafanapi/alerts`
      over **non-test** code returns only the two absence-asserting doc comments in
      `internal/alerts/rules.go` — no logging, printing or decrypting path anywhere.
      ➕ verbatim preservation: `internal/alerts/push_test.go:408`
      `assert.Contains(t, written[0].body, "\"url\":\"[REDACTED]\"")`, plus the recursive
      `equalRedacted` cases in `diff_test.go` (top-level `url` and nested
      `http_config.basic_auth.password`, with a sibling change still detected). Integration: Task 9
      pulled `url: "[REDACTED]"` and the admin decrypt export showed the **secret intact** after a
      push that changed the recipient.
- [x] `internal/config`, `internal/keychain`, `cmd/grafanapi/session`, `internal/launchd`,
      `internal/session`, `internal/explore` untouched (`git diff --stat alerts-command...HEAD`);
      `cmd/grafanapi/resources` touched **only** by the Task 0 extraction
      ➕ exact stat over the six protected paths — `cmd/grafanapi/session/refresh.go | 2 +-`,
      `internal/explore/run.go | 2 +-`, `internal/session/errors.go | 2 +-`
      (**3 files, 3 insertions, 3 deletions**); `internal/config`, `internal/keychain` and
      `internal/launchd` do not appear at all. The full diff of those three is **one comment line
      each**, the `convertSessionErrors` → `convertAuthErrors` rename recorded under Task 6 — no
      code change.
      ➕ `cmd/grafanapi/resources` stat: `delete.go | 5 ++-`, `get.go | 5 ++-`,
      `onerror.go | 56 ----`, `onerror_test.go | 66 ----`, `pull.go | 5 ++-`, `push.go | 5 ++-`,
      `validate.go | 5 ++-` (**7 files, 15 insertions, 132 deletions**) — the five call sites carry
      only the `onerror` import + `OnErrorMode`→`onerror.Mode` + `bindOnErrorFlag`→`onerror.BindFlag`
      substitution (diff read for `pull.go`/`get.go`), and the two deletions are the extracted files.
- [x] gates + confidentiality gate empty
      ➕ `go build ./...` clean. `go test -race $(go list ./... | grep -v internal/keychain)` →
      exit 0, 27 packages, **no FAIL** (`internal/keychain` excluded by instruction — cgo Keychain).
      `golangci-lint run -c .golangci.yaml` → **exactly 14 issues** (gosec 5 / govet 3 /
      nolintlint 1 / staticcheck 5), the unchanged baseline, and **zero** in any file this branch
      touches — the three `cmd/grafanapi/fail` findings are in `detailed.go`, which is not on the
      branch's change list.
      ➕ `rm -rf ./docs/reference/cli && go run ./scripts/cmd-reference` then
      `git diff --exit-code docs/reference` → **exit 0, no drift** (working tree clean, nothing to
      restore). mkdocs `build --strict` → **exit 0, zero WARNING/ERROR lines**.
      ➕ confidentiality gate over `git diff --name-only --diff-filter=AM alerts-command...HEAD` →
      **no output**. `ls -la` confirms `CLAUDE.md -> AGENTS.md` (symlink intact, not written).
      ➕ audit only: no source file was modified in this task; the sole edit is this plan.

### Task 11: [Final] Wrap up

- [x] re-read AGENTS.md inventory against shipped code
      ➕ every alerts-related AGENTS.md section was checked line by line against the shipped files —
      command group 8's three write bullets (flags, `--folder-uid` as `StringArrayVar`, single `-p`
      on push, `--ui-editable` creates-only, the exact summary/report format strings) against
      `cmd/grafanapi/alerts/{pull,push,ruleops,plan_table,command}.go`; the `onerror/` bullet
      against `cmd/grafanapi/onerror/onerror.go` (constants, `BindFlag`, the byte-identical
      "resources" help text, `FailOnErrors()` as a predicate); the `alerts/` cmd bullet; the six
      `internal/alerts` file bullets against the exported/unexported symbol set of
      `files.go`/`provenance.go`/`pull.go`/`diff.go`/`push.go`/`ruleops.go`; Key Design Pattern 7;
      Common Gotcha 7 against `cmd/grafanapi/fail/convert.go` (`convertAuthErrors`, the
      `switch apiErr.Code`, `permissionDeniedError`'s summary/suggestion, no `ExitCode`); and
      Technical Debt item 14. **One inaccuracy found and fixed**: the "Alerts Push Flow" closing
      paragraph said the rule ops "share steps **4**-6's shape", implying a folder pre-check —
      `ruleops.go` never calls `folderExists` (a rule's folder is wherever Grafana already holds
      it), so it now reads "steps 5-6's shape … and without step 4". Everything else was accurate,
      including that a `--dry-run` returns from `Push` *before* the folder pre-check
      (`push.go:63`), which is what step 3 already claims.
      ➕ the two Task 9 findings flagged for docs are now documented:
      (a) `docs/guides/alerting.md` §"A rule group is written whole" gained a paragraph telling the
      user to `alerts pull` again after a push that **added** a rule (a hand-typed rule has no uid
      until Grafana assigns one, so a second push of the same file plans it as both a `+` and a `-`
      for one title and replaces it) — or to write the assigned `uid` into the file; the same point
      is a new bullet in `skill/grafanapi/SKILL.md` §5.1, where an agent pushing twice in a row is
      the realistic failure;
      (b) the guide's *Permissions* section and SKILL.md §5.1's exit-code bullet now both say a
      `403` needs a **pending change** to surface — a push whose plan is entirely `Unchanged`
      writes nothing and exits **0** even for a Viewer.
      ➕ `README.md` deliberately left unchanged: its "Example: alerting as code" paragraph states
      the five limits correctly and neither finding contradicts it; both are workflow detail that
      belongs in the guide and the skill, not in a quick-start.
      ➕ gates: `mkdocs build --strict` → exit 0, zero WARNING/ERROR; `go build ./...` clean after
      the SKILL.md edit (go:embed); confidentiality gate over the branch's added/modified files →
      **empty**; `ls -la CLAUDE.md` → still `CLAUDE.md -> AGENTS.md` (symlink never written).
### Task 12: [Review] Address code-review findings (iteration 1)

- [x] behaviour changes the review asked for, recorded here because they change what the shipped
      commands do rather than only how the code reads:
      ➕ **`[REDACTED]` on a CREATE is a refusal.** "Push the marker back and the stored secret is
      kept" is a **PUT** property (verified 12.3.2, Context §Secrets); a POST has nothing stored to
      keep and would store the literal string as the secret. `createContactPoint` now walks the
      encoded point recursively (`redactedFields`/`collectRedacted`, dotted paths including array
      indices) and fails that item with `alerts: push contact point <id>: secret field(s) <paths>
      are redacted; set the real value(s) in the file before creating`. The PUT-preserve behaviour
      is unchanged. This is the one case pull→push into a **different** instance hits, since every
      contact point is a create there; documented in the guide, README, SKILL.md and the push help.
      ➕ **`--dry-run` now runs the read-only pre-checks.** It used to return straight after `Plan`,
      so the plan could promise a `Create RuleGroup` the next apply deterministically refuses.
      `Push` now runs the whole apply pipeline with every client call skipped (a `DryRun` guard
      immediately before each one), so the folder pre-check, the provenance classification and the
      redacted-secret check all run. Refusals land in `Result.Failed` and the cmd layer prints them
      as `✘ would fail: …` lines below the plan table. A dry run still exits **0**, and `Push`
      clears `StopOnError` under `DryRun` (stopping at the first refusal would hide the rest of the
      plan). Cost: a dry run now issues the folder GETs it did not before.
      ➕ **`runPush` reports before it returns.** It dropped `result` when `Push` errored, so an
      `--on-error abort` run, or a 401 found halfway through, printed nothing about the contact
      points and rule groups already written. It now mirrors `runRuleOp`: report, then return the
      push's own error (which still wins over the report's aggregate, keeping 401→exit 2 and
      403→exit 1).
      ➕ **Duplicate declarations are refused by `ReadBundle`.** Two files declaring the same
      (folderUid, title) — the path is not authoritative, so nothing else stopped it — and two
      contact-point entries sharing a uid (or, uid-less, a name) both became two racing writes to
      one object, last writer winning, silently. Both now error naming the files/entries.
      ➕ **`alerts.ErrRuleNotFound`** is exported and wrapped with the uid (the message is
      unchanged), with a `convertAlertsErrors` arm in `cmd/grafanapi/fail` rendering "Alert rule not
      found" at exit 1. The cmd layer still returns plain errors, never a `*fail.DetailedError`, so
      401/403 precedence is untouched (pinned by a new table test).
      ➕ **The `!` re-push guard.** A uid-less local rule whose title matches a remote rule the write
      would delete now emits `! <title> (<uid>) would be replaced, not updated; run alerts pull
      first`, sorting above the `+`/`-` pair it explains. It is a **guard, not a match**: `findRule`
      still keys on uid alone, since matching on title would silently update the wrong rule after a
      rename (open item 2 resolved this way rather than by changing the matching).
      ➕ **The 403 suggestion is operation-neutral.** `convertAuthErrors` matches every openapi 403,
      so `explore`/`datasources`/`config check`/`login` were being told alerting writes need Editor.
      The suggestion now names only the user's permissions and `config current-context`; the Editor
      requirement stays in the alerting help and guide. AGENTS.md Gotcha 7 records the scope.
      ➕ **`--on-error` help says "items", not "resources"** (it is on `alerts push` too). The CLI
      reference was regenerated: the `resources` pages change, which is a diff, not drift.
- [x] non-behavioural fixes: `writeTopLevelFile` encodes before removing the other-extension files
      (a marshal failure after the removal left the bundle silently unmanaging contact points);
      `equalProjected` folds the three near-identical comparers; `pauseWording` folds into
      `ruleOperation`; `provenanceMode`/`classifyProvenance` collapse into the `ProvenanceHeader`
      switch; `bundleExtensions` is a read-only package var; `isNilModel` drops its unreachable
      Map/Interface arms; `policyTreeCount` is inlined; `DefaultMaxConcurrent` is exported so the
      flag default is one number in one place.
      ⚠️ two simplifications were **declined**, with reasons: (a) unexported `local`/`remote` fields
      on `Change` — `Change` is an exported, comparable value the cmd layer renders and the tests
      compare whole; hiding two model pointers in it trades ~50 lines of explicit re-pairing for
      hidden state on a public type. (b) collapsing the dual `applyRuleGroupsInOrder` /
      `applyRuleGroupsConcurrently` paths to serial-only — `--max-concurrent` documents bounding the
      *writes*, and the same review asks for a test proving two PUTs overlap; that test now exists
      (barrier-based, under `-race`), which is the opposite of deleting the path.
      ⚠️ `fetchRule`'s nil-payload guard was **kept**: the generated reader allocates the payload
      before decoding into it, so no test can reach it through this client, and deleting a nil-deref
      guard on a third-party response type to gain a coverage point is the wrong trade. Recorded in
      the test file next to the two branches that *are* now covered (empty uid, a rule with no
      title/group).
- [x] tests: array-settings redaction (four cases through `equalRedactedSlice`), a nested policy
      tree body, foreign provenance on contact points **and** the policy tree, `Unchanged` in a real
      push (the Viewer-exits-0 shape) at both layers, both rule-op race messages, two rule-group
      PUTs in flight under `-race`, a rotate-then-replay proving `req.GetBody` works for a
      provisioning PUT **and** POST, `ReadBundle`'s top-level error branches, `WriteBundle`'s nil
      group element / empty-non-nil `keepFolders` / non-integral numbers, `sortResult` with three
      failures, the `!` detail line, the redacted-create refusal, the dry-run refusals, `--on-error
      abort` end to end, `--ui-editable` / `--max-concurrent` / OrgID propagation, and the
      sanitised-name collision aborting a pull. `TestPull_ConcurrencyBoundHonoured` is now
      barrier-based instead of sleep-based, and every `cmd/grafanapi/alerts` fixture registers its
      fake-keychain cleanup with `t.Cleanup` rather than handing back a closure to `defer`.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → 14 findings (gosec 5 / govet 3 / nolintlint 1 / staticcheck 5), the
      unchanged baseline, **zero** in any file this branch touches; CLI reference regenerated and
      committed; `mkdocs build --strict` → exit 0, zero warnings; `go build ./...` again after the
      SKILL.md edit; confidentiality gate over the branch's added/modified files → empty.

### Task 13: [Review] Address code-review findings (iteration 2)

- [x] behaviour changes, all five from the review's critical re-check:
      ➕ **`ReadBundle` validates structure explicitly instead of running the generated
      `Validate`.** The vendored swagger's `noDataState`/`execErrState` enums predate Grafana 11's
      `KeepLast` and its contact-point `type` enum predates webex/sns/mqtt/oncall — and
      `alerts pull` writes those values out verbatim, so a bundle pulled from a current Grafana was
      refused on the way back in by *this tool*, one rule being enough to fail the whole push.
      `validateGroup`/`validateRule`/`validateContactPoint` now check only what a write cannot be
      built without (the generated required-field set minus the enums): required pointers non-nil,
      `folderUid`/`title` non-empty, `interval > 0`, non-empty `data`, a condition, a contact-point
      type/name/settings. `models.Route` keeps its generated `Validate` — it declares no enum.
      Regression test round-trips a `KeepLast` rule and a `webex` contact point through
      `WriteBundle`→`ReadBundle` in **both** formats.
      ➕ **`~ rule order`.** Grafana evaluates a group's rules in the order the group body lists
      them, and the group PUT is the only way to change that — but every rule compared equal by
      uid, so a purely re-ordered file planned as `Unchanged`, no PUT was made, and the next pull
      reverted the file. `sameRuleOrder` compares the uid sequences the two sides *share* (uids
      present on both, in order) and `groupDetail` emits one `~ rule order` line naming no rule.
      Uid-less/dropped rules are excluded: they change the sequence by definition and already have
      a `+`/`-` line — which is why inserting a new rule between two existing ones is **not** a
      re-order (pinned by a third test case).
      ➕ **Contact-point uniqueness is enforced on the resolved remote object.** `ReadBundle`'s
      guard keys on uid-else-name, so `{uid: cp-1, name: team}` plus a uid-less `{name: team}`
      passed as two distinct IDs and then resolved to one remote point — two PUTs to one uid with
      no ordering between them. `planContactPoints` now tracks the matched remote pointer and errors
      naming **both** local entries, since the plan ID is exactly what hides the collision.
      ➕ **A push that applied nothing reports nothing.** A pre-apply failure (a 401 on the fetch
      `Plan` needs, an ambiguous contact-point name) left a zero `Result`, which `runPush`'s
      unconditional report rendered as `✔ 0 change(s) applied, 0 error(s)` — or, under `--dry-run`,
      a bare plan-table header — directly above the error that actually happened. The report is now
      conditional on `pushErr == nil || len(Applied)+len(Failed) != 0`; the mid-flight case (finding
      3 of iteration 1) is unchanged.
      ➕ docs: the guide's "re-ordering a group file is not a change" claim is replaced with the
      `~ rule order` line and its reason; AGENTS.md's `files.go`, `diff.go`, `alerts push` bullets
      and the Push Flow block carry all four changes.
- [x] **new documented limit: a field the vendored model does not declare is dropped on pull and
      wiped on push** (`metadata.editor_settings` is the known one) — the bundle is encoded from the
      generated models, so the field never reaches the file and the push then sends a body without
      it. Added to the guide's Scope limits (now five) and to AGENTS.md Technical Debt item 14 as
      (e), with the fix noted as re-vendoring the client. Worth stating next to the enum change,
      since the two read as contradictory otherwise: *values* inside a known field round-trip,
      *fields* the model lacks do not.
- [x] **left as is**: push refusals render under `cmd/grafanapi/fail`'s generic heading rather than
      an alerting-specific one. That matches `resources push`, the refusal text itself is specific
      (`provenance "file" is managed outside the API`, `folder <uid> does not exist`), and a bespoke
      heading for one command group would be the inconsistency.
- [x] tests: the enum round-trip (both formats), `rule 0: …`-prefixed structural failures for rules
      and contact points, an empty rule element, three `~ rule order` cases (pure re-order /
      re-order + uid-less add / insertion that is not a re-order), two local contact points
      resolving to one remote, and three pre-apply push failures (401 apply, 401 `--dry-run`,
      ambiguous plan) asserting **no** summary line on stdout with the exit codes unchanged
      (401 → 2). `runPush` sets `SilenceUsage` the way `cmd/grafanapi/root` does, so "the command
      printed nothing" is assertable at all — bare, cobra dumps its whole usage block into the same
      buffer the tests read as stdout.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → the unchanged 14-finding baseline (gosec 5 / govet 3 / nolintlint 1 /
      staticcheck 5), **zero** in any file this branch touches; CLI reference unchanged (no flag or
      help text moved); `mkdocs build --strict` → exit 0, zero warnings; confidentiality gate over
      the branch's added/modified files → empty.

### Task 14: [Review] Address code-review findings (iteration 3)

- [x] ➕ **`~ rule order` verified live against Grafana 12.3.2, not assumed.** The detail rests on
      a premise nothing in the unit tests can prove: that a GET returns a group's rules in the
      order the last PUT sent. Probed on the docker instance with a 3-rule group
      (`probe-folder/probe-group`, rules `r1`/`r2`/`r3`):
      `PUT … rule-groups/probe-group` with `[r1,r2,r3]` → `GET` returns `[r1,r2,r3]`; `PUT` with
      `[r3,r1,r2]` → `GET` returns `[r3,r1,r2]`, stably across repeated GETs. The flat
      `GET /api/v1/provisioning/alert-rules` returns the same sequence (`[r3,r1,r2]`), so the
      ordering is persisted (`rule_group_idx`) rather than an artifact of one endpoint's
      serialization. **Conclusion: the detail is correct; kept, with the observation recorded in
      AGENTS.md's `diff.go` bullet so the next reader does not have to re-derive it.**
      End-to-end through the tool on the same instance: `alerts pull` wrote the rules in the
      server's order → `alerts push --dry-run` reported all three objects `Unchanged`; re-ordering
      the three rules in the group file by hand → dry-run reported exactly one detail,
      `~ rule order`, on that group and nothing else; `alerts push` applied it, the server's order
      followed the file, and a fresh `pull` + `push --dry-run` was `Unchanged` again.
- [x] ➕ **rule-uid uniqueness across the bundle.** `validateGroup` refuses a uid repeated inside
      one group file (`rules 0 and 1 both use uid …`, naming the file) and `declareRuleUIDs` — a
      uid→file map threaded through `readGroupFiles` the way `declared` carries group IDs — refuses
      one claimed by two files, naming both. A uid addresses one rule in the whole instance, so the
      cross-file case is the sharper one: the group written second takes the rule out of the first,
      whose plan then reports it missing on every later push. Within one file the damage is to the
      plan instead — `Plan` pairs by uid, so both entries match the one remote rule and a duplicate
      renders as `~ rule order`. Uid-less rules are skipped (Grafana assigns the uid on create).
- [x] ➕ **`folderUID`, `ruleGroup` and `orgID` are optional on read.** `validateRule` hard-required
      all three while push overwrote every one of them (`ruleGroupBody` pins the group file's
      folder and title, `resolveOrgID` picks the org), so the hand-written rule the guide shows was
      refused and `orgID` — a field the guide tells users **not** to edit — was mandatory in every
      file. Now required: `title`, `condition`, `data`, `execErrState`, `noDataState`, `for`.
      A new `requireOrgID` covers the one case push cannot fill: a group that does not exist remotely,
      on a context whose `org-id` is 0 (Grafana Cloud addresses a stack), whose rules carry no
      `orgID` — it fails **that group** by name before the `--dry-run` return, instead of sending a
      body Grafana answers with an opaque 400. (Kept separate from `resolveOrgID` rather than folded
      into its return: a `(*int64, error)` that legitimately answers `nil, nil` trips `nilnil`.) Live-confirmed on 12.3.2: a group file whose
      rule declared none of the four was accepted, and the created rule came back with
      `folderUID: probe-folder`, `ruleGroup: hand-group`, `orgID: 1` and a Grafana-assigned uid.
- [x] minors, both taken: `operationError`'s 400 path returns a `*validationError` wrapper
      (`Error()` renders Grafana's message, `Unwrap()` keeps the generated response reachable) so
      substituting the message no longer costs `errors.As` — which is how `cmd/grafanapi/fail`
      picks between the 401 and 403 renderings; and `reportPush` / `ruleOpLabel` scrub IDs, titles
      and group names through the same `cell` sanitiser the tables use, since a newline in a rule
      title would split the line a script is reading.
- [x] tests: uid repeated in one group and across two group files; a hand-written rule reading back
      with `uid`/`folderUID`/`ruleGroup`/`orgID` absent; that same bundle read through `ReadBundle`
      pushing a body whose rule carries the group file's folder and title plus the context's
      `org-id`; an unresolvable `orgID` failing its group by name with no PUT made; the generated
      response still reachable by `errors.As` through the readable 400 message; a rule-op line whose
      title and group carry a newline and a tab rendering on one line.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → the unchanged 14-finding baseline (gosec 5 / govet 3 / nolintlint 1 /
      staticcheck 5), **zero** in any file this branch touches; CLI reference regenerated → no
      drift; `mkdocs build --strict` → exit 0, zero warnings; confidentiality gate over the
      branch's added/modified files → empty.

- [x] `git mv docs/plans/20260911-alerts-write.md docs/plans/completed/`
- [ ] PR (human) — targeting `alerts-command` (or `main` after it merges) — BLOCKED: PR is opened
      by the human author

## Post-Completion

- **Manual:** `alerts pull` from a real Editor context into a scratch dir, then `alerts push --dry-run`
  → expect all `unchanged`; never run a non-dry-run push against a real instance before reviewing the
  diff.
- **Follow-ups:** `--prune`; contact-point delete; mute timings/templates; `grafanapi api` passthrough
  + shared raw-HTTP seam; lifting `notifications.alerting.grafana.app` from the discovery ignore list.
- **Release-note limits:** no optimistic concurrency (last writer wins); group push replaces the
  group; folders not created; `isPaused` only reliable through the group PUT on 12.3.x; resources
  with `file`/other provenance are refused.

### Task 15: [Review] Address code-review findings (iteration 4)

- [x] ➕ **A 403 on the folder pre-check is a per-folder refusal, not the end of the push.**
      `checkFolders` treated every non-404 as fatal, so one inaccessible folder failed the whole
      rule-group phase — including under `--dry-run`, suppressing the plan for every *other*
      folder. That is the normal case under folder-scoped RBAC: Grafana grants `folders:read` per
      folder, so a bundle spanning ten folders routinely gets a 403 for the eight this session does
      not own. A 403 (matched through the new `isForbidden`/`hasStatus` pair, after `asAPIError`
      has normalised the folders client's typed responses) now fails only that folder's groups,
      with `folder <uid>: permission denied` and the `*runtime.APIError` still reachable so
      `cmd/grafanapi/fail` renders permission-denied and exits 1. A 401 and every transport failure
      stay fatal. `push.go`'s `validationError` was renamed `messageError` and reused for the
      refusal: one type that replaces a rendering while keeping the response reachable, rather than
      two identical ones.
- [x] ➕ **Result lines are scrubbed as whole lines.** `cell()` was applied to the plan ID only,
      while the same untrusted title, folder UID or contact-point name was re-emitted raw inside the
      error rendered on the same line — and `reportRefusals` scrubbed nothing at all. `cmdio`
      itself scrubs nothing, so a name holding `\n✔ …` could forge a success line in the very
      output a human reads before approving a destructive push, and a bidi override could re-order
      the rest of it. `reportPush`, `reportRefusals` and `reportRuleOp` now build the line with
      `fmt.Sprintf`, pass it through `cell` and emit `cmdio.X(out, "%s", scrubbed)`; `ruleOpLabel`
      returns its parts unscrubbed, since the line — not the label — is the unit that has to be
      safe.
- [x] ➕ **A uid-less contact point matched by name converges.** `equalContactPoints` compared the
      local file as written against the remote object: the file carries no `uid`, the remote does,
      so the projections differed by one key and an identical point planned as `Update` and was
      re-PUT on every push. It now compares `contactPointBody(local, remote.UID)` — the body the
      push would actually send.
- [x] tests: a 403 on one of two folders → the other folder's group applied, the failed entry
      carrying the 403 through `errors.As`, and no PUT for the refused one; the same under
      `StopOnError` (stops) and under `--dry-run` (refusal plus the rest of the plan); a 401 on the
      pre-check still fatal and still a `*runtime.APIError`; a contact-point name carrying a
      newline, a forged `✔` marker and a right-to-left override rendering as exactly one scrubbed
      line on both the apply and the dry-run path; the same for a rule title on both the success and
      the failure line of `rules delete`; a uid-less name-matched point planning `Unchanged` when
      identical (redacted secret included) and `Update` when its recipient changed. The forged
      payloads spell the override as a `\u202E` escape inside the YAML/JSON fixture, so no Go source
      file carries the byte `bidichk`/gosec G116 refuse.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → the unchanged 14-finding baseline (gosec 5 / govet 3 / nolintlint 1 /
      staticcheck 5), **zero** in any file this branch touches; CLI reference regenerated → no
      drift; `mkdocs build --strict` → exit 0, zero warnings; confidentiality gate over the
      branch's added/modified files → empty.

### Task 16: [Review] Address code-review findings (iteration 5)

- [x] ➕ **A rule that leaves out the fields push fills in converges.** `equalRules` compared the
      local rule as the file wrote it. `folderUID`, `ruleGroup` and `orgID` are pointers *without*
      `omitempty` in the vendored model, so a file omitting them — which `validateRule` has allowed
      since the previous iteration, and which the guide shows — marshals three `null`s,
      `dropEmptyValues` removes the keys, and the local projection came out three keys short of the
      remote one. Every such rule reported `~ <title> (<uid>)` on every plan and the group was
      re-PUT on every push, clobbering whatever had been edited meanwhile under last-writer-wins and
      re-stamping provenance — with no edit that could ever close it, since `ruleGroupBody` pins all
      three on the way out. The same held for a file whose `orgID` differed from the one
      `resolveOrgID` sends. The plan now compares what the push would **send**: `ruleDetails` takes
      the two whole groups and projects each local rule through `pinRule(rule, group.FolderUID,
      group.Title, match.OrgID)` before `equalRules`. `pinRule` is the pinning `ruleGroupBody`
      already did, extracted so the write and the plan cannot drift; every other field is still
      compared exactly as written.
- [x] tests: a rule identified by uid alone, omitting `folderUID`/`ruleGroup`/`orgID` and otherwise
      identical → the group is `Unchanged`; the same rule with a different `for` → `~ rule one
      (rule-uid-1)`, so the projection hides nothing else; a rule claiming another folder and group
      → `Unchanged` (push overwrites both — the case that previously asserted the opposite); a rule
      claiming another organization → `Unchanged`; the existing `~ rule order`, uid-less `+` and `!`
      guards unchanged.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → the unchanged 14-finding baseline (gosec 5 / govet 3 / nolintlint 1 /
      staticcheck 5), **zero** in any file this branch touches; `mkdocs build --strict` → exit 0,
      zero warnings; confidentiality gate over the branch's added/modified files → empty.

### Task 17: [Review] Address code-smell findings

- [x] ➕ **`push.go` stopped being the package's helper drawer.** The write-body projections
      (`pinRule`, `ruleGroupBody`, `contactPointBody`, `routeBody`, `resolveOrgID`, `requireOrgID`)
      moved to `internal/alerts/body.go` and the error vocabulary (`ErrRuleNotFound`,
      `messageError`, `operationError`, `validationMessage`) to `internal/alerts/errors.go`, both
      shared by `push.go`, `ruleops.go` and `diff.go`; `groupRefID` moved to `pull.go`, beside the
      `GroupRef` it renders. `push.go` now holds `pushRun` and its phases only.
- [x] ➕ **One renderer, one ladder, one options type.** `ruleops.go`'s two hand-built
      `alerts: <op> …: %w` strings now go through `operationError` (the message-only failures stay
      `fmt.Errorf` — there is no response to keep reachable); `cmd/grafanapi/alerts/report.go`'s
      `summaryPrinter(succeeded, failed)` is the single Success/Warning/Error ladder `reportPush`
      and `reportRuleOp` both close with; `WriteBundle(dir, bundle, WriteOptions{Format,
      KeepFolders})` and `DeleteRules(…, RuleOpOptions)` replace their positional forms, which also
      removes the cmd-layer closure that adapted `SetRulesPaused` (`ruleOperation` carries `paused`,
      `apply` is the internal function itself). The cmd constructors are `newDeleteOperation` /
      `newPauseOperation`, no longer colliding by name with `internal/alerts`' `deleteOperation` /
      `pauseOperation` constants.
- [x] ➕ **Plan-detail markers are constants.** `replacedMarker`/`addedMarker`/`removedMarker`/
      `changedMarker` in `diff.go`, with the `"!" < "+" < "-" < "~"` sort-order note on the block
      that defines them; `ruleOrderDetail` is built from `changedMarker`. `ReadBundle`'s three parts
      are one helper each (`readGroupFiles`, `readContactPointsFile`, `readPolicyFile`), and the
      AGENTS.md `provenance.go` bullet was rewritten against the shipped file (it still documented
      `provenanceMode`/`classifyProvenance`/`modeForeign`, removed two iterations ago).
- [x] tests: unchanged except for the mechanical call-site updates the three signatures forced
      (`WriteBundle` in `files_test.go`, `DeleteRules` in `ruleops_test.go`). No output string, exit
      code or wire behaviour changed, which is what the untouched assertions pin.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → the unchanged 14-finding baseline (gosec 5 / govet 3 / nolintlint 1 /
      staticcheck 5), **zero** in any file this branch touches; CLI reference regenerated → no
      drift; `mkdocs build --strict` → exit 0, zero warnings; confidentiality gate over the
      branch's added/modified files → empty.

### Task 18: [Review] Address code-review findings (iteration 6)

- [x] ➕ **The bundle's file names are compared the way the filesystem compares them.** Both
      `writeGroupFiles`' collision guard and `pruneGroupFiles`' staleness test keyed on the
      byte-exact path, but grafanapi is macOS-only and APFS is case-insensitive *and*
      normalisation-insensitive: `rules/F/Alpha.yaml` and `rules/F/alpha.yaml` are one directory
      entry. Two consequences, both data loss with `alerts pull` reporting success. Groups titled
      `Alpha` and `alpha` in one folder (or two folder UIDs differing only in case) walked straight
      past the guard and the second overwrote the first — two groups in, one read back. And when a
      group re-titled `Alpha` → `alpha` was re-pulled, `os.WriteFile` reused the existing entry
      while `os.ReadDir` still reported `Alpha.yaml`, which the `written` set (holding
      `alpha.yaml`) did not contain — so prune deleted the file it had just written and left the
      folder **empty**. Reproduced as failing tests in a real `t.TempDir()` before any production
      change: 0 files remained. Fixed with `foldFileName` = `strings.ToLower(norm.NFC.String(…))`
      (normalise *then* lowercase; lowercasing can itself change composition), applied to the
      collision key, to `written`, to the pruned entry names and to the `KeepFolders` comparison.
      The collision error now names both groups **and** both folders and states that names are
      compared case-insensitively. `golang.org/x/text/unicode/norm` was already vendored and
      already `## explicit` in `vendor/modules.txt`, so no dependency was added and neither
      `go.mod` nor `vendor/` changed.
- [x] tests: `TestWriteBundle_CaseInsensitiveNameCollision` — group titles differing only by case,
      folder UIDs differing only by case, and the composed vs decomposed spelling of one accented
      title → each a collision error naming both, with nothing written;
      `TestWriteBundle_PrunesTheFileItJustWrote` — a real temp dir, `Alpha` re-pulled as `alpha` →
      exactly one file remains and `ReadBundle` returns the one group titled `alpha` (phrased as
      "the group just written survives", so it holds on a case-sensitive filesystem too, where the
      old file is genuinely stale). The existing `TestWriteBundle_SanitizedNameCollision` and
      `TestWriteBundle_PrunesStaleFiles` are unchanged and still pass.
- [x] ➕ **Known limit (left as-is, deliberately):** `ruleDetails` pins a local rule's `orgID` from
      the matched *remote* rule (`match.OrgID`), while `pushRuleGroup` sends what `resolveOrgID`
      returns for the group. The two agree on every payload Grafana produces, because a rule that
      comes back inside a group always carries an `orgID`; they could only diverge for a
      hand-written remote fixture whose group rules omit it, which the server never returns. The
      assumption is recorded here rather than coded around, since threading the group-level
      resolution into the diff would duplicate `resolveOrgID`'s fallback chain for no observable
      difference.
- [x] gates: `go build ./...`; `go test -race` over every package but `internal/keychain`;
      `golangci-lint` → the unchanged 14-finding baseline (gosec 5 / govet 3 / nolintlint 1 /
      staticcheck 5), **zero** in any file this branch touches; CLI reference regenerated → no
      drift; `mkdocs build --strict` → exit 0, zero warnings; confidentiality gate over the
      branch's added/modified files → empty.
