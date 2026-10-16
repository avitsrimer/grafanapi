package alerts_test

import (
	"testing"

	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foreignProvenanceMessage is the fragment every refusal shares; it is what tells the user the
// object belongs to another provisioner rather than that the write was malformed.
const foreignProvenanceMessage = "is managed outside the API"

// provenanceRule builds one rule carrying only the field the provenance helpers read.
func provenanceRule(provenance string) *models.ProvisionedAlertRule {
	return &models.ProvisionedAlertRule{Provenance: models.Provenance(provenance)}
}

// TestCreateProvenanceHeader covers the one case with no stored value to mirror: --ui-editable
// asks Grafana not to claim the new object, and the default claims it, which is what the read
// side reports for everything grafanapi has written.
func TestCreateProvenanceHeader(t *testing.T) {
	editable := alerts.CreateProvenanceHeader(true)
	require.NotNil(t, editable, "--ui-editable must send the header so the new object stays UI-editable")
	assert.Equal(t, "true", *editable)

	assert.Nil(t, alerts.CreateProvenanceHeader(false), "the default must omit the header so Grafana stamps api")
}

// TestGroupProvenanceHeader_Uniform pins that a group is written with the mode its rules already
// agree on: omitting the header on an "api" group is what keeps it "api", and sending it on an
// unclaimed group is what stops the write from silently claiming it.
func TestGroupProvenanceHeader_Uniform(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		// header is the expected X-Disable-Provenance value, with "" meaning "the header must be
		// omitted" - the two are different requests, never the same one with an empty value.
		header string
	}{
		{name: "unclaimed group sends the header", stored: "", header: "true"},
		{name: "api group omits the header", stored: "api", header: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header, err := alerts.GroupProvenanceHeader([]*models.ProvisionedAlertRule{
				provenanceRule(tc.stored),
				provenanceRule(tc.stored),
			})
			require.NoError(t, err)
			assertHeader(t, tc.header, header)
		})
	}
}

// TestGroupProvenanceHeader_EmptyGroupIsUnclaimed covers the degenerate group: with no rules
// there is nothing to disagree about, so it is treated as unclaimed rather than refused.
func TestGroupProvenanceHeader_EmptyGroupIsUnclaimed(t *testing.T) {
	header, err := alerts.GroupProvenanceHeader(nil)
	require.NoError(t, err)
	require.NotNil(t, header)
	assert.Equal(t, "true", *header)
}

// TestGroupProvenanceHeader_Foreign pins that a whole group owned by another provisioner is
// refused with the same wording a single object is.
func TestGroupProvenanceHeader_Foreign(t *testing.T) {
	_, err := alerts.GroupProvenanceHeader([]*models.ProvisionedAlertRule{provenanceRule("file")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), foreignProvenanceMessage)
	assert.Contains(t, err.Error(), `"file"`)
}

// TestGroupProvenanceHeader_Mixed is the reason the group needs its own classifier at all: the
// API offers one header for a group whose rules each carry their own provenance, so either header
// would change the provenance of some of them. The refusal names both values it saw.
func TestGroupProvenanceHeader_Mixed(t *testing.T) {
	_, err := alerts.GroupProvenanceHeader([]*models.ProvisionedAlertRule{
		provenanceRule("api"),
		provenanceRule(""),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mixed provenance")
	assert.Contains(t, err.Error(), `"api"`)
}

// TestGroupProvenanceHeader_NilRuleIgnored guards the payload shape: the rules array is a slice
// of pointers, so a `null` element must not decide - or panic - the classification.
func TestGroupProvenanceHeader_NilRuleIgnored(t *testing.T) {
	header, err := alerts.GroupProvenanceHeader([]*models.ProvisionedAlertRule{
		provenanceRule("api"),
		nil,
		provenanceRule("api"),
	})
	require.NoError(t, err)
	assert.Nil(t, header)
}

// TestProvenanceHeader is the three-way classifier table. The open-ended case matters most:
// models.Provenance has no enum, so a provisioner grafanapi has never heard of must be refused
// rather than guessed at - writing it would be undone on that provisioner's next run.
func TestProvenanceHeader(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		// header is the expected X-Disable-Provenance value; "" means the header must be omitted.
		header  string
		refused bool
	}{
		{name: "unclaimed sends the header", stored: "", header: "true"},
		{name: "api omits the header", stored: "api"},
		{name: "file is foreign", stored: "file", refused: true},
		{name: "converted_prometheus is foreign", stored: "converted_prometheus", refused: true},
		{name: "disabled is foreign", stored: "disabled", refused: true},
		{name: "an unknown provisioner is foreign", stored: "some-future-provisioner", refused: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header, err := alerts.ProvenanceHeader(tc.stored)

			if tc.refused {
				require.Error(t, err)
				assert.Contains(t, err.Error(), foreignProvenanceMessage)
				assert.Contains(t, err.Error(), tc.stored, "the refusal must name the value it saw")
				assert.Nil(t, header)

				return
			}

			require.NoError(t, err)
			assertHeader(t, tc.header, header)
		})
	}
}

// assertHeader compares an expected X-Disable-Provenance value against the pointer the provenance
// API returns, where an empty expectation means the header must be omitted entirely.
func assertHeader(t *testing.T, expected string, actual *string) {
	t.Helper()

	if expected == "" {
		assert.Nil(t, actual)

		return
	}

	require.NotNil(t, actual)
	assert.Equal(t, expected, *actual)
}
