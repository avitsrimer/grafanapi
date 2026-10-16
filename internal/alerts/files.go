package alerts

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/go-openapi/strfmt"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/format"
	"golang.org/x/text/unicode/norm"
)

// The on-disk bundle layout. A bundle directory holds at most one file per rule group, one
// contact-point file and one policy-tree file:
//
//	<dir>/rules/<folderUID>/<group title>.<yaml|json>
//	<dir>/contact-points.<yaml|json>
//	<dir>/policies.<yaml|json>
//
// Path components are sanitized (see sanitizeFileName) and are therefore *not* authoritative:
// the folderUid and title recorded inside a group file are what push sends to Grafana.
const (
	rulesDirName          = "rules"
	contactPointsBaseName = "contact-points"
	policiesBaseName      = "policies"
)

// File modes for everything a bundle write creates. A bundle can carry webhook URLs and other
// user-supplied settings, so files are owner-only even though Grafana redacts the fields it knows
// to be secret.
const (
	bundleDirPerm  fs.FileMode = 0o750
	bundleFilePerm fs.FileMode = 0o600
)

// The read-only keys stripped from every model before it is written. Grafana assigns them and
// rejects or ignores them on the way back in, so keeping them in a file under review would show a
// diff that no push can ever close - and `updated` in particular would otherwise appear in JSON
// files only, because strfmt.DateTime is a struct and `omitempty` does not apply to it.
const (
	idKey         = "id"
	updatedKey    = "updated"
	provenanceKey = "provenance"
)

// The keys the strippers recurse through: a group's rules and a route's child routes are the only
// two places a stripped key can hide below the top level of a file.
const (
	rulesKey  = "rules"
	routesKey = "routes"
)

// bundleExtensions are the file extensions a bundle file may carry, in the order the lookup prefers
// them. Read-only: every use either ranges over it or asks whether it contains an extension.
//
//nolint:gochecknoglobals // read-only lookup table, not mutated after init
var bundleExtensions = []string{".yaml", ".yml", ".json"}

// WriteBundle writes bundle into dir in opts.Format, laying it out as documented above.
//
// Encoding deliberately goes through a generic map rather than handing the generated model to the
// codec: each model is marshalled to JSON, decoded into map[string]any, cleaned of the read-only
// keys, and only then encoded. That is what makes the YAML and JSON files carry the *identical*
// key set - encoding the model directly would leak `updated` into JSON alone - and it is also the
// only place the read-only keys can be removed at every depth.
//
// Numbers survive the round trip as integers where they were integers (json.Number is decoded and
// re-typed, so an interval reads `600` rather than `600.0` in YAML), and `orgID` is kept exactly
// as fetched: push overwrites it, and dropping it here would leave a file that cannot be read back
// into a valid model.
//
// Two groups whose titles sanitize to the same file name are an error naming both - sanitization
// is many-to-one, and silently overwriting one group with another would delete rules on the next
// push. Nothing is written when that happens. File names are compared *case-insensitively* and
// after Unicode normalization (see foldFileName), because the filesystem grafanapi runs on holds
// `Alpha.yaml` and `alpha.yaml` as one entry: two such groups, or two folder UIDs differing only
// in case, are a collision even though their paths differ byte for byte.
//
// Stale files are pruned: any file under rules/ that this write did not produce is removed, so a
// group deleted in Grafana is not resurrected by the next push. opts.KeepFolders restricts that
// pruning to the named folder UIDs. The contact-point and policy files are always rewritten, in
// the requested format only - a file left behind by a pull in the other format is removed rather
// than left to contradict it.
func WriteBundle(dir string, bundle Bundle, opts WriteOptions) error {
	codec, ok := format.Codecs()[opts.Format]
	if !ok {
		return fmt.Errorf("alerts: write bundle: unsupported format %q", opts.Format)
	}

	written, err := writeGroupFiles(dir, codec, bundle.Groups)
	if err != nil {
		return err
	}

	if err := pruneGroupFiles(dir, written, opts.KeepFolders); err != nil {
		return err
	}

	if err := writeTopLevelFile(dir, codec, contactPointsBaseName, bundle.ContactPoints, stripContactPointListFields); err != nil {
		return err
	}

	return writeTopLevelFile(dir, codec, policiesBaseName, bundle.Policies, stripRouteFields)
}

// ReadBundle reads the bundle laid out under dir, in whichever of the two formats each file
// carries (the extension decides; .yaml, .yml and .json are recognized).
//
// Decoding is strict in both formats - the YAML codec is already strict and JSON is decoded with
// DisallowUnknownFields - so a typo'd key fails the same way whichever format the user chose,
// rather than being silently dropped and then deleted from Grafana by the push that follows.
//
// Rule groups, rules and contact points are checked *structurally* - the fields a write cannot be
// built without - and not with their generated Validate; see validateRule and validateContactPoint
// for why. The policy tree still uses it: models.Route declares no enum, so there is nothing there
// to go stale.
//
// Missing parts are not errors: a bundle with no rules/ directory, no contact-point file or no
// policy file simply yields the zero value for that part, so a partial bundle can be pushed. A
// missing dir is an error - the user named it.
func ReadBundle(dir string) (Bundle, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return Bundle{}, fmt.Errorf("alerts: read bundle: %s: %w", dir, err)
	}

	if !info.IsDir() {
		return Bundle{}, fmt.Errorf("alerts: read bundle: %s is not a directory", dir)
	}

	groups, err := readGroupFiles(filepath.Join(dir, rulesDirName))
	if err != nil {
		return Bundle{}, err
	}

	points, err := readContactPointsFile(dir)
	if err != nil {
		return Bundle{}, err
	}

	policies, err := readPolicyFile(dir)
	if err != nil {
		return Bundle{}, err
	}

	return Bundle{Groups: groups, ContactPoints: points, Policies: policies}, nil
}

// Bundle is one alerting configuration as it lives on disk and in memory between a pull and a
// push: the rule groups, the contact points and the notification policy tree, held as the
// generated models the provisioning API both returns and accepts.
//
// The models are used unchanged on purpose - unlike the rules read path there is nothing to join
// or re-project, and the payloads round-trip losslessly, so a local wire struct would only be one
// more thing to keep in step with Grafana.
//
// Every part is optional: a bundle read from a directory holding only rule groups has nil
// ContactPoints and nil Policies, and push simply has nothing to apply for those kinds.
type Bundle struct {
	Groups        []*models.AlertRuleGroup
	ContactPoints []*models.EmbeddedContactPoint
	Policies      *models.Route
}

// WriteOptions controls one bundle write.
type WriteOptions struct {
	// Format is the file format every bundle file is encoded in. A format no codec answers to is
	// an error rather than a fallback.
	Format format.Format
	// KeepFolders names the folder UIDs under rules/ whose stale files may be pruned, and
	// distinguishes nil from empty the way PullOptions.Groups does: nil prunes every folder, while
	// a non-nil empty slice prunes none. A partial pull passes the folders it fetched, so the ones
	// it did not fetch are left alone.
	KeepFolders []string
}

// bundleFilePath returns the path of the single <dir>/<base>.<ext> file that exists, or "" when
// none does. Two files differing only in extension are an error rather than a silent choice: they
// are two bundles disagreeing about the same object, and picking one would push the other's
// absence.
func bundleFilePath(dir, base string) (string, error) {
	found := ""

	for _, ext := range bundleExtensions {
		path := filepath.Join(dir, base+ext)

		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}

		if found != "" {
			return "", fmt.Errorf("alerts: read bundle: %s and %s both exist; keep one", found, path)
		}

		found = path
	}

	return found, nil
}

// compareGroups orders two rule groups by folder UID then title, which is the order a bundle is
// read in regardless of how the filesystem happens to list it.
func compareGroups(a, b *models.AlertRuleGroup) int {
	return cmp.Or(
		cmp.Compare(a.FolderUID, b.FolderUID),
		cmp.Compare(a.Title, b.Title),
	)
}

// decodeBundleFile decodes one bundle file into value, choosing the codec from its extension and
// rejecting unknown keys in both formats. Errors name the file, since a bundle is many files and
// a decoder message alone ("unknown field ...") says nothing about which.
func decodeBundleFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("alerts: read bundle: %s: %w", path, err)
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(value); err != nil {
			return fmt.Errorf("alerts: read bundle: %s: %w", path, err)
		}
	case ".yaml", ".yml":
		if err := format.NewYAMLCodec().Decode(bytes.NewReader(data), value); err != nil {
			return fmt.Errorf("alerts: read bundle: %s: %w", path, err)
		}
	default:
		return fmt.Errorf("alerts: read bundle: %s: unsupported file extension", path)
	}

	return nil
}

// declareRuleUIDs records every uid the group's rules carry in files, which maps a uid to the file
// that already claimed it, and fails naming both files when one is claimed twice.
//
// Rules without a uid are skipped: Grafana assigns one on create, so a bundle may legitimately
// hold any number of them.
func declareRuleUIDs(path string, group *models.AlertRuleGroup, files map[string]string) error {
	for _, rule := range group.Rules {
		if rule == nil || rule.UID == "" {
			continue
		}

		if previous, duplicate := files[rule.UID]; duplicate {
			return fmt.Errorf(
				"alerts: read bundle: %s and %s both declare rule uid %s; keep one", previous, path, rule.UID,
			)
		}

		files[rule.UID] = path
	}

	return nil
}

// encodableValue turns a generated model into the generic value WriteBundle encodes: the model's
// own JSON shape, with integers kept integral and the read-only keys removed by strip.
//
// Plan uses it too - comparing two models through the same projection is what makes a file and a
// fetched model compare equal when they mean the same thing - which is why the error is unprefixed:
// the caller knows whether it is writing or diffing.
func encodableValue(model any, strip func(any)) (any, error) {
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}

	value = normalizeNumbers(value)
	strip(value)

	return value, nil
}

// foldFileName maps a bundle path, or a single component of one, onto the key under which the
// filesystem will actually hold it. grafanapi is macOS-only and APFS is case-insensitive and
// normalization-insensitive, so `Alpha.yaml`, `alpha.yaml` and the decomposed spelling of an
// accented title are one directory entry, not three. Everything that has to answer "is this the
// same file?" - the collision guard in writeGroupFiles and the staleness test in pruneGroupFiles -
// compares folded names, never the bytes a title happened to be written with.
//
// Normalization comes first: lowercasing can itself change how a string is composed, so folding
// the other way round would leave two spellings of one name with different keys.
func foldFileName(name string) string {
	return strings.ToLower(norm.NFC.String(name))
}

// groupFilePath returns the bundle path of one rule group's file, with both path components
// sanitized so a title carrying a separator cannot escape the layout.
func groupFilePath(dir string, codec format.Codec, group *models.AlertRuleGroup) string {
	return filepath.Join(
		dir, rulesDirName,
		sanitizeFileName(group.FolderUID),
		sanitizeFileName(group.Title)+"."+string(codec.Format()),
	)
}

// isBundleFile reports whether name is a file the bundle layout owns, i.e. one pruning may
// remove. Anything else under rules/ - a README, an editor backup - is left alone.
func isBundleFile(name string) bool {
	return slices.Contains(bundleExtensions, strings.ToLower(filepath.Ext(name)))
}

// isNilModel reports whether a bundle part is absent. A nil slice or pointer arrives here boxed in
// a non-nil interface, so a plain `model == nil` would write `null` files for a bundle that simply
// has no contact points or no policy tree.
func isNilModel(model any) bool {
	if model == nil {
		return true
	}

	value := reflect.ValueOf(model)
	switch value.Kind() {
	case reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// normalizeNumbers replaces every json.Number in a decoded value with an int64 where the literal
// is integral and a float64 otherwise.
//
// Without it the two formats disagree on what a number looks like: decoding into `any` the usual
// way turns every number into a float64, which the YAML encoder writes as `600.0` and `43200.0`,
// and a query model read back from such a file would push floats where Grafana had integers.
// json.Number itself cannot be handed to the YAML encoder either - it is a string type, so it
// would be quoted.
func normalizeNumbers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeNumbers(item)
		}

		return typed
	case []any:
		for i, item := range typed {
			typed[i] = normalizeNumbers(item)
		}

		return typed
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}

		if float, err := typed.Float64(); err == nil {
			return float
		}

		return typed.String()
	default:
		return value
	}
}

// pruneGroupFiles removes every bundle file under rules/ that the current write did not produce,
// restricted to keepFolders when that is non-nil (a nil slice prunes every folder, an empty one
// prunes none).
//
// written is keyed by folded name (see foldFileName), and the directory entries this walks are
// folded before they are looked up: on the case-insensitive filesystem grafanapi runs on, a group
// re-titled `Alpha` -> `alpha` is written *into the existing entry*, which os.ReadDir still reports
// under its original spelling. Comparing the two byte-exactly would classify the file that was
// just written as stale and delete it, leaving the folder empty while the pull reported success.
//
// Only files the layout owns are removed, and only one level below rules/, so a directory a user
// added for their own purposes survives. A missing rules/ directory is not an error: there is
// nothing to prune.
func pruneGroupFiles(dir string, written map[string]struct{}, keepFolders []string) error {
	rulesDir := filepath.Join(dir, rulesDirName)

	folders, err := os.ReadDir(rulesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("alerts: write bundle: %s: %w", rulesDir, err)
	}

	var prunable []string
	if keepFolders != nil {
		prunable = make([]string, 0, len(keepFolders))
		for _, folder := range keepFolders {
			prunable = append(prunable, foldFileName(sanitizeFileName(folder)))
		}
	}

	for _, folder := range folders {
		if !folder.IsDir() || (keepFolders != nil && !slices.Contains(prunable, foldFileName(folder.Name()))) {
			continue
		}

		folderDir := filepath.Join(rulesDir, folder.Name())

		files, err := os.ReadDir(folderDir)
		if err != nil {
			return fmt.Errorf("alerts: write bundle: %s: %w", folderDir, err)
		}

		for _, file := range files {
			path := filepath.Join(folderDir, file.Name())
			if _, ok := written[foldFileName(path)]; ok || file.IsDir() || !isBundleFile(file.Name()) {
				continue
			}

			if err := os.Remove(path); err != nil {
				return fmt.Errorf("alerts: write bundle: remove stale %s: %w", path, err)
			}
		}
	}

	return nil
}

// readContactPointsFile reads the bundle's single contact-point file, or reports no contact points
// at all when there is none - which is what tells push "contact points are not managed here"
// rather than "there are none".
//
// Two entries addressing one contact point are two writes to the same object with no ordering
// guarantee between them, so such a file is refused rather than half-applied.
func readContactPointsFile(dir string) ([]*models.EmbeddedContactPoint, error) {
	path, err := bundleFilePath(dir, contactPointsBaseName)
	if err != nil || path == "" {
		return nil, err
	}

	var points []*models.EmbeddedContactPoint
	if err := decodeBundleFile(path, &points); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(points))

	for _, point := range points {
		if point == nil {
			return nil, fmt.Errorf("alerts: read bundle: %s: contains an empty contact point", path)
		}

		if err := validateContactPoint(path, point); err != nil {
			return nil, err
		}

		id := contactPointID(point)
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf(
				"alerts: read bundle: %s: contact point %q is declared twice; keep one", path, id,
			)
		}

		seen[id] = struct{}{}
	}

	return points, nil
}

// readGroupFiles reads every rule-group file under rulesDir (one directory level per folder),
// validates it and returns the groups sorted by folder UID then title, so a bundle read twice is
// read identically. A missing rulesDir yields no groups and no error.
//
// Two files declaring the same (folderUid, title) are an error naming both. The path is not
// authoritative, so nothing stops a user from writing one group into two files - and a push would
// then replace that group twice, concurrently, with whichever body finished last surviving.
//
// A rule uid repeated in two files is refused for a sharper reason: a uid addresses one rule in
// the whole instance, so the two groups would fight over it - the second group's write moves the
// rule out of the first, and the plan for the first group would report the rule as re-ordered or
// missing on every subsequent push. validateGroup rejects the same repetition inside one file.
func readGroupFiles(rulesDir string) ([]*models.AlertRuleGroup, error) {
	folders, err := os.ReadDir(rulesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("alerts: read bundle: %s: %w", rulesDir, err)
	}

	var groups []*models.AlertRuleGroup

	declared := map[string]string{}
	ruleFiles := map[string]string{}

	for _, folder := range folders {
		if !folder.IsDir() {
			continue
		}

		folderDir := filepath.Join(rulesDir, folder.Name())

		files, err := os.ReadDir(folderDir)
		if err != nil {
			return nil, fmt.Errorf("alerts: read bundle: %s: %w", folderDir, err)
		}

		for _, file := range files {
			if file.IsDir() || !isBundleFile(file.Name()) {
				continue
			}

			path := filepath.Join(folderDir, file.Name())

			group := &models.AlertRuleGroup{}
			if err := decodeBundleFile(path, group); err != nil {
				return nil, err
			}

			if err := validateGroup(path, group); err != nil {
				return nil, err
			}

			id := groupID(group)
			if previous, duplicate := declared[id]; duplicate {
				return nil, fmt.Errorf(
					"alerts: read bundle: %s and %s both declare rule group %s; keep one", previous, path, id,
				)
			}

			declared[id] = path

			if err := declareRuleUIDs(path, group, ruleFiles); err != nil {
				return nil, err
			}

			groups = append(groups, group)
		}
	}

	slices.SortStableFunc(groups, compareGroups)

	return groups, nil
}

// readPolicyFile reads the bundle's single notification-policy file, or reports no tree at all
// when there is none, which push reads as "the policy tree is not managed here".
//
// This is the one part still checked with its generated Validate: models.Route declares no enum,
// so there is nothing in it that can go stale against a newer Grafana (see ReadBundle).
func readPolicyFile(dir string) (*models.Route, error) {
	path, err := bundleFilePath(dir, policiesBaseName)
	if err != nil || path == "" {
		return nil, err
	}

	policies := &models.Route{}
	if err := decodeBundleFile(path, policies); err != nil {
		return nil, err
	}

	if err := policies.Validate(strfmt.Default); err != nil {
		return nil, fmt.Errorf("alerts: read bundle: %s: %w", path, err)
	}

	return policies, nil
}

// sanitizeFileName maps a folder UID or group title onto a single path component: the separators
// and control characters that would escape or corrupt the layout become "_", and a leading dot is
// replaced too, so neither ".." nor a hidden file can be produced.
//
// The mapping is many-to-one by design - it must not invent names a user cannot recognize - which
// is exactly why WriteBundle refuses a collision instead of overwriting.
func sanitizeFileName(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == filepath.Separator || unicode.IsControl(r) {
			return '_'
		}

		return r
	}, name)

	if sanitized == "" {
		return "_"
	}

	if strings.HasPrefix(sanitized, ".") {
		sanitized = "_" + sanitized[1:]
	}

	return sanitized
}

// stripContactPointFields removes the read-only keys from one encoded contact point.
func stripContactPointFields(value any) {
	if fields, ok := value.(map[string]any); ok {
		delete(fields, provenanceKey)
	}
}

// stripContactPointListFields removes the read-only keys from an encoded contact-point list, which
// is the shape the bundle file holds.
func stripContactPointListFields(value any) {
	points, ok := value.([]any)
	if !ok {
		return
	}

	for _, point := range points {
		stripContactPointFields(point)
	}
}

// stripGroupFields removes the read-only keys from an encoded rule group. The group itself has
// none; its rules carry all three.
func stripGroupFields(value any) {
	group, ok := value.(map[string]any)
	if !ok {
		return
	}

	rules, ok := group[rulesKey].([]any)
	if !ok {
		return
	}

	for _, rule := range rules {
		stripRuleFields(rule)
	}
}

// stripRouteFields removes the read-only keys from an encoded policy tree, recursing into child
// routes: provenance is stamped on every node, not only the root.
func stripRouteFields(value any) {
	route, ok := value.(map[string]any)
	if !ok {
		return
	}

	delete(route, provenanceKey)

	children, ok := route[routesKey].([]any)
	if !ok {
		return
	}

	for _, child := range children {
		stripRouteFields(child)
	}
}

// stripRuleFields removes the read-only keys from one encoded alert rule. Stripping is targeted
// rather than recursive on purpose: a rule's data[].model is free-form JSON that may legitimately
// carry its own "id", which a blanket recursion would silently delete.
func stripRuleFields(value any) {
	fields, ok := value.(map[string]any)
	if !ok {
		return
	}

	delete(fields, idKey)
	delete(fields, updatedKey)
	delete(fields, provenanceKey)
}

// validateContactPoint checks one decoded contact point for the structure a provisioning write
// cannot be built without.
//
// The generated Validate is deliberately not used: it pins `type` to the enum the vendored swagger
// was generated from, which knows 18 integrations and not Grafana 12's webex, sns, mqtt or
// oncall. A bundle pulled from a current Grafana would then be refused on the way back in, by this
// tool rather than by Grafana. Which integrations exist is the server's business; a name is
// required because a contact point without one cannot be addressed, and settings because the
// write has no body without them.
func validateContactPoint(path string, point *models.EmbeddedContactPoint) error {
	var err error

	switch {
	case point.Type == nil || *point.Type == "":
		err = errors.New("contact point type is required")
	case point.Name == "":
		err = errors.New("contact point name is required")
	case point.Settings == nil:
		err = errors.New("contact point settings are required")
	}

	if err != nil {
		return fmt.Errorf("alerts: read bundle: %s: %w", path, err)
	}

	return nil
}

// validateGroup checks one decoded rule group, and every rule in it, for the constraints the
// provisioning API requires and the model leaves optional.
//
// A uid repeated inside one group is refused here rather than left to Grafana: the group body
// would carry the same rule twice, and the plan - which pairs local and remote rules by uid -
// would match both entries to the one remote rule and report a re-order that is really a
// duplicate.
func validateGroup(path string, group *models.AlertRuleGroup) error {
	var err error

	switch {
	case group.FolderUID == "":
		err = errors.New("folderUid is required")
	case group.Title == "":
		err = errors.New("title is required")
	case group.Interval <= 0:
		err = errors.New("interval must be greater than 0")
	}

	if err != nil {
		return fmt.Errorf("alerts: read bundle: %s: %w", path, err)
	}

	seen := map[string]int{}

	for i, rule := range group.Rules {
		if rule == nil {
			return fmt.Errorf("alerts: read bundle: %s: rule %d is empty", path, i)
		}

		if err := validateRule(rule); err != nil {
			return fmt.Errorf("alerts: read bundle: %s: rule %d: %w", path, i, err)
		}

		if rule.UID == "" {
			continue
		}

		if previous, duplicate := seen[rule.UID]; duplicate {
			return fmt.Errorf(
				"alerts: read bundle: %s: rules %d and %d both use uid %s; keep one", path, previous, i, rule.UID,
			)
		}

		seen[rule.UID] = i
	}

	return nil
}

// validateRule checks one decoded alert rule for the fields a provisioning write cannot be built
// without. The error is unprefixed; validateGroup names the file and the rule's position.
//
// The generated Validate is deliberately not used here either, and for a sharper reason than the
// contact point's: its noDataState and execErrState enums predate Grafana 11's KeepLast, which
// `alerts pull` writes out verbatim - so reading that bundle back would fail, and one such rule
// anywhere would take the whole push with it. Enum membership is the server's business; what is
// checked here is only the structure, so a file that cannot produce a request is refused before
// any network call rather than as an opaque 400 per group.
//
// folderUID, ruleGroup and orgID are deliberately *not* required, even though the provisioning
// API needs all three: the group file is what addresses the group, so ruleGroupBody overwrites a
// rule's folderUID and ruleGroup with the file's own and resolveOrgID supplies the organization.
// Requiring them here would refuse the hand-written rule the guide shows and make orgID - a field
// the guide tells users not to edit - mandatory in every file.
func validateRule(rule *models.ProvisionedAlertRule) error {
	switch {
	case rule.Title == nil || *rule.Title == "":
		return errors.New("title is required")
	case rule.Condition == nil || *rule.Condition == "":
		return errors.New("condition is required")
	case len(rule.Data) == 0:
		return errors.New("data is required")
	case rule.ExecErrState == nil || *rule.ExecErrState == "":
		return errors.New("execErrState is required")
	case rule.NoDataState == nil || *rule.NoDataState == "":
		return errors.New("noDataState is required")
	case rule.For == nil:
		return errors.New("for is required")
	default:
		return nil
	}
}

// writeBundleFile encodes value into path, creating the parent directory. It is the single place
// a bundle file's mode is decided.
func writeBundleFile(path string, codec format.Codec, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), bundleDirPerm); err != nil {
		return fmt.Errorf("alerts: write bundle: %s: %w", filepath.Dir(path), err)
	}

	var encoded bytes.Buffer
	if err := codec.Encode(&encoded, value); err != nil {
		return fmt.Errorf("alerts: write bundle: %s: %w", path, err)
	}

	if err := os.WriteFile(path, encoded.Bytes(), bundleFilePerm); err != nil {
		return fmt.Errorf("alerts: write bundle: %s: %w", path, err)
	}

	return nil
}

// writeGroupFiles writes one file per rule group and returns the set of paths it produced, which
// is what tells pruning which files are current.
//
// Collisions are detected across every group before anything is written, so a bundle that cannot
// be represented on disk leaves the directory untouched rather than half-rewritten. They are
// detected on the *folded* name (see foldFileName), which is what makes two groups titled `Alpha`
// and `alpha` - or two folder UIDs differing only in case - a refusal rather than one file holding
// whichever group was written last.
func writeGroupFiles(dir string, codec format.Codec, groups []*models.AlertRuleGroup) (map[string]struct{}, error) {
	claimed := make(map[string]*models.AlertRuleGroup, len(groups))
	byPath := make(map[string]*models.AlertRuleGroup, len(groups))
	ordered := make([]string, 0, len(groups))

	for _, group := range groups {
		if group == nil {
			continue
		}

		path := groupFilePath(dir, codec, group)

		if previous, ok := claimed[foldFileName(path)]; ok {
			return nil, fmt.Errorf(
				"alerts: write bundle: rule groups %q (folder %q) and %q (folder %q) both map to %s; "+
					"rename one - file names are compared case-insensitively",
				previous.Title, previous.FolderUID, group.Title, group.FolderUID, path,
			)
		}

		claimed[foldFileName(path)] = group
		byPath[path] = group
		ordered = append(ordered, path)
	}

	written := make(map[string]struct{}, len(ordered))

	for _, path := range ordered {
		value, err := encodableValue(byPath[path], stripGroupFields)
		if err != nil {
			return nil, fmt.Errorf("alerts: write bundle: %s: %w", path, err)
		}

		if err := writeBundleFile(path, codec, value); err != nil {
			return nil, err
		}

		written[foldFileName(path)] = struct{}{}
	}

	return written, nil
}

// writeTopLevelFile rewrites <dir>/<base>.<ext> for the requested format and removes the same
// file in every other recognized extension, so switching `-o` cannot leave two files disagreeing
// about the same object. A nil model writes nothing and leaves the directory without the file.
//
// The model is encoded *before* anything is removed. A bundle whose contact-point file is missing
// reads back as "contact points are not managed here", so an encoding failure after the removal
// would silently take them out of the next push rather than failing it.
func writeTopLevelFile(dir string, codec format.Codec, base string, model any, strip func(any)) error {
	path := filepath.Join(dir, base+"."+string(codec.Format()))

	var value any

	present := !isNilModel(model)

	if present {
		encoded, err := encodableValue(model, strip)
		if err != nil {
			return fmt.Errorf("alerts: write bundle: %s: %w", path, err)
		}

		value = encoded
	}

	for _, ext := range bundleExtensions {
		stale := filepath.Join(dir, base+ext)
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("alerts: write bundle: remove %s: %w", stale, err)
		}
	}

	if !present {
		return nil
	}

	return writeBundleFile(path, codec, value)
}
