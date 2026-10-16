package alerts

import (
	"fmt"

	"github.com/grafana/grafana-openapi-client-go/models"
)

// The provenance vocabulary the provisioning API and this tool share.
//
// Grafana stamps every object it accepts through the provisioning API with a provenance value
// recording who owns it: "" for an object the UI may still edit, "api" for one this API claims,
// and a provisioner's own name ("file", "converted_prometheus", ...) for one that is generated
// elsewhere and re-applied on every restart. models.Provenance is a bare string with no enum, so
// the set is open - which is exactly why the header helpers below have an "everything else" case
// rather than a list of the values seen so far.
const (
	// apiProvenance is what Grafana stores for an object written through the provisioning API
	// without the X-Disable-Provenance header. Such an object is read-only in the UI.
	apiProvenance = "api"

	// disableProvenanceValue is the only X-Disable-Provenance value Grafana acts on. The header
	// asks Grafana *not* to claim the object, leaving it editable in the UI.
	disableProvenanceValue = "true"
)

// CreateProvenanceHeader returns the X-Disable-Provenance header value for *creating* an object,
// which is the one case where there is no stored provenance to mirror and the caller therefore
// chooses: uiEditable leaves the new object editable in the UI (provenance ""), and the default
// claims it for the API (provenance "api"), matching what the read side reports for everything
// grafanapi has written so far.
//
// It applies to creates only. An update always mirrors the stored value (see ProvenanceHeader),
// because changing an object's provenance is a different operation from changing its contents and
// Grafana rejects the attempt outright.
//
// The body is disableProvenanceHeader's, unchanged: this exists to give the create-time case a
// name of its own at the three call sites that have it, so that "the object is being created and
// the user asked for it to stay UI-editable" is what a reader sees rather than a bare bool handed
// to the header renderer - which, on the update path, would mean something else entirely.
func CreateProvenanceHeader(uiEditable bool) *string {
	return disableProvenanceHeader(uiEditable)
}

// GroupProvenanceHeader returns the X-Disable-Provenance header value for a whole rule group,
// whose rules each carry their own provenance while the API offers one header for the group.
//
// The group is therefore writable only when its rules agree: a mixed group is refused, because
// either header would change the provenance of some of its rules, and a group owned by another
// provisioner is refused for the same reason a single object is. An empty group has nothing to
// disagree about and is treated as unclaimed.
//
// The error is unprefixed on purpose - the caller knows which operation and which group it is
// about and wraps it as "alerts: <op> rule group <folderUID>/<title>: %w".
func GroupProvenanceHeader(rules []*models.ProvisionedAlertRule) (*string, error) {
	stored, err := groupProvenance(rules)
	if err != nil {
		return nil, err
	}

	return ProvenanceHeader(stored)
}

// ProvenanceHeader returns the X-Disable-Provenance header value for updating or deleting an
// object whose stored provenance is stored, or an error when that object is managed outside the
// API and must not be written at all.
//
// The mapping is not symmetric and not a preference - it is what Grafana 12.3 accepts:
//
//   - stored "": send the header. Omitting it on a PUT would silently *upgrade* the object to
//     "api" and take it out of the UI's hands.
//   - stored "api": omit the header. Sending it on a PUT is a 500 (rules) or a 403 (policies), and
//     on a DELETE a 409 provenanceMismatch.
//   - anything else: the object belongs to another provisioner, which would overwrite whatever we
//     wrote on its next run, so the write is refused rather than guessed at.
//
// The error is unprefixed on purpose; the caller wraps it with the operation, kind and id, which
// is what turns it into the documented per-item failure.
func ProvenanceHeader(stored string) (*string, error) {
	switch stored {
	case "":
		return disableProvenanceHeader(true), nil
	case apiProvenance:
		return disableProvenanceHeader(false), nil
	default:
		return nil, foreignProvenanceError(stored)
	}
}

// disableProvenanceHeader renders one of the two header forms the generated client takes: a
// pointer to "true" when the write must not claim the object, and nil to omit the header
// otherwise. Both are correct answers - see ProvenanceHeader for why neither can be used
// unconditionally.
func disableProvenanceHeader(disable bool) *string {
	if !disable {
		return nil
	}

	value := disableProvenanceValue

	return &value
}

// foreignProvenanceError is the single wording for "this object belongs to another provisioner",
// shared by the single-object and whole-group paths so the user sees one message.
func foreignProvenanceError(stored string) error {
	return fmt.Errorf("provenance %q is managed outside the API", stored)
}

// groupProvenance reduces a rule group's per-rule provenance values to the single value its group
// write can be sent under, rejecting a group whose rules disagree.
//
// An empty group, and one holding nothing but nil rules, yields "" - it has nothing to disagree
// about, and an unclaimed write is the inert choice for a group with no rules to claim.
func groupProvenance(rules []*models.ProvisionedAlertRule) (string, error) {
	stored, seen := "", false

	for _, rule := range rules {
		if rule == nil {
			continue
		}

		value := string(rule.Provenance)

		if seen && value != stored {
			return "", fmt.Errorf("rules carry mixed provenance (%q and %q)", stored, value)
		}

		stored, seen = value, true
	}

	return stored, nil
}
