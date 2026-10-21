package alerts_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The modes WriteBundle creates its files and directories with. A bundle can carry user-supplied
// webhook URLs, so they are part of the contract rather than incidental.
const (
	bundleFilePerm os.FileMode = 0o600
	bundleDirPerm  os.FileMode = 0o750
)

// TestWriteBundle_RoundTrip pins the contract push depends on: everything a write needs survives
// pull -> disk -> push in both formats, and the read-only fields Grafana owns do not.
func TestWriteBundle_RoundTrip(t *testing.T) {
	for _, f := range []format.Format{format.YAML, format.JSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, alerts.WriteBundle(dir, testBundle(), alerts.WriteOptions{Format: f}))

			assertPerm(t, filepath.Join(dir, "rules", "folder-uid-1", "probe-group."+string(f)), bundleFilePerm)
			assertPerm(t, filepath.Join(dir, "rules", "folder-uid-1"), bundleDirPerm)
			assertPerm(t, filepath.Join(dir, "contact-points."+string(f)), bundleFilePerm)
			assertPerm(t, filepath.Join(dir, "policies."+string(f)), bundleFilePerm)

			read, err := alerts.ReadBundle(dir)
			require.NoError(t, err)

			assert.Equal(t, strippedBundle(), read)
		})
	}
}

// TestWriteBundle_IdenticalKeySets is the reason encoding goes through a generic map: the two
// formats must produce the same keys, or a bundle pulled as JSON and pushed back would carry
// fields a YAML bundle does not (`updated` above all, which omitempty cannot suppress).
//
// The expected sets are spelled out rather than only compared to each other, so the two spellings
// Grafana uses in one file (`folderUid` on the group, `folderUID` on a rule) and the presence of
// `orgID` are pinned too.
func TestWriteBundle_IdenticalKeySets(t *testing.T) {
	yamlDir, jsonDir := t.TempDir(), t.TempDir()
	require.NoError(t, alerts.WriteBundle(yamlDir, testBundle(), alerts.WriteOptions{Format: format.YAML}))
	require.NoError(t, alerts.WriteBundle(jsonDir, testBundle(), alerts.WriteOptions{Format: format.JSON}))

	tests := []struct {
		name string
		file string
		want []string
	}{
		{
			name: "rule group",
			file: filepath.Join("rules", "folder-uid-1", "probe-group"),
			want: []string{
				"folderUid",
				"interval",
				"rules",
				"rules[]",
				"rules[].condition",
				"rules[].data",
				"rules[].data[]",
				"rules[].data[].datasourceUid",
				"rules[].data[].model",
				"rules[].data[].model.type",
				"rules[].data[].refId",
				"rules[].execErrState",
				"rules[].folderUID",
				"rules[].for",
				"rules[].isPaused",
				"rules[].noDataState",
				"rules[].orgID",
				"rules[].ruleGroup",
				"rules[].title",
				"rules[].uid",
				"title",
			},
		},
		{
			name: "contact points",
			file: "contact-points",
			want: []string{
				"[]",
				"[].name",
				"[].settings",
				"[].settings.recipient",
				"[].settings.url",
				"[].type",
				"[].uid",
			},
		},
		{
			name: "policies",
			file: "policies",
			want: []string{
				"active_time_intervals",
				"group_by",
				"mute_time_intervals",
				"receiver",
				"routes",
				"routes[]",
				"routes[].active_time_intervals",
				"routes[].group_by",
				"routes[].mute_time_intervals",
				"routes[].receiver",
				"routes[].routes",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fromYAML := decodedKeyPaths(t, filepath.Join(yamlDir, test.file+".yaml"))
			fromJSON := decodedKeyPaths(t, filepath.Join(jsonDir, test.file+".json"))

			assert.Equal(t, test.want, fromYAML)
			assert.Equal(t, fromYAML, fromJSON)

			for _, path := range fromYAML {
				key := path[strings.LastIndex(path, ".")+1:]
				assert.NotContains(t, []string{"id", "updated", "provenance"}, key, "read-only key at %s", path)
			}
		})
	}
}

// TestWriteBundle_SanitizesPathComponents pins the mapping from a folder UID or group title onto
// one path component. Every case here is a name that would otherwise escape the bundle layout or
// hide the file it writes.
func TestWriteBundle_SanitizesPathComponents(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "plain title unchanged", title: "probe group 1", want: "probe group 1"},
		{name: "forward slash", title: "a/b", want: "a_b"},
		{name: "backslash", title: `a\b`, want: "a_b"},
		{name: "traversal", title: "../etc/passwd", want: "_._etc_passwd"},
		{name: "parent directory", title: "..", want: "_."},
		{name: "current directory", title: ".", want: "_"},
		{name: "leading dot hides the file", title: ".hidden", want: "_hidden"},
		{name: "control characters", title: "a\nb\tc\x00d", want: "a_b_c_d"},
		{name: "empty", title: "", want: "_"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			bundle := alerts.Bundle{Groups: []*models.AlertRuleGroup{testGroup("folder-uid-1", test.title)}}

			require.NoError(t, alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: format.YAML}))
			assert.FileExists(t, filepath.Join(dir, "rules", "folder-uid-1", test.want+".yaml"))
		})
	}

	t.Run("folder uid", func(t *testing.T) {
		dir := t.TempDir()
		bundle := alerts.Bundle{Groups: []*models.AlertRuleGroup{testGroup("../folder-uid-1", "probe-group")}}

		require.NoError(t, alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: format.YAML}))
		assert.FileExists(t, filepath.Join(dir, "rules", "_._folder-uid-1", "probe-group.yaml"))
	})
}

// TestWriteBundle_SanitizedNameCollision covers the consequence of that mapping being
// many-to-one: two groups that would share a file are refused by name, and nothing is written -
// silently keeping one of them would delete the other's rules on the next push.
func TestWriteBundle_SanitizedNameCollision(t *testing.T) {
	dir := t.TempDir()

	bundle := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		testGroup("folder-uid-1", "a/b"),
		testGroup("folder-uid-1", "a_b"),
	}}

	err := alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: format.YAML})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a/b"`)
	assert.Contains(t, err.Error(), `"a_b"`)
	assert.Contains(t, err.Error(), "folder-uid-1")

	_, statErr := os.Stat(filepath.Join(dir, "rules"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// TestWriteBundle_CaseInsensitiveNameCollision covers the same guard on the filesystem grafanapi
// actually runs on: APFS folds case (and normalizes Unicode), so two names that differ only that
// way are one directory entry. Keying the guard on the byte-exact path would let the second group
// silently overwrite the first.
func TestWriteBundle_CaseInsensitiveNameCollision(t *testing.T) {
	tests := []struct {
		name     string
		folderA  string
		titleA   string
		folderB  string
		titleB   string
		contains []string
	}{
		{
			name: "group titles differing only by case", folderA: "folder-uid-1", titleA: "Alpha",
			folderB: "folder-uid-1", titleB: "alpha", contains: []string{`"Alpha"`, `"alpha"`, "folder-uid-1"},
		},
		{
			name: "folder uids differing only by case", folderA: "Folder-UID-1", titleA: "probe-group",
			folderB: "folder-uid-1", titleB: "probe-group", contains: []string{"Folder-UID-1", "folder-uid-1"},
		},
		{
			// The composed and decomposed spellings of one title are distinct byte strings that
			// APFS stores under a single entry, so they collide exactly like a case difference.
			name: "unicode spellings of one title", folderA: "folder-uid-1", titleA: "\u00e9quipe",
			folderB: "folder-uid-1", titleB: "e\u0301quipe", contains: []string{"\u00e9quipe", "folder-uid-1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()

			bundle := alerts.Bundle{Groups: []*models.AlertRuleGroup{
				testGroup(test.folderA, test.titleA),
				testGroup(test.folderB, test.titleB),
			}}

			err := alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: format.YAML})
			require.Error(t, err)

			for _, want := range test.contains {
				assert.Contains(t, err.Error(), want)
			}

			_, statErr := os.Stat(filepath.Join(dir, "rules"))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "a bundle that cannot be represented writes nothing")
		})
	}
}

// TestWriteBundle_PrunesTheFileItJustWrote is the other half of that fold, and the one that loses
// data: re-pulling a group whose title changed only in case reuses the existing directory entry on
// APFS, so pruning must recognize the name the filesystem reports as the file it just wrote.
//
// The assertion holds on a case-sensitive filesystem too - there the old file is genuinely stale
// and pruned - which is why it is phrased as "the group just written survives and reads back".
func TestWriteBundle_PrunesTheFileItJustWrote(t *testing.T) {
	dir := t.TempDir()

	first := alerts.Bundle{Groups: []*models.AlertRuleGroup{testGroup("folder-uid-1", "Alpha")}}
	require.NoError(t, alerts.WriteBundle(dir, first, alerts.WriteOptions{Format: format.YAML}))

	second := alerts.Bundle{Groups: []*models.AlertRuleGroup{testGroup("folder-uid-1", "alpha")}}
	require.NoError(t, alerts.WriteBundle(dir, second, alerts.WriteOptions{Format: format.YAML}))

	entries, err := os.ReadDir(filepath.Join(dir, "rules", "folder-uid-1"))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "exactly one file holds the group")

	read, err := alerts.ReadBundle(dir)
	require.NoError(t, err)
	require.Len(t, read.Groups, 1)
	assert.Equal(t, "alpha", read.Groups[0].Title)
}

// TestWriteBundle_PrunesStaleFiles covers the second half of a pull: a group deleted in Grafana
// must not survive on disk and be pushed back, but a partial pull must not delete the folders it
// never fetched.
func TestWriteBundle_PrunesStaleFiles(t *testing.T) {
	tests := []struct {
		name        string
		keepFolders []string
		wantOther   bool
		wantStale   bool
	}{
		{name: "prunes every folder when nil", keepFolders: nil},
		{name: "prunes only the selected folders", keepFolders: []string{"folder-uid-1"}, wantOther: true},
		// An empty-but-non-nil selection is what a pull selecting no folders at all would pass. It
		// must prune nothing rather than everything: nil is the only value meaning "every folder".
		{name: "an empty selection prunes nothing", keepFolders: []string{}, wantOther: true, wantStale: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()

			full := alerts.Bundle{Groups: []*models.AlertRuleGroup{
				testGroup("folder-uid-1", "kept-group"),
				testGroup("folder-uid-1", "removed-group"),
				testGroup("folder-uid-2", "other-group"),
			}}
			require.NoError(t, alerts.WriteBundle(dir, full, alerts.WriteOptions{Format: format.YAML}))

			readme := filepath.Join(dir, "rules", "folder-uid-1", "README.md")
			require.NoError(t, os.WriteFile(readme, []byte("notes"), bundleFilePerm))

			remaining := alerts.Bundle{Groups: []*models.AlertRuleGroup{testGroup("folder-uid-1", "kept-group")}}
			require.NoError(t, alerts.WriteBundle(dir, remaining,
				alerts.WriteOptions{Format: format.YAML, KeepFolders: test.keepFolders}))

			assert.FileExists(t, filepath.Join(dir, "rules", "folder-uid-1", "kept-group.yaml"))
			assert.FileExists(t, readme, "only files the layout owns are pruned")

			stale := filepath.Join(dir, "rules", "folder-uid-1", "removed-group.yaml")
			other := filepath.Join(dir, "rules", "folder-uid-2", "other-group.yaml")

			for path, want := range map[string]bool{stale: test.wantStale, other: test.wantOther} {
				if want {
					assert.FileExists(t, path)
				} else {
					assert.NoFileExists(t, path)
				}
			}
		})
	}
}

// TestWriteBundle_ReplacesTheOtherFormat pins that re-pulling with a different -o does not leave
// two top-level files disagreeing about the same object - which ReadBundle would then refuse.
func TestWriteBundle_ReplacesTheOtherFormat(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, alerts.WriteBundle(dir, testBundle(), alerts.WriteOptions{Format: format.YAML}))
	require.NoError(t, alerts.WriteBundle(dir, testBundle(), alerts.WriteOptions{Format: format.JSON}))

	assert.FileExists(t, filepath.Join(dir, "contact-points.json"))
	assert.NoFileExists(t, filepath.Join(dir, "contact-points.yaml"))
	assert.FileExists(t, filepath.Join(dir, "policies.json"))
	assert.NoFileExists(t, filepath.Join(dir, "policies.yaml"))
}

// TestWriteBundle_UnsupportedFormat keeps the table codec (and any future one) out of the file
// layout: a bundle has no rendering, only two serializations.
func TestWriteBundle_UnsupportedFormat(t *testing.T) {
	err := alerts.WriteBundle(t.TempDir(), testBundle(), alerts.WriteOptions{Format: format.Format("table")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported format "table"`)
}

// TestWriteBundle_OmitsAbsentParts covers a bundle with no contact points and no policy tree: the
// files must be absent rather than hold `null`, which would read back as an empty object and push
// as a wipe.
func TestWriteBundle_OmitsAbsentParts(t *testing.T) {
	dir := t.TempDir()
	bundle := alerts.Bundle{Groups: []*models.AlertRuleGroup{testGroup("folder-uid-1", "probe-group")}}
	require.NoError(t, alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: format.YAML}))

	assert.NoFileExists(t, filepath.Join(dir, "contact-points.yaml"))
	assert.NoFileExists(t, filepath.Join(dir, "policies.yaml"))

	read, err := alerts.ReadBundle(dir)
	require.NoError(t, err)
	assert.Nil(t, read.ContactPoints)
	assert.Nil(t, read.Policies)
	assert.Len(t, read.Groups, 1)
}

// TestWriteBundle_KeepsIntegersIntegral pins the number handling the generic-map encoding needs:
// decoding into `any` the usual way would make every number a float64, and the YAML encoder would
// write `interval: 600.0` and turn a query model's integers into floats on the way back to
// Grafana.
func TestWriteBundle_KeepsIntegersIntegral(t *testing.T) {
	dir := t.TempDir()

	group := testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "always firing"))
	group.Interval = 600
	group.Rules[0].Data[0].Model = map[string]any{"maxDataPoints": 43200, "intervalMs": 1000}

	require.NoError(t, alerts.WriteBundle(dir, alerts.Bundle{Groups: []*models.AlertRuleGroup{group}},
		alerts.WriteOptions{Format: format.YAML}))

	written, err := os.ReadFile(filepath.Join(dir, "rules", "folder-uid-1", "probe-group.yaml"))
	require.NoError(t, err)

	assert.Contains(t, string(written), "interval: 600\n")
	assert.Contains(t, string(written), "maxDataPoints: 43200\n")
	assert.Contains(t, string(written), "intervalMs: 1000\n")
	assert.NotContains(t, string(written), ".0")
}

// TestWriteBundle_KeepsNonIntegralNumbers is the other half of the number handling: a value that
// was genuinely fractional stays fractional, and a literal too large for either Go type is kept
// verbatim rather than silently rounded. A query model is free-form JSON, so both reach the writer.
func TestWriteBundle_KeepsNonIntegralNumbers(t *testing.T) {
	dir := t.TempDir()

	group := testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "always firing"))
	group.Rules[0].Data[0].Model = map[string]any{
		"threshold": 0.75,
		"huge":      json.Number("1e1000"),
	}

	require.NoError(t, alerts.WriteBundle(dir, alerts.Bundle{Groups: []*models.AlertRuleGroup{group}},
		alerts.WriteOptions{Format: format.YAML}))

	written, err := os.ReadFile(filepath.Join(dir, "rules", "folder-uid-1", "probe-group.yaml"))
	require.NoError(t, err)

	assert.Contains(t, string(written), "threshold: 0.75\n")
	assert.Contains(t, string(written), "huge: 1e1000\n", "a literal neither Go type can hold is kept as it was written")
	assert.NotContains(t, string(written), "Inf", "a too-large literal must not be rounded to an infinity")
}

// TestWriteBundle_SkipsNilGroups pins that a nil element in Bundle.Groups is skipped rather than
// dereferenced. Nothing in this package produces one, but Bundle is exported and its slices are
// plain Go slices a caller assembles.
func TestWriteBundle_SkipsNilGroups(t *testing.T) {
	dir := t.TempDir()

	bundle := alerts.Bundle{Groups: []*models.AlertRuleGroup{nil, testGroup("folder-uid-1", "probe-group"), nil}}
	require.NoError(t, alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: format.YAML}))

	read, err := alerts.ReadBundle(dir)
	require.NoError(t, err)
	require.Len(t, read.Groups, 1)
	assert.Equal(t, "probe-group", read.Groups[0].Title)
}

// TestReadBundle_MissingDirectory separates "nothing to push" from "the path is wrong": an empty
// bundle directory is a valid, empty bundle, but a directory the user named and that does not
// exist is a mistake worth reporting.
func TestReadBundle_MissingDirectory(t *testing.T) {
	bundle, err := alerts.ReadBundle(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, alerts.Bundle{}, bundle)

	missing := filepath.Join(t.TempDir(), "absent")
	_, err = alerts.ReadBundle(missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
}

// TestReadBundle_RejectsBadFiles covers the strictness contract: a typo'd key, a malformed file
// and a model that cannot be pushed are all errors that name the file, in both formats - a
// dropped key would otherwise be pushed as a deletion.
func TestReadBundle_RejectsBadFiles(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		wantErr string
	}{
		{
			name:    "unknown key in yaml",
			file:    "probe-group.yaml",
			content: "folderUid: folder-uid-1\ntitle: probe-group\ninterval: 60\nintervall: 60\n",
			wantErr: "intervall",
		},
		{
			name:    "unknown key in json",
			file:    "probe-group.json",
			content: `{"folderUid":"folder-uid-1","title":"probe-group","interval":60,"intervall":60}`,
			wantErr: "intervall",
		},
		{
			name:    "malformed yaml",
			file:    "probe-group.yaml",
			content: "folderUid: [folder-uid-1\n",
			wantErr: "probe-group.yaml",
		},
		{
			name:    "malformed json",
			file:    "probe-group.json",
			content: `{"folderUid":`,
			wantErr: "probe-group.json",
		},
		{
			name:    "missing folder uid",
			file:    "probe-group.yaml",
			content: "title: probe-group\ninterval: 60\n",
			wantErr: "folderUid is required",
		},
		{
			name:    "missing title",
			file:    "probe-group.yaml",
			content: "folderUid: folder-uid-1\ninterval: 60\n",
			wantErr: "title is required",
		},
		{
			name:    "zero interval",
			file:    "probe-group.yaml",
			content: "folderUid: folder-uid-1\ntitle: probe-group\ninterval: 0\n",
			wantErr: "interval must be greater than 0",
		},
		{
			name:    "rule missing a required field",
			file:    "probe-group.yaml",
			content: "folderUid: folder-uid-1\ntitle: probe-group\ninterval: 60\nrules:\n  - uid: rule-uid-1\n",
			wantErr: "rule 0: title is required",
		},
		{
			name:    "rule missing its condition",
			file:    "probe-group.yaml",
			content: "folderUid: folder-uid-1\ntitle: probe-group\ninterval: 60\nrules:\n  - uid: rule-uid-1\n    title: probe\n",
			wantErr: "rule 0: condition is required",
		},
		{
			name:    "an empty rule",
			file:    "probe-group.yaml",
			content: "folderUid: folder-uid-1\ntitle: probe-group\ninterval: 60\nrules:\n  -\n",
			wantErr: "rule 0 is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			folder := filepath.Join(dir, "rules", "folder-uid-1")
			require.NoError(t, os.MkdirAll(folder, bundleDirPerm))
			require.NoError(t, os.WriteFile(filepath.Join(folder, test.file), []byte(test.content), bundleFilePerm))

			_, err := alerts.ReadBundle(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
			assert.Contains(t, err.Error(), filepath.Join(folder, test.file))
		})
	}
}

// TestReadBundle_RejectsBadTopLevelFiles is TestReadBundle_RejectsBadFiles for the two files that
// are not rule groups: the same strictness, reached through a different branch of ReadBundle.
func TestReadBundle_RejectsBadTopLevelFiles(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		wantErr string
	}{
		{
			name:    "an empty contact-point entry",
			file:    "contact-points.yaml",
			content: testPoint("cp-uid-1", "probe") + "-\n",
			wantErr: "contains an empty contact point",
		},
		{
			name:    "a contact point missing a required field",
			file:    "contact-points.yaml",
			content: "- uid: cp-uid-1\n  name: probe\n  settings:\n    recipient: \"#probe\"\n",
			wantErr: "contact point type is required",
		},
		{
			name:    "a contact point with no name",
			file:    "contact-points.yaml",
			content: "- uid: cp-uid-1\n  type: slack\n  settings:\n    recipient: \"#probe\"\n",
			wantErr: "contact point name is required",
		},
		{
			name:    "a contact point with no settings",
			file:    "contact-points.yaml",
			content: "- uid: cp-uid-1\n  name: probe\n  type: slack\n",
			wantErr: "contact point settings are required",
		},
		{
			name:    "two contact points addressing one object",
			file:    "contact-points.yaml",
			content: testPoint("cp-uid-1", "probe") + testPoint("cp-uid-1", "probe-again"),
			wantErr: `contact point "cp-uid-1" is declared twice`,
		},
		{
			name:    "two uid-less contact points sharing a name",
			file:    "contact-points.yaml",
			content: testPoint("", "probe") + testPoint("", "probe"),
			wantErr: `contact point "probe" is declared twice`,
		},
		{
			name:    "an unknown key in the policy tree",
			file:    "policies.yaml",
			content: "receiver: probe\nreceivr: typo\n",
			wantErr: "receivr",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, test.file)
			require.NoError(t, os.WriteFile(path, []byte(test.content), bundleFilePerm))

			_, err := alerts.ReadBundle(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
			assert.Contains(t, err.Error(), path)
		})
	}
}

// TestReadBundle_DuplicateRuleGroupFiles pins the refusal that keeps a bundle from silently
// destroying rules. The path is not authoritative, so nothing stops two files from declaring the
// same (folderUid, title) - and a push would then replace that group twice, concurrently, with
// whichever body finished last surviving.
func TestReadBundle_DuplicateRuleGroupFiles(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, bundleDirPerm))

	group := "folderUid: folder-uid-1\ntitle: probe-group\ninterval: 60\n"
	first := filepath.Join(folder, "probe-group.yaml")
	second := filepath.Join(folder, "a-copy.yaml")
	require.NoError(t, os.WriteFile(first, []byte(group), bundleFilePerm))
	require.NoError(t, os.WriteFile(second, []byte(group), bundleFilePerm))

	_, err := alerts.ReadBundle(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), first, "the error names both files")
	assert.Contains(t, err.Error(), second)
	assert.Contains(t, err.Error(), "folder-uid-1/probe-group")
}

// TestReadBundle_DuplicateRuleUIDAcrossFiles pins the cross-file half of rule-uid uniqueness. A
// uid addresses one rule in the whole instance, so two groups claiming it would fight: whichever
// group is written second takes the rule out of the first, and the first group's plan would then
// report it as missing on every later push.
func TestReadBundle_DuplicateRuleUIDAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, bundleDirPerm))

	first := filepath.Join(folder, "a-group.yaml")
	second := filepath.Join(folder, "b-group.yaml")
	require.NoError(t, os.WriteFile(first, []byte(testGroupFile("a-group", testRuleEntry("rule-uid-1"))), bundleFilePerm))
	require.NoError(t, os.WriteFile(second, []byte(testGroupFile("b-group", testRuleEntry("rule-uid-1"))), bundleFilePerm))

	_, err := alerts.ReadBundle(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), first, "the error names both files")
	assert.Contains(t, err.Error(), second)
	assert.Contains(t, err.Error(), "rule-uid-1")
}

// TestReadBundle_DuplicateRuleUIDInGroup pins the within-file half. The group body would carry the
// same rule twice, and Plan - which pairs local and remote rules by uid - would match both entries
// to the one remote rule and report a re-order that is really a duplicate.
func TestReadBundle_DuplicateRuleUIDInGroup(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, bundleDirPerm))

	path := filepath.Join(folder, "probe-group.yaml")
	content := testGroupFile("probe-group", testRuleEntry("rule-uid-1")+testRuleEntry("rule-uid-1"))
	require.NoError(t, os.WriteFile(path, []byte(content), bundleFilePerm))

	_, err := alerts.ReadBundle(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path, "the error names the group file")
	assert.Contains(t, err.Error(), "rules 0 and 1")
	assert.Contains(t, err.Error(), "rule-uid-1")
}

// TestReadBundle_HandWrittenRuleNeedsNoServerFields is the read half of the hand-typed rule the
// guide shows: folderUID, ruleGroup, orgID and a uid are all things push or Grafana fills in, so
// a file that omits them is a valid bundle rather than an aborted read.
func TestReadBundle_HandWrittenRuleNeedsNoServerFields(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, bundleDirPerm))

	path := filepath.Join(folder, "probe-group.yaml")
	require.NoError(t, os.WriteFile(path, []byte(testGroupFile("probe-group", testRuleEntry(""))), bundleFilePerm))

	bundle, err := alerts.ReadBundle(dir)
	require.NoError(t, err)

	require.Len(t, bundle.Groups, 1)
	require.Len(t, bundle.Groups[0].Rules, 1)

	rule := bundle.Groups[0].Rules[0]
	assert.Empty(t, rule.UID)
	assert.Nil(t, rule.FolderUID, "push pins the group's own folder")
	assert.Nil(t, rule.RuleGroup, "push pins the group's own title")
	assert.Nil(t, rule.OrgID, "push resolves the organization")
	assert.Equal(t, "always firing", *rule.Title)
}

// TestReadBundle_NotADirectory pins that a path naming a file is refused rather than read as an
// empty bundle, which a push would apply as "nothing is managed here".
func TestReadBundle_NotADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerting")
	require.NoError(t, os.WriteFile(path, []byte("not a bundle"), bundleFilePerm))

	_, err := alerts.ReadBundle(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a directory")
	assert.Contains(t, err.Error(), path)
}

// TestReadBundle_AmbiguousTopLevelFile pins that two hand-made files for the same object are
// refused rather than silently resolved: picking one would push the other's absence.
func TestReadBundle_AmbiguousTopLevelFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "policies.yaml"), []byte("receiver: a\n"), bundleFilePerm))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "policies.json"), []byte(`{"receiver":"a"}`), bundleFilePerm))

	_, err := alerts.ReadBundle(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "policies.yaml")
	assert.Contains(t, err.Error(), "policies.json")
}

// TestReadBundle_AcceptsValuesTheVendoredEnumsPredate is the regression test for the bundle that
// could be pulled and never pushed back.
//
// The vendored swagger's noDataState and execErrState enums do not know Grafana 11's KeepLast, and
// its contact-point type enum stops at the integrations that predate Grafana 12's webex, sns, mqtt
// and oncall. A pull writes those values out verbatim, so validating a file against the
// generated enums refused the *whole* bundle on the way back in - one rule was enough. Which values
// exist is the server's business; ReadBundle checks structure only.
func TestReadBundle_AcceptsValuesTheVendoredEnumsPredate(t *testing.T) {
	keepLast := "KeepLast"
	webex := "webex"

	rule := testRule("folder-uid-1", "probe-group", "rule-uid-1", "always firing")
	rule.NoDataState = &keepLast
	rule.ExecErrState = &keepLast

	bundle := alerts.Bundle{
		Groups: []*models.AlertRuleGroup{testGroup("folder-uid-1", "probe-group", rule)},
		ContactPoints: []*models.EmbeddedContactPoint{{
			UID:      "cp-uid-1",
			Name:     "probe-webex",
			Type:     &webex,
			Settings: map[string]any{"room_id": "probe"},
		}},
	}

	for _, f := range []format.Format{format.YAML, format.JSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, alerts.WriteBundle(dir, bundle, alerts.WriteOptions{Format: f}))

			read, err := alerts.ReadBundle(dir)
			require.NoError(t, err)

			require.Len(t, read.Groups, 1)
			require.Len(t, read.Groups[0].Rules, 1)
			assert.Equal(t, keepLast, *read.Groups[0].Rules[0].NoDataState)
			assert.Equal(t, keepLast, *read.Groups[0].Rules[0].ExecErrState)

			require.Len(t, read.ContactPoints, 1)
			assert.Equal(t, webex, *read.ContactPoints[0].Type)
		})
	}
}

// TestWriteRuleGroups_WritesOnlyTheGroupFile pins what separates WriteRuleGroups from WriteBundle:
// the rest of the bundle is left exactly as it was. A write that went through WriteBundle would
// remove the policy file (nil policies), prune other.yaml and rewrite the contact-point file.
func TestWriteRuleGroups_WritesOnlyTheGroupFile(t *testing.T) {
	dir := t.TempDir()

	contactPoints := filepath.Join(dir, "contact-points.yaml")
	policies := filepath.Join(dir, "policies.json")

	require.NoError(t, os.WriteFile(contactPoints, []byte(testPoint("cp-uid-1", "probe-slack")), bundleFilePerm))
	require.NoError(t, os.WriteFile(policies, []byte(`{"receiver":"grafana-default-email"}`+"\n"), bundleFilePerm))

	seedRuleGroup(t, dir, format.YAML, testGroup("folder-uid-1", "other", testRule("folder-uid-1", "other", "rule-uid-9", "other rule")))

	local := remoteRuleGroup()
	local.Rules[0].IsPaused = false
	local.Rules[0].Title = new("edited locally")
	groupPath := seedRuleGroup(t, dir, format.YAML, local)

	before := snapshotDir(t, dir)

	err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, alerts.RuleGroupWriteOptions{
		Format:   format.YAML,
		RuleUIDs: []string{"rule-uid-1"},
	})
	require.NoError(t, err)

	after := snapshotDir(t, dir)
	for path, content := range before {
		if path == groupPath {
			continue
		}

		assert.Equal(t, content, after[path], "%s must be byte-identical", path)
	}

	assert.NotEqual(t, before[groupPath], after[groupPath])

	bundle, err := alerts.ReadBundle(dir)
	require.NoError(t, err)
	require.Len(t, bundle.Groups, 2)

	written := bundle.Groups[1]
	require.Equal(t, "probe-group", written.Title)
	assert.True(t, written.Rules[0].IsPaused, "the named rule is Grafana's version, not the local edit")
	assert.Equal(t, "always firing", *written.Rules[0].Title)
}

func TestWriteRuleGroups_CreatesTheFileInAFreshDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "alerting")

	err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, alerts.RuleGroupWriteOptions{
		Format:   format.JSON,
		RuleUIDs: []string{"rule-uid-1"},
	})
	require.NoError(t, err)

	files := snapshotDir(t, dir)
	require.Len(t, files, 1)
	assert.Contains(t, files, filepath.Join(dir, "rules", "folder-uid-1", "probe-group.json"))
}

func TestWriteRuleGroups_UnsupportedFormat(t *testing.T) {
	err := alerts.WriteRuleGroups(t.TempDir(), nil, alerts.RuleGroupWriteOptions{Format: "toml"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported format "toml"`)
}

// TestWriteRuleGroups_RefusesTheOtherFormat covers the twin the command must never remove (it may
// hold the only copy of a local edit) and never leave beside the new file (every later ReadBundle
// would refuse two files for one group). Overwrite does not lift it.
func TestWriteRuleGroups_RefusesTheOtherFormat(t *testing.T) {
	for _, twinFormat := range []format.Format{format.JSON, format.YAML} {
		for _, overwrite := range []bool{false, true} {
			t.Run(string(twinFormat)+overwriteName(overwrite), func(t *testing.T) {
				dir := t.TempDir()

				seedRuleGroup(t, dir, format.JSON, testGroup("folder-uid-1", "other", testRule("folder-uid-1", "other", "rule-uid-9", "other rule")))

				twin := seedRuleGroup(t, dir, twinFormat, remoteRuleGroup())
				if twinFormat == format.YAML {
					// A .yml file is a twin of a .yaml write: the extensions are compared exactly.
					renamed := strings.TrimSuffix(twin, ".yaml") + ".yml"
					require.NoError(t, os.Rename(twin, renamed))

					twin = renamed
				}

				before := snapshotDir(t, dir)

				err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, alerts.RuleGroupWriteOptions{
					Format:    format.YAML,
					RuleUIDs:  []string{"rule-uid-1"},
					Overwrite: overwrite,
				})
				require.Error(t, err)
				assert.Contains(t, err.Error(), filepath.Base(twin))
				assert.Contains(t, err.Error(), "-o")
				assert.Equal(t, before, snapshotDir(t, dir), "nothing is written, removed or renamed")
			})
		}
	}
}

// TestWriteRuleGroups_RefusesADifferentGroupWithTheSameFileName is the data-loss case on APFS: the
// write would replace the file of a group that is not the one being pulled.
func TestWriteRuleGroups_RefusesADifferentGroupWithTheSameFileName(t *testing.T) {
	tests := []struct {
		name     string
		seeded   string
		pulled   string
		contains []string
	}{
		{name: "title differing by case", seeded: "alpha", pulled: "Alpha", contains: []string{`"alpha"`, `"Alpha"`}},
		{name: "sanitized separator", seeded: "a_b", pulled: "a/b", contains: []string{`"a_b"`, `"a/b"`}},
	}

	for _, test := range tests {
		for _, overwrite := range []bool{false, true} {
			t.Run(test.name+overwriteName(overwrite), func(t *testing.T) {
				dir := t.TempDir()

				seedRuleGroup(t, dir, format.YAML, testGroup("folder-uid-1", test.seeded, testRule("folder-uid-1", test.seeded, "rule-uid-1", "always firing")))

				before := snapshotDir(t, dir)

				pulled := testGroup("folder-uid-1", test.pulled, testRule("folder-uid-1", test.pulled, "rule-uid-1", "always firing"))

				err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{pulled}, alerts.RuleGroupWriteOptions{
					Format:    format.YAML,
					RuleUIDs:  []string{"rule-uid-1"},
					Overwrite: overwrite,
				})
				require.Error(t, err)

				for _, want := range test.contains {
					assert.Contains(t, err.Error(), want)
				}

				assert.Equal(t, before, snapshotDir(t, dir), "the seeded file is byte-identical")
			})
		}
	}
}

func TestWriteRuleGroups_RefusesAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "rules", "folder-uid-1", "probe-group.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), bundleDirPerm))
	require.NoError(t, os.WriteFile(path, []byte("folderUid: [\n"), bundleFilePerm))

	before := snapshotDir(t, dir)

	err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, alerts.RuleGroupWriteOptions{
		Format:    format.YAML,
		RuleUIDs:  []string{"rule-uid-1"},
		Overwrite: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe-group.yaml")
	assert.Contains(t, err.Error(), "cannot be read")
	assert.Equal(t, before, snapshotDir(t, dir))
}

// TestWriteRuleGroups_RefusesLocalDifferencesOutsideTheNamedRules is the safeguard for unpushed
// work: a sibling edit in the file is invisible to a pull that names another rule, so replacing
// the file would silently discard it.
func TestWriteRuleGroups_RefusesLocalDifferencesOutsideTheNamedRules(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(group *models.AlertRuleGroup)
		first string
	}{
		{
			name:  "an edited sibling",
			edit:  func(group *models.AlertRuleGroup) { group.Rules[1].IsPaused = false },
			first: "~ rule two (rule-uid-2)",
		},
		{
			name:  "a dropped sibling",
			edit:  func(group *models.AlertRuleGroup) { group.Rules = append(group.Rules[:1], group.Rules[2:]...) },
			first: "- rule two (rule-uid-2)",
		},
		{
			name: "an added sibling",
			edit: func(group *models.AlertRuleGroup) {
				group.Rules = append(group.Rules, testRule("folder-uid-1", "probe-group", "rule-uid-9", "rule nine"))
			},
			first: "+ rule nine (rule-uid-9)",
		},
		{
			name:  "a changed interval",
			edit:  func(group *models.AlertRuleGroup) { group.Interval = 120 },
			first: "~ interval 60s",
		},
		{
			name:  "re-ordered siblings",
			edit:  func(group *models.AlertRuleGroup) { group.Rules[1], group.Rules[2] = group.Rules[2], group.Rules[1] },
			first: "~ rule order",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()

			local := remoteRuleGroup()
			test.edit(local)

			path := seedRuleGroup(t, dir, format.YAML, local)
			before := snapshotDir(t, dir)
			opts := alerts.RuleGroupWriteOptions{Format: format.YAML, RuleUIDs: []string{"rule-uid-1"}}

			err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), path)
			assert.Contains(t, err.Error(), "1 difference(s)")
			assert.Contains(t, err.Error(), "first: "+test.first)
			assert.Contains(t, err.Error(), "--overwrite")
			assert.Equal(t, before, snapshotDir(t, dir), "the file is byte-identical")

			opts.Overwrite = true
			require.NoError(t, alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, opts))

			bundle, err := alerts.ReadBundle(dir)
			require.NoError(t, err)

			changes, err := alerts.Plan(bundle, alerts.Bundle{Groups: []*models.AlertRuleGroup{remoteRuleGroup()}})
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Equal(t, alerts.ActionUnchanged, changes[0].Action, "with Overwrite the file equals the fetched group")
		})
	}
}

func TestWriteRuleGroups_NamedRulesMayDifferLocally(t *testing.T) {
	dir := t.TempDir()

	local := remoteRuleGroup()
	local.Rules[0].IsPaused = false
	local.Rules[1].Title = new("edited too")
	seedRuleGroup(t, dir, format.YAML, local)

	// Not rule-uid-3: the siblings of the two named rules are equal.
	local.Rules[2].IsPaused = true

	err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup()}, alerts.RuleGroupWriteOptions{
		Format:   format.YAML,
		RuleUIDs: []string{"rule-uid-1", "rule-uid-2"},
	})
	require.NoError(t, err)

	bundle, err := alerts.ReadBundle(dir)
	require.NoError(t, err)
	require.Len(t, bundle.Groups, 1)
	assert.True(t, bundle.Groups[0].Rules[0].IsPaused)
	assert.Equal(t, "rule two", *bundle.Groups[0].Rules[1].Title)
}

// TestWriteRuleGroups_RefusalWritesNothing pins atomicity: every group is checked before the first
// one is written, so a refusal on the second leaves the first group's file absent.
func TestWriteRuleGroups_RefusalWritesNothing(t *testing.T) {
	dir := t.TempDir()

	other := testGroup("folder-uid-2", "other", testRule("folder-uid-2", "other", "rule-uid-9", "other rule"))
	edited := testGroup("folder-uid-2", "other", testRule("folder-uid-2", "other", "rule-uid-9", "other rule"), testRule("folder-uid-2", "other", "rule-uid-8", "sibling"))
	seedRuleGroup(t, dir, format.YAML, edited)

	before := snapshotDir(t, dir)

	err := alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{remoteRuleGroup(), other}, alerts.RuleGroupWriteOptions{
		Format:   format.YAML,
		RuleUIDs: []string{"rule-uid-1", "rule-uid-9"},
	})
	require.Error(t, err)
	assert.Equal(t, before, snapshotDir(t, dir), "the clean group was not written either")
}

// testPoint renders one valid contact-point entry of a contact-points.yaml file, so the tests
// above vary only the thing they are about rather than repeating every required field.
func testPoint(uid, name string) string {
	entry := "- name: " + name + "\n"
	if uid != "" {
		entry = "- uid: " + uid + "\n  name: " + name + "\n"
	}

	return entry + "  type: slack\n  settings:\n    recipient: \"#probe\"\n"
}

// testGroupFile renders one rule-group file around the rule entries testRuleEntry produced.
func testGroupFile(title, rules string) string {
	return "folderUid: folder-uid-1\ntitle: " + title + "\ninterval: 60\nrules:\n" + rules
}

// testRuleEntry renders one rule carrying nothing but the fields a bundle genuinely has to
// supply - folderUID, ruleGroup and orgID are push's business, and a uid is Grafana's - so the
// tests above vary only the uid they are about.
func testRuleEntry(uid string) string {
	entry := "  - title: always firing\n"
	if uid != "" {
		entry = "  - uid: " + uid + "\n    title: always firing\n"
	}

	return entry +
		"    condition: C\n" +
		"    execErrState: Error\n" +
		"    noDataState: NoData\n" +
		"    for: 5m0s\n" +
		"    data:\n" +
		"      - refId: A\n" +
		"        datasourceUid: __expr__\n" +
		"        model:\n" +
		"          type: math\n"
}

// overwriteName suffixes a subtest name with the Overwrite setting it runs under.
func overwriteName(overwrite bool) string {
	if overwrite {
		return "/overwrite"
	}

	return ""
}

// remoteRuleGroup is the group a rule-selecting pull fetched: three rules, so a test can name one
// and have two siblings whose order and content matter.
func remoteRuleGroup() *models.AlertRuleGroup {
	one := testRule("folder-uid-1", "probe-group", "rule-uid-1", "always firing")
	two := testRule("folder-uid-1", "probe-group", "rule-uid-2", "rule two")
	three := testRule("folder-uid-1", "probe-group", "rule-uid-3", "rule three")

	return testGroup("folder-uid-1", "probe-group", one, two, three)
}

// seedRuleGroup writes group into dir through WriteRuleGroups, which is only used here to produce a
// file; it returns the file's path. The target must not exist yet.
func seedRuleGroup(t *testing.T, dir string, f format.Format, group *models.AlertRuleGroup) string {
	t.Helper()

	require.NoError(t, alerts.WriteRuleGroups(dir, []*models.AlertRuleGroup{group}, alerts.RuleGroupWriteOptions{Format: f}))

	return filepath.Join(dir, "rules", group.FolderUID, strings.ReplaceAll(group.Title, "/", "_")+"."+string(f))
}

// snapshotDir returns every file under dir with its content, so a test can assert that a refused
// write changed nothing - not a byte, not a name.
func snapshotDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()

	var paths []string

	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			paths = append(paths, path)
		}

		return err
	})
	require.NoError(t, err)

	files := make(map[string][]byte, len(paths))

	for _, path := range paths {
		content, err := os.ReadFile(path)
		require.NoError(t, err)

		files[path] = content
	}

	return files
}

// assertPerm checks the mode of a file or directory a bundle write created.
func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, want, info.Mode().Perm(), path)
}

// collectKeyPaths flattens a decoded file into dotted key paths, marking a slice with "[]" so a
// key nested inside an array element is distinguishable from a top-level one.
func collectKeyPaths(value any, prefix string, paths *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}

			*paths = append(*paths, path)
			collectKeyPaths(item, path, paths)
		}
	case []any:
		*paths = append(*paths, prefix+"[]")

		for _, item := range typed {
			collectKeyPaths(item, prefix+"[]", paths)
		}
	}
}

// compactStrings removes adjacent duplicates, which a repeated key inside two array elements
// produces.
func compactStrings(values []string) []string {
	compacted := make([]string, 0, len(values))

	for _, value := range values {
		if len(compacted) > 0 && compacted[len(compacted)-1] == value {
			continue
		}

		compacted = append(compacted, value)
	}

	return compacted
}

// decodedKeyPaths reads one written bundle file and returns its sorted, de-duplicated key paths.
func decodedKeyPaths(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var value any

	if filepath.Ext(path) == ".json" {
		require.NoError(t, json.Unmarshal(data, &value))
	} else {
		require.NoError(t, format.NewYAMLCodec().Decode(bytes.NewReader(data), &value))
	}

	var paths []string
	collectKeyPaths(value, "", &paths)
	sort.Strings(paths)

	return compactStrings(paths)
}

// strippedBundle is testBundle as it comes back from disk: the read-only fields Grafana owns are
// gone, everything a push needs is not.
func strippedBundle() alerts.Bundle {
	bundle := testBundle()

	for _, rule := range bundle.Groups[0].Rules {
		rule.ID = 0
		rule.Provenance = ""
		rule.Updated = strfmt.DateTime{}
	}

	bundle.ContactPoints[0].Provenance = ""
	bundle.Policies.Provenance = ""
	bundle.Policies.Routes[0].Provenance = ""

	return bundle
}

// testBundle is one of each kind, with every read-only field set to something non-zero so the
// round-trip tests can see it removed.
func testBundle() alerts.Bundle {
	pointType := models.EmbeddedContactPointTypeSlack

	return alerts.Bundle{
		Groups: []*models.AlertRuleGroup{
			testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "always firing")),
		},
		ContactPoints: []*models.EmbeddedContactPoint{{
			UID:        "cp-uid-1",
			Name:       "probe-slack",
			Type:       &pointType,
			Provenance: "api",
			Settings:   map[string]any{"recipient": "#probe", "url": "[REDACTED]"},
		}},
		Policies: &models.Route{
			Receiver:   "grafana-default-email",
			Provenance: "api",
			Routes: []*models.Route{{
				Receiver:   "probe-slack",
				Provenance: "api",
			}},
		},
	}
}

// testGroup builds a rule group with the interval a push requires and the rules the caller names.
func testGroup(folderUID, title string, rules ...*models.ProvisionedAlertRule) *models.AlertRuleGroup {
	return &models.AlertRuleGroup{
		FolderUID: folderUID,
		Title:     title,
		Interval:  60,
		Rules:     rules,
	}
}

// testRule builds a rule carrying every field the generated validation requires, plus the three
// read-only ones a write must strip.
func testRule(folderUID, group, uid, title string) *models.ProvisionedAlertRule {
	condition := "C"
	execErrState := "Error"
	noDataState := "NoData"
	orgID := int64(1)
	pending := strfmt.Duration(5 * time.Minute)

	return &models.ProvisionedAlertRule{
		UID:          uid,
		Title:        &title,
		Condition:    &condition,
		ExecErrState: &execErrState,
		NoDataState:  &noDataState,
		FolderUID:    &folderUID,
		RuleGroup:    &group,
		OrgID:        &orgID,
		For:          &pending,
		IsPaused:     true,
		Data: []*models.AlertQuery{{
			RefID:         "A",
			DatasourceUID: "__expr__",
			Model:         map[string]any{"type": "math"},
		}},
		ID:         7,
		Provenance: models.Provenance("api"),
		Updated:    strfmt.DateTime(time.Date(2026, time.September, 11, 10, 0, 0, 0, time.UTC)),
	}
}
